package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

func TestSnapshotGenerationResumeFenceAndSeal(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)
	sync, err := s.ConnectorSyncForProject(ctx, projectID, syncID)
	if err != nil {
		t.Fatal(err)
	}
	binding := connector.SourceBinding{ProjectID: projectID, ConnectorID: sync.ConnectorID, Schema: "public", Relation: "users", RelationKind: connector.RelationKindView,
		Columns: []connector.SourcePolicyColumn{{Name: "id", PGType: "text"}}, KeyColumn: "id", KeyStability: connector.KeyStabilityImmutableUnique}
	s.sourcePolicy = &connector.SourcePolicy{Version: 1, Bindings: []connector.SourceBinding{binding}}
	s.sourcePolicyConfigured = true
	if _, err := s.pg.Exec(ctx, `UPDATE connector_syncs SET source_table='public.users',sync_mode='snapshot',cursor_column='' WHERE id=$1`, syncID); err != nil {
		t.Fatal(err)
	}
	run, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "snapshot-1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimConnectorRun(ctx, run.ID, "owner-a")
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	job, err := s.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.ClaimSnapshotGeneration(ctx, job, run.ID, "owner-a", claimed.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	wireRows, err := connector.SnapshotRows([]connector.LandedRow{{Key: "1", DataJSON: `{"id":"1"}`}})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := connector.SnapshotPayloadDigest(wireRows)
	env := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: g.ProjectID, ConnectorID: g.ConnectorID, Table: g.Table, SyncID: g.SyncID, RunID: run.ID,
		Generation: g.Generation, GenerationSeq: g.GenerationSeq, BindingDigest: g.BindingDigest, CaptureStartedAt: g.CaptureStartedAt, Kind: connector.SnapshotKindBatch,
		BatchID: "batch-000000", BatchIndex: 0, PayloadSHA256: digest, Rows: wireRows}
	item, err := s.PrepareSnapshotEnvelope(ctx, g, env, "", "1")
	if err != nil {
		t.Fatal(err)
	}
	if item.LowerKey != "" || item.UpperKey != "1" {
		t.Fatalf("outbox bounds=%q..%q", item.LowerKey, item.UpperKey)
	}
	pending, err := s.PendingSnapshotOutbox(ctx, g)
	if err != nil || len(pending) != 1 || string(pending[0].Payload) != string(item.Payload) {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := s.MarkSnapshotOutboxPublished(ctx, g, item); err != nil {
		t.Fatal(err)
	}
	if err := s.YieldSnapshotGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishConnectorRun(ctx, run.ID, syncID, "owner-a", connector.SyncResult{Rows: 1}, false); err != nil {
		t.Fatal(err)
	}
	run2, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "snapshot-2")
	if err != nil {
		t.Fatal(err)
	}
	claimed2, ok, err := s.ClaimConnectorRun(ctx, run2.ID, "owner-b")
	if err != nil || !ok {
		t.Fatalf("claim2=%+v ok=%v err=%v", claimed2, ok, err)
	}
	job, err = s.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := s.ClaimSnapshotGeneration(ctx, job, run2.ID, "owner-b", claimed2.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if g2.Generation != g.Generation || g2.KeyPosition != "1" || g2.NextBatchIndex != 1 {
		t.Fatalf("resume=%+v", g2)
	}
	entries, expectedRows, err := s.SnapshotManifest(ctx, g.Generation)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := connector.SnapshotManifestDigest(entries)
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	complete := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: g2.ProjectID, ConnectorID: g2.ConnectorID, Table: g2.Table, SyncID: g2.SyncID, RunID: run2.ID,
		Generation: g2.Generation, GenerationSeq: g2.GenerationSeq, BindingDigest: g2.BindingDigest, CaptureStartedAt: g2.CaptureStartedAt, CaptureFinishedAt: &finished, Kind: connector.SnapshotKindComplete,
		ExpectedBatches: int64(len(entries)), ExpectedRows: expectedRows, BatchManifestSHA256: manifest}
	completion, err := s.SealSnapshotGeneration(ctx, g2, complete, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Once sealed, cancellation cannot revoke or rewrite the immutable success
	// path; it returns the still-running receipt while publication finishes.
	if got, err := s.CancelConnectorRun(ctx, projectID, run2.ID); err != nil || got.CancelRequested {
		t.Fatalf("cancel after seal=%+v err=%v", got, err)
	}
	if err := s.MarkSnapshotOutboxPublished(ctx, g2, completion); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishConnectorRun(ctx, run2.ID, syncID, "owner-b", connector.SyncResult{}, false); err != nil {
		t.Fatal(err)
	}
	descriptors, err := s.SnapshotGenerationDescriptors(ctx, projectID, sync.ConnectorID, "public.users")
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 1 || descriptors[0].Resumable || descriptors[0].HasUnpublishedOutbox {
		t.Fatalf("descriptors=%+v", descriptors)
	}
}

func TestStagingCleanupPreservesPerColourAuthority(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)
	sync, err := s.ConnectorSyncForProject(ctx, projectID, syncID)
	if err != nil {
		t.Fatal(err)
	}
	s.duck = openTestDuckDB(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	generation := uuid.NewString()
	if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,terminal_at,run_id,owner,lease_epoch)
VALUES($1,$2,$3,$4,$5,1,$6,'failed',$7,$7,$8,'retention-test',1)`, projectID, sync.ConnectorID, sync.SourceTable, syncID,
		generation, strings.Repeat("a", 64), old, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	green := &Store{pg: s.pg, duck: openTestDuckDB(t)}
	for _, duck := range []*DuckDB{s.duck, green.duck} {
		if err := duck.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_rows
(project_id,connector_id,table_name,generation,batch_id,row_key,data) VALUES(?,?,?,?,?,?,?)`,
				projectID, sync.ConnectorID, sync.SourceTable, generation, "batch-0", "1", "{}")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	if deleted, _, err := s.DeleteEligibleStagingChunk(ctx, generation, cutoff, stagingDeleteChunk); err != nil || deleted != 1 {
		t.Fatalf("blue cleanup = %d err=%v", deleted, err)
	}
	var authorityRows int
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM connector_snapshot_generations WHERE generation=$1`, generation).Scan(&authorityRows); err != nil || authorityRows != 1 {
		t.Fatalf("shared cleanup authority rows = %d err=%v", authorityRows, err)
	}
	candidates, err := green.ListStagingGenerations(ctx, cutoff, 256)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		found = found || candidate.Generation == generation
	}
	if !found {
		t.Fatal("green colour lost shared cleanup authority after blue cleanup")
	}
	if deleted, _, err := green.DeleteEligibleStagingChunk(ctx, generation, cutoff, stagingDeleteChunk); err != nil || deleted != 1 {
		t.Fatalf("green cleanup = %d err=%v", deleted, err)
	}
	for name, colour := range map[string]*Store{"blue": s, "green": green} {
		candidates, err := colour.ListStagingGenerations(ctx, cutoff, 256)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range candidates {
			if candidate.Generation == generation {
				t.Fatalf("%s rediscovered locally completed cleanup", name)
			}
		}
	}
}
