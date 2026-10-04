package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

func TestSnapshotAtomicPromotionAndReplay(t *testing.T) {
	ctx := context.Background()
	d, err := OpenDuckDB(ctx, filepath.Join(t.TempDir(), "snapshot.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	raw, err := os.ReadFile(filepath.Join("..", "ingest", "testdata", "c1-wire-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Legacy struct {
			ProjectID, ConnectorID, Table string
			Rows                          []struct {
				Key, Cursor string
				Data        json.RawMessage
			}
		} `json:"legacy_batch"`
		Batches  []connector.SnapshotEnvelope `json:"batches"`
		Complete connector.SnapshotEnvelope   `json:"complete"`
		Empty    connector.SnapshotEnvelope   `json:"empty_complete"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	legacyRows := make([]connector.LandedRow, 0, len(f.Legacy.Rows))
	for _, r := range f.Legacy.Rows {
		legacyRows = append(legacyRows, connector.LandedRow{Key: r.Key, Cursor: r.Cursor, DataJSON: string(r.Data)})
	}
	// Seed the same binding with old live data. Staged partial generations must
	// leave it untouched.
	if err := d.InsertExternalRows(ctx, f.Complete.ProjectID, f.Complete.ConnectorID, f.Complete.Table, legacyRows, AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Batches[0], AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Complete, AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	if got := snapshotLiveCount(t, d, f.Complete.ProjectID, f.Complete.ConnectorID, f.Complete.Table); got != 1 {
		t.Fatalf("partial generation exposed: %d", got)
	}
	promotion, err := d.ApplySnapshotEnvelope(ctx, f.Batches[1], AppliedMark{Durable: "test", Seq: 3})
	if err != nil {
		t.Fatal(err)
	}
	if promotion == nil || promotion.Generation != f.Complete.Generation {
		t.Fatalf("promotion=%+v", promotion)
	}
	var receiptGeneration string
	var completionSeen bool
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT generation::VARCHAR,completion_seen FROM data_receipt_sources
WHERE project_id=? AND connector_id=? AND table_name=?`, f.Complete.ProjectID, f.Complete.ConnectorID, f.Complete.Table).
			Scan(&receiptGeneration, &completionSeen)
	}); err != nil {
		t.Fatal(err)
	}
	if receiptGeneration != f.Complete.Generation || !completionSeen {
		t.Fatalf("snapshot promotion receipt = generation %q complete=%v", receiptGeneration, completionSeen)
	}
	if got := snapshotLiveCount(t, d, f.Complete.ProjectID, f.Complete.ConnectorID, f.Complete.Table); got != 2 {
		t.Fatalf("promoted rows=%d", got)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Batches[0], AppliedMark{Durable: "test", Seq: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Complete, AppliedMark{Durable: "test", Seq: 5}); err != nil {
		t.Fatal(err)
	}
	if got := snapshotLiveCount(t, d, f.Complete.ProjectID, f.Complete.ConnectorID, f.Complete.Table); got != 2 {
		t.Fatalf("replay changed rows=%d", got)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Empty, AppliedMark{Durable: "test", Seq: 6}); err != nil {
		t.Fatal(err)
	}
	if got := snapshotLiveCount(t, d, f.Complete.ProjectID, f.Complete.ConnectorID, f.Complete.Table); got != 0 {
		t.Fatalf("empty snapshot rows=%d", got)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Complete, AppliedMark{Durable: "test", Seq: 7}); err != nil {
		t.Fatal(err)
	}
	if got := snapshotLiveCount(t, d, f.Complete.ProjectID, f.Complete.ConnectorID, f.Complete.Table); got != 0 {
		t.Fatalf("older replay rolled back active generation: %d", got)
	}
}

func TestSnapshotStagingCompactionPreservesActiveGeneration(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)
	raw, err := os.ReadFile(filepath.Join("..", "ingest", "testdata", "c1-wire-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Batches  []connector.SnapshotEnvelope `json:"batches"`
		Complete connector.SnapshotEnvelope   `json:"complete"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, env := range f.Batches {
		if _, err := d.ApplySnapshotEnvelope(ctx, env, AppliedMark{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Complete, AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	descriptor := StagingGenerationDescriptor{ProjectID: f.Complete.ProjectID, ConnectorID: f.Complete.ConnectorID,
		Table: f.Complete.Table, Generation: f.Complete.Generation, GenerationSeq: f.Complete.GenerationSeq, State: "sealed"}
	deleted, _, eligible, err := d.deleteSnapshotStagingChunk(ctx, descriptor, 50_000)
	if err != nil || deleted != 0 {
		t.Fatalf("active generation cleanup = deleted %d err=%v", deleted, err)
	}
	if eligible {
		t.Fatal("active generation remained eligible inside deletion transaction")
	}
	var rows int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM connector_snapshot_rows WHERE generation=?`, f.Complete.Generation).Scan(&rows)
	}); err != nil || rows == 0 {
		t.Fatalf("active staging rows=%d err=%v", rows, err)
	}
}

func TestDelayedSnapshotDoesNotOverwriteNewerIncrementalRows(t *testing.T) {
	for _, tc := range []struct {
		name               string
		delayedFinalBatch  bool
		legitimateNewStart bool
	}{
		{name: "delayed_completion"},
		{name: "delayed_final_batch", delayedFinalBatch: true},
		{name: "later_snapshot_transition", legitimateNewStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDuckDB(t)
			ctx := context.Background()
			projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			base := time.Now().UTC().Add(-time.Hour)
			batch1, complete1 := snapshotEnvelopePair(t, projectID, connectorID, syncID, base.Add(time.Minute), 1, 1)
			for _, env := range []connector.SnapshotEnvelope{batch1, complete1} {
				if _, err := d.ApplySnapshotEnvelope(ctx, env, AppliedMark{}); err != nil {
					t.Fatal(err)
				}
			}

			snapshotStart := base.Add(2 * time.Minute)
			if tc.legitimateNewStart {
				snapshotStart = base.Add(4 * time.Minute)
			}
			batch2, complete2 := snapshotEnvelopePair(t, projectID, connectorID, syncID, snapshotStart, 2, 2)
			if tc.delayedFinalBatch {
				if promotion, err := d.ApplySnapshotEnvelope(ctx, complete2, AppliedMark{}); err != nil || promotion != nil {
					t.Fatalf("stage completion promotion=%+v err=%v", promotion, err)
				}
			} else if !tc.legitimateNewStart {
				if promotion, err := d.ApplySnapshotEnvelope(ctx, batch2, AppliedMark{}); err != nil || promotion != nil {
					t.Fatalf("stage batch promotion=%+v err=%v", promotion, err)
				}
			}

			incrementalAt := base.Add(3 * time.Minute)
			index := uint64(0)
			incremental := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
				RunID: uuid.NewString(), BatchID: "delta", BatchIndex: &index, PayloadSHA256: strings.Repeat("b", 64),
				CaptureStartedAt: &incrementalAt, Promoted: true}
			if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders",
				[]connector.LandedRow{{Key: "same", DataJSON: `{"n":3}`}}, AppliedMark{Source: &incremental}); err != nil {
				t.Fatal(err)
			}

			trigger := complete2
			if tc.delayedFinalBatch || tc.legitimateNewStart {
				trigger = batch2
				if tc.legitimateNewStart {
					if promotion, err := d.ApplySnapshotEnvelope(ctx, complete2, AppliedMark{}); err != nil || promotion != nil {
						t.Fatalf("stage later completion promotion=%+v err=%v", promotion, err)
					}
				}
			}
			promotion, err := d.ApplySnapshotEnvelope(ctx, trigger, AppliedMark{})
			if err != nil {
				t.Fatal(err)
			}
			want := 3
			if tc.legitimateNewStart {
				want = 2
				if promotion == nil {
					t.Fatal("later snapshot was not promoted")
				}
			} else if promotion != nil {
				t.Fatalf("delayed snapshot unexpectedly promoted: %+v", promotion)
			}
			var got int
			if err := d.Read(ctx, func(conn *sql.Conn) error {
				return conn.QueryRowContext(ctx, `SELECT CAST(json_extract(data,'$.n') AS INTEGER) FROM external_rows
WHERE project_id=? AND connector_id=? AND table_name='orders'`, projectID, connectorID).Scan(&got)
			}); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("serving value=%d, want %d", got, want)
			}
		})
	}
}

func snapshotEnvelopePair(t *testing.T, projectID, connectorID, syncID string, started time.Time, sequence int64, value int) (connector.SnapshotEnvelope, connector.SnapshotEnvelope) {
	t.Helper()
	rows := []connector.SnapshotRow{{Key: "same", Data: json.RawMessage(fmt.Sprintf(`{"n":%d}`, value))}}
	payload, err := connector.SnapshotPayloadDigest(rows)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := connector.SnapshotManifestDigest([]connector.SnapshotManifestEntry{{Index: 0, BatchID: "batch", PayloadSHA256: payload, RowCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	finished := started.Add(time.Second)
	batch := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: connectorID,
		Table: "orders", SyncID: syncID, RunID: uuid.NewString(), Generation: uuid.NewString(), GenerationSeq: sequence,
		BindingDigest: strings.Repeat("a", 64), CaptureStartedAt: started, Kind: connector.SnapshotKindBatch,
		BatchID: "batch", PayloadSHA256: payload, Rows: rows}
	complete := batch
	complete.Kind, complete.Rows, complete.BatchID, complete.PayloadSHA256 = connector.SnapshotKindComplete, nil, "", ""
	complete.CaptureFinishedAt, complete.ExpectedBatches, complete.ExpectedRows = &finished, 1, 1
	complete.BatchManifestSHA256 = manifest
	return batch, complete
}

func TestSnapshotCleanupTombstoneFencesLateRedelivery(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)
	raw, err := os.ReadFile(filepath.Join("..", "ingest", "testdata", "c1-wire-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Batches  []connector.SnapshotEnvelope `json:"batches"`
		Complete connector.SnapshotEnvelope   `json:"complete"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Batches[0], AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	sealed := StagingGenerationDescriptor{ProjectID: f.Batches[0].ProjectID, ConnectorID: f.Batches[0].ConnectorID,
		Table: f.Batches[0].Table, Generation: f.Batches[0].Generation, GenerationSeq: f.Batches[0].GenerationSeq, State: "sealed",
		IsSupersededOnThisStore: true}
	deleted, more, eligible, err := d.deleteSnapshotStagingChunk(ctx, sealed, 50_000)
	if err != nil || deleted != 0 || more || eligible {
		t.Fatalf("sealed cleanup without newer local promotion = deleted %d more=%v eligible=%v err=%v", deleted, more, eligible, err)
	}
	descriptor := StagingGenerationDescriptor{Generation: f.Batches[0].Generation, State: "failed"}
	deleted, more, eligible, err = d.deleteSnapshotStagingChunk(ctx, descriptor, 50_000)
	if err != nil || deleted == 0 || more || !eligible {
		t.Fatalf("partial cleanup = deleted %d more=%v eligible=%v err=%v", deleted, more, eligible, err)
	}
	delivery := DeliveryReceiptMark{StreamID: "snapshots@test", Subject: "snapshots", StreamSeq: 9, PayloadSHA256: strings.Repeat("a", 64)}
	for _, env := range []connector.SnapshotEnvelope{f.Batches[0], f.Complete} {
		if promotion, err := d.ApplySnapshotEnvelope(ctx, env, AppliedMark{Durable: "blue", Seq: 9, Deliveries: []DeliveryReceiptMark{delivery}}); err != nil || promotion != nil {
			t.Fatalf("late %s envelope promotion=%+v err=%v", env.Kind, promotion, err)
		}
	}
	var rows, batches, completions, deliveries int
	var position uint64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM connector_snapshot_rows WHERE generation=?),
(SELECT count(*) FROM connector_snapshot_batches WHERE generation=?),
(SELECT count(*) FROM connector_snapshot_completions WHERE generation=?),
(SELECT count(*) FROM data_receipt_deliveries WHERE stream_id='snapshots@test'),
(SELECT applied_seq FROM ingest_position WHERE durable='blue')`,
			f.Complete.Generation, f.Complete.Generation, f.Complete.Generation).Scan(&rows, &batches, &completions, &deliveries, &position)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || batches != 0 || completions != 0 || deliveries != 1 || position != 9 {
		t.Fatalf("late redelivery rows=%d batches=%d completions=%d deliveries=%d position=%d", rows, batches, completions, deliveries, position)
	}
}

func snapshotLiveCount(t *testing.T, d *DuckDB, projectID, connectorID, table string) int {
	t.Helper()
	var n int
	if err := d.Read(context.Background(), func(conn *sql.Conn) error {
		return conn.QueryRowContext(context.Background(), `SELECT count(*) FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=?`, projectID, connectorID, table).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSnapshotConflictingReplayRejected(t *testing.T) {
	ctx := context.Background()
	d, err := OpenDuckDB(ctx, filepath.Join(t.TempDir(), "conflict.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	raw, _ := os.ReadFile(filepath.Join("..", "ingest", "testdata", "c1-wire-v1.json"))
	var f struct {
		Batches []connector.SnapshotEnvelope `json:"batches"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, f.Batches[0], AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	bad := f.Batches[0]
	bad.PayloadSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := d.ApplySnapshotEnvelope(ctx, bad, AppliedMark{}); err == nil {
		t.Fatal("conflicting replay accepted")
	}
}

func TestSnapshotDuplicateStagingErrorDoesNotExposeKey(t *testing.T) {
	ctx := context.Background()
	d, err := OpenDuckDB(ctx, filepath.Join(t.TempDir(), "staging-pii.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	raw, err := os.ReadFile(filepath.Join("..", "ingest", "testdata", "c1-wire-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Batches []connector.SnapshotEnvelope `json:"batches"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	const sensitiveKey = "fixture.patient.0042@example.test"
	first := fixture.Batches[0]
	first.Rows[0].Key = sensitiveKey
	first.PayloadSHA256, _ = connector.SnapshotPayloadDigest(first.Rows)
	if _, err := d.ApplySnapshotEnvelope(ctx, first, AppliedMark{Durable: "staging-pii", Seq: 1}); err != nil {
		t.Fatal(err)
	}

	conflict := fixture.Batches[1]
	conflict.Rows = append([]connector.SnapshotRow(nil), first.Rows...)
	conflict.PayloadSHA256, _ = connector.SnapshotPayloadDigest(conflict.Rows)
	if _, err := d.ApplySnapshotEnvelope(ctx, conflict, AppliedMark{Durable: "staging-pii", Seq: 2}); err == nil {
		t.Fatal("cross-batch duplicate accepted")
	} else {
		if !strings.Contains(err.Error(), "rejected by staging") {
			t.Fatalf("error = %q, want safe staging category", err)
		}
		if strings.Contains(err.Error(), sensitiveKey) {
			t.Fatalf("staging error exposed source row key: %v", err)
		}
	}

	var batches, rows int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM connector_snapshot_batches),(SELECT count(*) FROM connector_snapshot_rows)`).Scan(&batches, &rows)
	}); err != nil {
		t.Fatal(err)
	}
	if batches != 1 || rows != len(first.Rows) {
		t.Fatalf("partial bad batch committed: batches=%d rows=%d", batches, rows)
	}
	position, err := d.AppliedPosition(ctx, "staging-pii")
	if err != nil || position.Seq != 1 {
		t.Fatalf("mark advanced: %+v %v", position, err)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, first, AppliedMark{Durable: "staging-pii", Seq: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplySnapshotEnvelope(ctx, fixture.Batches[1], AppliedMark{Durable: "staging-pii", Seq: 3}); err != nil {
		t.Fatal(err)
	}
}
