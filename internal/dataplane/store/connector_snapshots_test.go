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

func TestRepairG1FinishCancellationTerminalizesGeneration(t *testing.T) {
	s, ctx, projectID, syncID, run, claimed, g := seedRepairSnapshotGeneration(t, "g1")
	if _, err := s.CancelConnectorRun(ctx, projectID, run.ID); err != nil {
		t.Fatal(err)
	}
	// Reproduce the heartbeat race: durable cancellation is already visible,
	// but the local run context has not observed it yet (cancelled=false).
	if err := s.FinishConnectorRun(ctx, run.ID, syncID, "owner-g1", connector.SyncResult{}, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.SnapshotGeneration(ctx, g.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "cancelled" {
		t.Fatalf("generation state=%q after durable cancellation, want cancelled (run=%s epoch=%d)", got.State, claimed.ID, claimed.LeaseEpoch)
	}
}

func TestRepairR1LocalShutdownKeepsGenerationResumable(t *testing.T) {
	s, ctx, _, syncID, run, _, g := seedRepairSnapshotGeneration(t, "r1-shutdown")
	if err := s.FinishConnectorRun(ctx, run.ID, syncID, "owner-r1-shutdown", connector.SyncResult{Err: "context canceled"}, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.SnapshotGeneration(ctx, g.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "capturing" {
		t.Fatalf("local shutdown changed generation to %s without durable cancellation", got.State)
	}
}

func TestRepairR2CancelledCrashedRunCannotResumeGeneration(t *testing.T) {
	s, ctx, projectID, syncID, run, _, g := seedRepairSnapshotGeneration(t, "r2-crash")
	if _, err := s.CancelConnectorRun(ctx, projectID, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pg.Exec(ctx, `UPDATE connector_runs SET heartbeat_at=now()-interval '3 minutes' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileConnectorRuns(ctx, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	next, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "after-cancelled-crash")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimConnectorRun(ctx, next.ID, "owner-r2-next")
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	job, err := s.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ClaimSnapshotGeneration(ctx, job, next.ID, "owner-r2-next", claimed.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Generation == g.Generation {
		t.Fatalf("durably cancelled generation resumed after crash: %s", g.Generation)
	}
}

func TestSnapshotClaimDoesNotResumeArchiveCancelledGeneration(t *testing.T) {
	s, ctx, projectID, syncID, run, _, generation := seedRepairSnapshotGeneration(t, "archive-crash")
	dc, err := s.DataConnectorForProject(ctx, projectID, generation.ConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := s.ArchiveDataConnectorIdempotent(ctx, projectID, generation.ConnectorID, dc.Revision, "archive-crash", "archive-crash")
	if err != nil {
		t.Fatal(err)
	}
	cancelledRun, err := s.ConnectorRunForProject(ctx, projectID, run.ID)
	if err != nil || !cancelledRun.CancelRequested {
		t.Fatalf("archive cancellation is not durable: run=%+v err=%v", cancelledRun, err)
	}
	// Simulate the worker disappearing before it observes cancel_requested and
	// calls FinishConnectorRun, then restore the connector for a later run.
	if _, err := s.pg.Exec(ctx, `UPDATE connector_runs SET heartbeat_at=now()-interval '3 minutes' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileConnectorRuns(ctx, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UnarchiveDataConnectorIdempotent(ctx, projectID, generation.ConnectorID, archived.Revision, "unarchive-crash", "unarchive-crash"); err != nil {
		t.Fatal(err)
	}
	nextRun, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "after-archive-crash")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimConnectorRun(ctx, nextRun.ID, "owner-archive-next")
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	job, err := s.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		t.Fatal(err)
	}
	nextGeneration, err := s.ClaimSnapshotGeneration(ctx, job, nextRun.ID, "owner-archive-next", claimed.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if nextGeneration.Generation == generation.Generation {
		t.Fatalf("archive-cancelled generation resumed after crash: %s", generation.Generation)
	}
	retired, err := s.SnapshotGeneration(ctx, generation.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != "cancelled" {
		t.Fatalf("archive-cancelled generation state=%q, want cancelled", retired.State)
	}
}

func TestRepairG5ExpiredOwnerCannotFailGeneration(t *testing.T) {
	s, ctx, _, _, run, _, g := seedRepairSnapshotGeneration(t, "g5")
	if _, err := s.pg.Exec(ctx, `UPDATE connector_runs SET heartbeat_at=now()-interval '3 minutes' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FailSnapshotGeneration(ctx, g); err == nil {
		t.Fatal("expired owner changed generation to failed")
	}
	got, err := s.SnapshotGeneration(ctx, g.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "capturing" {
		t.Fatalf("generation state=%q, want capturing", got.State)
	}
}

func seedRepairSnapshotGeneration(t *testing.T, suffix string) (*Store, context.Context, string, string, connector.Run, connector.Run, connector.SnapshotGeneration) {
	t.Helper()
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
	run, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "repair-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	owner := "owner-" + suffix
	claimed, ok, err := s.ClaimConnectorRun(ctx, run.ID, owner)
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	job, err := s.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.ClaimSnapshotGeneration(ctx, job, run.ID, owner, claimed.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, projectID, syncID, run, claimed, g
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

func TestStagingCleanupRetiresSupersededSealedPayload(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)
	sync, err := s.ConnectorSyncForProject(ctx, projectID, syncID)
	if err != nil {
		t.Fatal(err)
	}
	s.duck = openTestDuckDB(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	oldGeneration, activeGeneration := uuid.NewString(), uuid.NewString()
	if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,capture_finished_at,terminal_at,run_id,owner,lease_epoch)
VALUES($1,$2,$3,$4,$5,1,$6,'sealed',$7,$7,$7,$8,'retention-test',1)`, projectID, sync.ConnectorID, sync.SourceTable,
		syncID, oldGeneration, strings.Repeat("a", 64), old, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_outbox
(project_id,connector_id,table_name,generation,batch_id,batch_index,kind,payload,published,published_at)
VALUES($1,$2,$3,$4,'old-batch',0,'batch',$5,true,now())`, projectID, sync.ConnectorID, sync.SourceTable,
		oldGeneration, []byte("published-payload")); err != nil {
		t.Fatal(err)
	}
	if err := s.duck.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_rows
(project_id,connector_id,table_name,generation,batch_id,row_key,data) VALUES(?,?,?,?,?,?,?)`,
			projectID, sync.ConnectorID, sync.SourceTable, oldGeneration, "old-batch", "1", `{}`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_promotions
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,expected_batches,expected_rows,batch_manifest_sha256,capture_started_at,capture_finished_at,promoted_at)
VALUES(?,?,?,?,?,2,?,0,0,?,?,?,?)`, projectID, sync.ConnectorID, sync.SourceTable, syncID, activeGeneration,
			strings.Repeat("b", 64), strings.Repeat("c", 64), old.Add(time.Hour), old.Add(2*time.Hour), old.Add(2*time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	candidates, err := s.ListStagingGenerations(ctx, cutoff, 256)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		found = found || candidate.Generation == oldGeneration
	}
	if !found {
		t.Fatal("superseded sealed generation was not discovered")
	}
	deleted, eligible, err := s.DeleteEligibleStagingChunk(ctx, oldGeneration, cutoff, stagingDeleteChunk)
	if err != nil || !eligible || deleted != 1 {
		t.Fatalf("sealed cleanup deleted=%d eligible=%v err=%v", deleted, eligible, err)
	}
	var outbox int
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM connector_snapshot_outbox WHERE generation=$1`, oldGeneration).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("sealed cleanup retained outbox=%d err=%v", outbox, err)
	}
}

func TestStagingCleanupPreservesSealedGenerationUntilNewerLocalPromotion(t *testing.T) {
	for _, tc := range []struct {
		name            string
		seedOlderActive bool
	}{
		{name: "no_local_promotion"},
		{name: "only_older_local_promotion", seedOlderActive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openConvTestStore(t)
			ctx := context.Background()
			projectID, syncID := seedConnectorSync(t, s)
			sync, err := s.ConnectorSyncForProject(ctx, projectID, syncID)
			if err != nil {
				t.Fatal(err)
			}
			s.duck = openTestDuckDB(t)
			started := time.Now().UTC().Add(-10 * 24 * time.Hour)
			finished := started.Add(time.Hour)
			generation, runID := uuid.NewString(), uuid.NewString()
			bindingDigest := strings.Repeat("a", 64)
			if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,capture_finished_at,terminal_at,run_id,owner,lease_epoch)
VALUES($1,$2,$3,$4,$5,2,$6,'sealed',$7,$8,$8,$9,'retention-test',1)`, projectID, sync.ConnectorID, sync.SourceTable,
				syncID, generation, bindingDigest, started, finished, runID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_outbox
(project_id,connector_id,table_name,generation,batch_id,batch_index,kind,payload,published,published_at)
VALUES($1,$2,$3,$4,'batch-0',0,'batch',$5,true,now())`, projectID, sync.ConnectorID, sync.SourceTable,
				generation, []byte("published-payload")); err != nil {
				t.Fatal(err)
			}

			rows := []connector.SnapshotRow{{Key: "late", Data: []byte(`{"n":1}`)}}
			payloadDigest, err := connector.SnapshotPayloadDigest(rows)
			if err != nil {
				t.Fatal(err)
			}
			manifestDigest, err := connector.SnapshotManifestDigest([]connector.SnapshotManifestEntry{{
				Index: 0, BatchID: "batch-0", PayloadSHA256: payloadDigest, RowCount: 1,
			}})
			if err != nil {
				t.Fatal(err)
			}
			batch := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: sync.ConnectorID,
				Table: sync.SourceTable, SyncID: syncID, RunID: runID, Generation: generation, GenerationSeq: 2,
				BindingDigest: bindingDigest, CaptureStartedAt: started, Kind: connector.SnapshotKindBatch,
				BatchID: "batch-0", BatchIndex: 0, PayloadSHA256: payloadDigest, Rows: rows}
			if promotion, err := s.ApplySnapshotEnvelope(ctx, batch, AppliedMark{}); err != nil || promotion != nil {
				t.Fatalf("stage delayed batch promotion=%+v err=%v", promotion, err)
			}
			if tc.seedOlderActive {
				if err := s.duck.Write(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_promotions
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,expected_batches,expected_rows,batch_manifest_sha256,capture_started_at,capture_finished_at,promoted_at)
VALUES(?,?,?,?,?,1,?,0,0,?,?,?,?)`, projectID, sync.ConnectorID, sync.SourceTable, syncID, uuid.NewString(),
						strings.Repeat("b", 64), strings.Repeat("c", 64), started.Add(-2*time.Hour), started.Add(-time.Hour), started.Add(-time.Hour))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
			candidates, err := s.ListStagingGenerations(ctx, cutoff, 256)
			if err != nil {
				t.Fatal(err)
			}
			var candidate *StagingGenerationDescriptor
			for i := range candidates {
				if candidates[i].Generation == generation {
					candidate = &candidates[i]
					break
				}
			}
			if candidate == nil {
				t.Fatal("sealed generation was not discovered")
			}
			if candidate.IsSupersededOnThisStore || EligibleForStagingCleanup(*candidate, cutoff) {
				t.Fatalf("non-superseded sealed generation became eligible: %+v", *candidate)
			}
			if deleted, eligible, err := s.DeleteEligibleStagingChunk(ctx, generation, cutoff, stagingDeleteChunk); err != nil || deleted != 0 || eligible {
				t.Fatalf("sealed cleanup deleted=%d eligible=%v err=%v", deleted, eligible, err)
			}

			complete := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: sync.ConnectorID,
				Table: sync.SourceTable, SyncID: syncID, RunID: runID, Generation: generation, GenerationSeq: 2,
				BindingDigest: bindingDigest, CaptureStartedAt: started, CaptureFinishedAt: &finished, Kind: connector.SnapshotKindComplete,
				ExpectedBatches: 1, ExpectedRows: 1, BatchManifestSHA256: manifestDigest}
			promotion, err := s.ApplySnapshotEnvelope(ctx, complete, AppliedMark{})
			if err != nil || promotion == nil || promotion.Generation != generation {
				t.Fatalf("delayed completion promotion=%+v err=%v", promotion, err)
			}
			if got := snapshotLiveCount(t, s.duck, projectID, sync.ConnectorID, sync.SourceTable); got != 1 {
				t.Fatalf("promoted rows=%d, want 1", got)
			}
			var receiptGeneration string
			var completionSeen bool
			if err := s.duck.Read(ctx, func(conn *sql.Conn) error {
				return conn.QueryRowContext(ctx, `SELECT generation::VARCHAR,completion_seen FROM data_receipt_sources
WHERE project_id=? AND connector_id=? AND table_name=?`, projectID, sync.ConnectorID, sync.SourceTable).
					Scan(&receiptGeneration, &completionSeen)
			}); err != nil {
				t.Fatal(err)
			}
			if receiptGeneration != generation || !completionSeen {
				t.Fatalf("readiness receipt generation=%q complete=%v", receiptGeneration, completionSeen)
			}
		})
	}
}

func TestStagingRetentionPaginatesPastProtectedSealedGenerations(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)
	sync, err := s.ConnectorSyncForProject(ctx, projectID, syncID)
	if err != nil {
		t.Fatal(err)
	}
	s.duck = openTestDuckDB(t)
	t.Cleanup(func() {
		if _, err := s.pg.Exec(ctx, `DELETE FROM connector_snapshot_generations WHERE project_id=$1`, projectID); err != nil {
			t.Error(err)
		}
	})
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,capture_finished_at,terminal_at,run_id,owner,lease_epoch,sync_revision,source_revision)
SELECT $1,$2,$3,$4,gen_random_uuid(),i,$5,'sealed',$6,$6,$6,gen_random_uuid(),'retention-pagination',1,1,1
FROM generate_series(1,256) AS series(i)`, projectID, sync.ConnectorID, sync.SourceTable, syncID, strings.Repeat("a", 64), old); err != nil {
		t.Fatal(err)
	}
	target := uuid.NewString()
	if _, err := s.pg.Exec(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,terminal_at,run_id,owner,lease_epoch,sync_revision,source_revision)
VALUES($1,$2,$3,$4,$5,257,$6,'failed',$7,$7,$8,'retention-pagination',1,1,1)`,
		projectID, sync.ConnectorID, sync.SourceTable, syncID, target, strings.Repeat("a", 64), old.Add(time.Hour), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if err := s.duck.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_rows
(project_id,connector_id,table_name,generation,batch_id,row_key,data) VALUES(?,?,?,?,?,?,?)`,
			projectID, sync.ConnectorID, sync.SourceTable, target, "batch-0", "1", "{}")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := NewStagingRetention(s, 7*24*time.Hour)
	r.sweep(ctx)
	var rows int
	if err := s.duck.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM connector_snapshot_rows WHERE generation=?`, target).Scan(&rows)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("eligible candidate after protected page retained %d staging rows", rows)
	}
}
