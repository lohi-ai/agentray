package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

type replayReadinessFixture struct {
	t           *testing.T
	store       *Store
	projectID   string
	connectorID string
	syncID      string
	table       string
	now         time.Time
	duckPath    string
}

type replaySnapshot struct {
	batches  []connector.SnapshotEnvelope
	complete connector.SnapshotEnvelope
}

func newReplayReadinessFixture(t *testing.T) *replayReadinessFixture {
	t.Helper()
	s := openConvTestStore(t)
	_, projectID := seedConvProject(t, s)
	ctx := context.Background()
	var connectorID, syncID string
	if err := s.pg.QueryRow(ctx, `INSERT INTO data_connectors(project_id,name,kind) VALUES($1,'replay-state','postgres') RETURNING id::text`, projectID).Scan(&connectorID); err != nil {
		t.Fatal(err)
	}
	if err := s.pg.QueryRow(ctx, `INSERT INTO connector_syncs(connector_id,project_id,source_table,key_column) VALUES($1,$2,'orders','id') RETURNING id::text`, connectorID, projectID).Scan(&syncID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replay-state.duckdb")
	d, err := OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.duck = d
	s.sandboxes = newSQLSandboxPool(d)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	f := &replayReadinessFixture{t: t, store: s, projectID: projectID, connectorID: connectorID, syncID: syncID, table: "orders", now: now, duckPath: path}
	t.Cleanup(func() {
		s.sandboxes.closeAll()
		_ = s.duck.Close()
	})
	return f
}

func (f *replayReadinessFixture) restart() {
	f.t.Helper()
	f.store.sandboxes.closeAll()
	if err := f.store.duck.Close(); err != nil {
		f.t.Fatal(err)
	}
	d, err := OpenDuckDB(context.Background(), f.duckPath)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store.duck = d
	f.store.sandboxes = newSQLSandboxPool(d)
}

func (f *replayReadinessFixture) snapshot(seq int64) replaySnapshot {
	f.t.Helper()
	started := f.now.Add(time.Duration(seq) * time.Minute)
	finished := started.Add(time.Second)
	rows := [][]connector.SnapshotRow{
		{{Key: "one", Data: json.RawMessage(`{"n":1}`)}},
		{{Key: "two", Data: json.RawMessage(`{"n":2}`)}},
	}
	batches := make([]connector.SnapshotEnvelope, 0, len(rows))
	manifest := make([]connector.SnapshotManifestEntry, 0, len(rows))
	generation, runID := uuid.NewString(), uuid.NewString()
	for i, batchRows := range rows {
		digest, err := connector.SnapshotPayloadDigest(batchRows)
		if err != nil {
			f.t.Fatal(err)
		}
		batchID := fmt.Sprintf("batch-%d", i)
		batches = append(batches, connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1,
			ProjectID: f.projectID, ConnectorID: f.connectorID, Table: f.table, SyncID: f.syncID,
			Generation: generation, GenerationSeq: seq, BindingDigest: replayDigest('b'), CaptureStartedAt: started,
			Kind: connector.SnapshotKindBatch, RunID: runID, BatchID: batchID, BatchIndex: int64(i), PayloadSHA256: digest, Rows: batchRows})
		manifest = append(manifest, connector.SnapshotManifestEntry{Index: int64(i), BatchID: batchID, PayloadSHA256: digest, RowCount: len(batchRows)})
	}
	manifestDigest, err := connector.SnapshotManifestDigest(manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	complete := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1,
		ProjectID: f.projectID, ConnectorID: f.connectorID, Table: f.table, SyncID: f.syncID,
		Generation: generation, GenerationSeq: seq, BindingDigest: replayDigest('b'), CaptureStartedAt: started,
		Kind: connector.SnapshotKindComplete, RunID: runID, CaptureFinishedAt: &finished,
		ExpectedBatches: int64(len(rows)), ExpectedRows: int64(len(rows)), BatchManifestSHA256: manifestDigest}
	return replaySnapshot{batches: batches, complete: complete}
}

func replayDigest(ch byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = ch
	}
	return string(b)
}

func (f *replayReadinessFixture) apply(env connector.SnapshotEnvelope) {
	f.t.Helper()
	if _, err := f.store.ApplySnapshotEnvelope(context.Background(), env, AppliedMark{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *replayReadinessFixture) promote(snapshot replaySnapshot) {
	f.t.Helper()
	for _, batch := range snapshot.batches {
		f.apply(batch)
	}
	f.apply(snapshot.complete)
}

func (f *replayReadinessFixture) query() {
	f.t.Helper()
	if _, _, err := f.store.RunSQLWithMeta(context.Background(), f.projectID, `SELECT count(*) FROM external_rows`); err != nil {
		f.t.Fatal(err)
	}
}

func (f *replayReadinessFixture) readiness() *Readiness {
	f.t.Helper()
	got, err := f.store.SourceReadiness(context.Background(), f.projectID, []ReadinessSource{{
		SyncID: f.syncID, ConnectorID: f.connectorID, Table: f.table, ScheduleCron: "* * * * *", Configured: true,
	}})
	if err != nil {
		f.t.Fatal(err)
	}
	return got[f.syncID]
}

func (f *replayReadinessFixture) assertReadiness(state, reason string) {
	f.t.Helper()
	got := f.readiness()
	gotReason := ""
	if got.Reason != nil {
		gotReason = *got.Reason
	}
	if got.State != state || gotReason != reason {
		f.t.Fatalf("readiness=%s/%s want %s/%s", got.State, gotReason, state, reason)
	}
}

func (f *replayReadinessFixture) liveRows() int {
	f.t.Helper()
	var count int
	if err := f.store.duck.Read(context.Background(), func(conn *sql.Conn) error {
		return conn.QueryRowContext(context.Background(), `SELECT count(*) FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=?`, f.projectID, f.connectorID, f.table).Scan(&count)
	}); err != nil {
		f.t.Fatal(err)
	}
	return count
}

func (f *replayReadinessFixture) seedAmbiguity(active replaySnapshot) {
	f.t.Helper()
	f.promote(active)
	f.query()
	f.assertReadiness(ReadinessReady, "")
	oldAt := active.complete.CaptureStartedAt.Add(-time.Minute)
	index, expected := uint64(0), uint64(1)
	old := SourceReceiptMark{ProjectID: f.projectID, ConnectorID: f.connectorID, Table: f.table, SyncID: f.syncID,
		RunID: uuid.NewString(), BatchID: "old-batch", BatchIndex: &index, PayloadSHA256: replayDigest('d'), CaptureStartedAt: &oldAt, Promoted: true}
	if err := f.store.duck.InsertExternalRows(context.Background(), f.projectID, f.connectorID, f.table,
		[]connector.LandedRow{{Key: "old-only", DataJSON: `{"n":9}`}}, AppliedMark{Source: &old}); err != nil {
		f.t.Fatal(err)
	}
	complete := old
	complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
	complete.ExpectedBatches, complete.CaptureFinishedAt, complete.Complete = &expected, &oldAt, true
	if err := f.store.duck.RecordPosition(context.Background(), AppliedMark{Source: &complete}); err != nil {
		f.t.Fatal(err)
	}
	f.assertReadiness(ReadinessIncomplete, "incremental_ordering_ambiguous")
}

func TestSnapshotReplayStateMachineBeforePromotion(t *testing.T) {
	cases := map[string]func(*replayReadinessFixture, replaySnapshot){
		"batch-replay": func(f *replayReadinessFixture, next replaySnapshot) {
			f.apply(next.batches[0])
			f.apply(next.batches[0])
		},
		"completion-replay": func(f *replayReadinessFixture, next replaySnapshot) {
			f.apply(next.complete)
			f.apply(next.complete)
		},
		"both": func(f *replayReadinessFixture, next replaySnapshot) {
			f.apply(next.batches[0])
			f.apply(next.complete)
			f.apply(next.batches[0])
			f.apply(next.complete)
		},
		"out-of-order": func(f *replayReadinessFixture, next replaySnapshot) {
			f.apply(next.complete)
			f.apply(next.batches[0])
			f.apply(next.complete)
		},
		"duplicate": func(f *replayReadinessFixture, next replaySnapshot) {
			for range 3 {
				f.apply(next.batches[0])
				f.apply(next.complete)
			}
		},
		"restart-between": func(f *replayReadinessFixture, next replaySnapshot) {
			f.apply(next.batches[0])
			f.apply(next.complete)
			f.restart()
			f.apply(next.batches[0])
			f.apply(next.complete)
		},
	}
	for name, replay := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReplayReadinessFixture(t)
			f.seedAmbiguity(f.snapshot(1))
			if got := f.liveRows(); got != 3 {
				t.Fatalf("ambiguous rows=%d want 3", got)
			}
			next := f.snapshot(2)
			replay(f, next)
			f.assertReadiness(ReadinessIncomplete, "incremental_ordering_ambiguous")
			if got := f.liveRows(); got != 3 {
				t.Fatalf("pre-promotion replay changed rows=%d want 3", got)
			}
			// Supplying the missing manifest member performs a real atomic
			// replacement. Only that generation transition may clear ambiguity.
			for _, batch := range next.batches {
				f.apply(batch)
			}
			f.apply(next.complete)
			f.query()
			f.assertReadiness(ReadinessReady, "")
			if got := f.liveRows(); got != 2 {
				t.Fatalf("promoted rows=%d want 2", got)
			}
		})
	}
}

func TestSnapshotReplayStateMachineAfterPromotion(t *testing.T) {
	cases := map[string]func(*replayReadinessFixture, replaySnapshot){
		"batch-replay": func(f *replayReadinessFixture, active replaySnapshot) {
			f.apply(active.batches[0])
		},
		"completion-replay": func(f *replayReadinessFixture, active replaySnapshot) {
			f.apply(active.complete)
		},
		"both": func(f *replayReadinessFixture, active replaySnapshot) {
			f.apply(active.batches[0])
			f.apply(active.complete)
		},
		"out-of-order": func(f *replayReadinessFixture, active replaySnapshot) {
			f.apply(active.complete)
			f.apply(active.batches[1])
			f.apply(active.batches[0])
		},
		"duplicate": func(f *replayReadinessFixture, active replaySnapshot) {
			for range 3 {
				f.apply(active.batches[0])
				f.apply(active.complete)
			}
		},
		"restart-between": func(f *replayReadinessFixture, active replaySnapshot) {
			f.apply(active.batches[0])
			f.restart()
			f.apply(active.complete)
			// Query confirmation is process-local by design, so prove the reopened
			// serving file before comparing its durable classification.
			f.query()
		},
	}
	for name, replay := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReplayReadinessFixture(t)
			active := f.snapshot(1)
			f.promote(active)
			f.query()
			f.assertReadiness(ReadinessReady, "")

			// Positive direction: replays of an honest active snapshot are true
			// no-ops for both classification and row cardinality.
			replay(f, active)
			f.assertReadiness(ReadinessReady, "")
			if got := f.liveRows(); got != 2 {
				t.Fatalf("ready replay rows=%d want 2", got)
			}

			// Negative direction: once an older incremental delivery makes row
			// ownership ambiguous, no active-generation replay may launder it.
			f.seedAmbiguity(active)
			before := f.liveRows()
			replay(f, active)
			f.query()
			f.assertReadiness(ReadinessIncomplete, "incremental_ordering_ambiguous")
			if got := f.liveRows(); got != before || got != 3 {
				t.Fatalf("ambiguous replay rows=%d before=%d want 3", got, before)
			}

			// A different, fully sequenced generation really replaces the rows
			// and is the sole transition back to ready.
			f.promote(f.snapshot(2))
			f.query()
			f.assertReadiness(ReadinessReady, "")
			if got := f.liveRows(); got != 2 {
				t.Fatalf("replacement rows=%d want 2", got)
			}
		})
	}
}
