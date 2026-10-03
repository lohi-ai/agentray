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
	// Public/display metadata revisions are unrelated to the immutable source
	// contract. They must not make an otherwise-current snapshot job stale.
	if _, err := s.pg.Exec(ctx, `UPDATE data_connectors SET name=name || ' renamed',revision=revision+1 WHERE id=$1`, sync.ConnectorID); err != nil {
		t.Fatal(err)
	}
	g, err := s.ClaimSnapshotGeneration(ctx, job, run.ID, "owner-a", claimed.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if g.SyncRevision != job.SyncRevision || g.SourceRevision != job.SourceRevision || g.SyncRevision == 0 || g.SourceRevision == 0 {
		t.Fatalf("generation did not persist config identity: generation=%+v job=%+v", g, job)
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var connectorRevision int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM data_connectors WHERE id=$1`, sync.ConnectorID).Scan(&connectorRevision); err != nil {
		t.Fatal(err)
	}
	replacementCredential := "00000000-0000-4000-8000-000000000099"
	if _, err := updateDataConnectorRevision(ctx, tx, projectID, sync.ConnectorID, nil, &replacementCredential, connectorRevision); err == nil || !strings.Contains(err.Error(), "snapshot generation is active") {
		t.Fatalf("credential change did not fence active generation: %v", err)
	}
	tx.Rollback(ctx)
	var userID string
	if err := s.pg.QueryRow(ctx, `SELECT owner_id::text FROM projects WHERE id=$1`, projectID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	mode := "snapshot"
	if _, err := s.UpdateConnectorSync(ctx, userID, projectID, syncID, ConnectorSyncInput{SourceTable: "public.other", KeyColumn: "id", SyncMode: &mode, Enabled: true}); err == nil || !strings.Contains(err.Error(), "snapshot generation is active") {
		t.Fatalf("sync reconfiguration did not fence active generation: %v", err)
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
	// Once sealed, the originating run is already succeeded atomically;
	// cancellation cannot revoke or rewrite the immutable success path.
	if got, err := s.CancelConnectorRun(ctx, projectID, run2.ID); err != nil || got.CancelRequested {
		t.Fatalf("cancel after seal=%+v err=%v", got, err)
	}
	if got, err := s.ConnectorRunForProject(ctx, projectID, run2.ID); err != nil || got.Status != "succeeded" {
		t.Fatalf("sealed run was not finalized atomically: %+v err=%v", got, err)
	}
	// Simulate losing the completion publish acknowledgement after the atomic
	// seal. A later run must claim the sealed generation and drain the exact
	// immutable completion instead of opening a replacement generation.
	run3, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "snapshot-drain")
	if err != nil {
		t.Fatal(err)
	}
	claimed3, ok, err := s.ClaimConnectorRun(ctx, run3.ID, "owner-c")
	if err != nil || !ok {
		t.Fatalf("claim3=%+v ok=%v err=%v", claimed3, ok, err)
	}
	job, err = s.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		t.Fatal(err)
	}
	g3, err := s.ClaimSnapshotGeneration(ctx, job, run3.ID, "owner-c", claimed3.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if g3.Generation != g.Generation || g3.State != "sealed" {
		t.Fatalf("sealed drain claimed wrong generation: %+v", g3)
	}
	pending, err = s.PendingSnapshotOutbox(ctx, g3)
	if err != nil || len(pending) != 1 || pending[0].ID != completion.ID {
		t.Fatalf("sealed completion pending=%+v err=%v", pending, err)
	}
	if err := s.MarkSnapshotOutboxPublished(ctx, g3, pending[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishConnectorRun(ctx, run3.ID, syncID, "owner-c", connector.SyncResult{}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnectorSync(ctx, userID, projectID, syncID); err == nil || !strings.Contains(err.Error(), "history protects") {
		t.Fatalf("snapshot history was deleted with its sync: %v", err)
	}
	descriptors, err := s.SnapshotGenerationDescriptors(ctx, projectID, sync.ConnectorID, "public.users")
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 1 || descriptors[0].Resumable || descriptors[0].HasUnpublishedOutbox {
		t.Fatalf("descriptors=%+v", descriptors)
	}
}

func TestSnapshotClaimRejectsStaleSyncAndSourceRevisions(t *testing.T) {
	for _, mutate := range []string{"sync", "source"} {
		t.Run(mutate, func(t *testing.T) {
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
			job, err := s.ConnectorSyncJob(ctx, syncID)
			if err != nil {
				t.Fatal(err)
			}
			if mutate == "sync" {
				_, err = s.pg.Exec(ctx, `UPDATE connector_syncs SET config_revision=config_revision+1 WHERE id=$1`, syncID)
			} else {
				_, err = s.pg.Exec(ctx, `UPDATE data_connectors SET source_config_revision=source_config_revision+1 WHERE id=$1`, sync.ConnectorID)
			}
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "stale-"+mutate)
			if err != nil {
				t.Fatal(err)
			}
			claimed, ok, err := s.ClaimConnectorRun(ctx, run.ID, "owner-stale")
			if err != nil || !ok {
				t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
			}
			if _, err := s.ClaimSnapshotGeneration(ctx, job, run.ID, "owner-stale", claimed.LeaseEpoch); err == nil || !strings.Contains(err.Error(), "configuration is stale") {
				t.Fatalf("stale %s job claimed a generation: %v", mutate, err)
			}
		})
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
	runID := uuid.NewString()
	if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,terminal_at,run_id,owner,lease_epoch)
VALUES($1,$2,$3,$4,$5,1,$6,'failed',$7,$7,$8,'retention-test',1)`, projectID, sync.ConnectorID, sync.SourceTable, syncID,
		generation, strings.Repeat("a", 64), old, runID); err != nil {
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
	rows := []connector.SnapshotRow{{Key: "late", Data: []byte(`{"n":1}`)}}
	digest, err := connector.SnapshotPayloadDigest(rows)
	if err != nil {
		t.Fatal(err)
	}
	late := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: sync.ConnectorID,
		Table: sync.SourceTable, SyncID: syncID, Generation: generation, GenerationSeq: 1, BindingDigest: strings.Repeat("a", 64),
		CaptureStartedAt: old, Kind: connector.SnapshotKindBatch, RunID: runID, BatchID: "late-batch", PayloadSHA256: digest, Rows: rows}
	if _, err := s.ApplySnapshotEnvelope(ctx, late, AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	var blueRows int
	if err := s.duck.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM connector_snapshot_rows WHERE generation=?`, generation).Scan(&blueRows)
	}); err != nil || blueRows != 0 {
		t.Fatalf("blue tombstone retained %d late rows err=%v", blueRows, err)
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
	if _, err := green.ApplySnapshotEnvelope(ctx, late, AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	var greenRows int
	if err := green.duck.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM connector_snapshot_rows WHERE generation=?`, generation).Scan(&greenRows)
	}); err != nil || greenRows != 0 {
		t.Fatalf("green tombstone retained %d late rows err=%v", greenRows, err)
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
