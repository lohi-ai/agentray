package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

func TestSnapshotMillionRowPromotionAfterFragmentedStaging(t *testing.T) {
	for _, tc := range []struct {
		name            string
		completionFirst bool
	}{
		{name: "completion_last"},
		{name: "completion_first", completionFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testSnapshotMillionRowPromotionAfterFragmentedStaging(t, tc.completionFirst)
		})
	}
}

func testSnapshotMillionRowPromotionAfterFragmentedStaging(t *testing.T, completionFirst bool) {
	ctx := context.Background()
	db, err := OpenDuckDB(ctx, filepath.Join(t.TempDir(), "fragmented-million.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const projectID, connectorID = "00000000-0000-4000-8000-000000000201", "00000000-0000-4000-8000-000000000202"
	const syncID, generation = "00000000-0000-4000-8000-000000000203", "00000000-0000-4000-8000-000000000204"
	const runID, table = "00000000-0000-4000-8000-000000000205", "qa_exports.rows_v1"
	const binding = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	started := time.Now().UTC().Truncate(time.Microsecond)
	entries := make([]connector.SnapshotManifestEntry, 1001)
	for i := range entries {
		count := 1000
		if i == 1000 {
			count = 1
		}
		entries[i] = connector.SnapshotManifestEntry{Index: int64(i), BatchID: fmt.Sprintf("batch-%06d", i), PayloadSHA256: fmt.Sprintf("%064x", i+1), RowCount: count}
	}
	lastRows := []connector.SnapshotRow{{Key: "1000001", Data: json.RawMessage(`{"id":1000001,"amount":10000010}`)}}
	entries[1000].PayloadSHA256, err = connector.SnapshotPayloadDigest(lastRows)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := connector.SnapshotManifestDigest(entries)
	if err != nil {
		t.Fatal(err)
	}
	finished := started.Add(time.Minute)
	complete := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: connectorID, Table: table,
		SyncID: syncID, RunID: runID, Generation: generation, GenerationSeq: 1, BindingDigest: binding, CaptureStartedAt: started,
		CaptureFinishedAt: &finished, Kind: connector.SnapshotKindComplete, ExpectedBatches: 1001, ExpectedRows: 1000001, BatchManifestSHA256: manifest}
	last := complete
	last.Kind = connector.SnapshotKindBatch
	last.CaptureFinishedAt = nil
	last.ExpectedBatches, last.ExpectedRows, last.BatchManifestSHA256 = 0, 0, ""
	last.BatchID, last.BatchIndex, last.PayloadSHA256, last.Rows = entries[1000].BatchID, 1000, entries[1000].PayloadSHA256, lastRows

	if err := db.InsertExternalRows(ctx, projectID, connectorID, table, []connector.LandedRow{{Key: "old", DataJSON: `{"id":"old"}`}}, AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	if completionFirst {
		if promotion, err := db.ApplySnapshotEnvelope(ctx, complete, AppliedMark{Durable: "qa", Seq: 1}); err != nil || promotion != nil {
			t.Fatalf("early completion promotion=%+v err=%v", promotion, err)
		}
	}
	stagedBatches := len(entries)
	if completionFirst {
		stagedBatches--
	}
	for i := 0; i < stagedBatches; i++ {
		entry := entries[i]
		lower := int64(i*1000 + 1)
		upper := lower + int64(entry.RowCount)
		err = db.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_batches
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,batch_id,batch_index,payload_sha256,row_count,capture_started_at,run_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, projectID, connectorID, table, syncID, generation, 1, binding, entry.BatchID, entry.Index, entry.PayloadSHA256, entry.RowCount, started, runID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_rows (project_id,connector_id,table_name,generation,batch_id,row_key,data)
SELECT ?,?,?,?,?,CAST(i AS VARCHAR),'{' || '"id":' || CAST(i AS VARCHAR) || ',"amount":' || CAST(i*10 AS VARCHAR) || '}'
FROM range(?,?) t(i)`, projectID, connectorID, table, generation, entry.BatchID, lower, upper)
			return err
		})
		if err != nil {
			t.Fatalf("stage batch %d: %v", i, err)
		}
	}
	if got := snapshotLiveCount(t, db, projectID, connectorID, table); got != 1 {
		t.Fatalf("partial generation exposed %d live rows before promotion", got)
	}

	trigger := complete
	triggerStageTable := "connector_snapshot_completions"
	if completionFirst {
		trigger = last
		triggerStageTable = "connector_snapshot_batches"
	}
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE ingest_position`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplySnapshotEnvelope(ctx, trigger, AppliedMark{Durable: "qa", Seq: 1002}); err == nil {
		t.Fatal("promotion unexpectedly committed without ingest_position")
	}
	assertSnapshotPromotionState(t, db, projectID, connectorID, table, generation, triggerStageTable, 1, 0, 0)
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE ingest_position (
durable VARCHAR PRIMARY KEY,
applied_seq UBIGINT NOT NULL DEFAULT 0,
refused_missing UBIGINT NOT NULL DEFAULT 0,
updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	begin := time.Now()
	promotion, err := db.ApplySnapshotEnvelope(ctx, trigger, AppliedMark{Durable: "qa", Seq: 1002})
	if err != nil {
		t.Fatalf("promote fragmented million-row generation after %s: %v", time.Since(begin), err)
	}
	if promotion == nil || promotion.ExpectedRows != 1000001 {
		t.Fatalf("promotion=%+v", promotion)
	}
	assertSnapshotPromotionState(t, db, projectID, connectorID, table, generation, triggerStageTable, 1000001, 1, 1)
	if replay, err := db.ApplySnapshotEnvelope(ctx, trigger, AppliedMark{Durable: "qa", Seq: 1003}); err != nil || replay == nil {
		t.Fatalf("replay promotion=%+v err=%v", replay, err)
	}
	assertSnapshotPromotionState(t, db, projectID, connectorID, table, generation, triggerStageTable, 1000001, 1, 1)
}

func assertSnapshotPromotionState(t *testing.T, db *DuckDB, projectID, connectorID, table, generation, triggerStageTable string, wantLive, wantPromotions, wantTriggerStage int64) {
	t.Helper()
	var live, promotions, triggerStage int64
	if err := db.Read(context.Background(), func(conn *sql.Conn) error {
		triggerPredicate := ""
		if triggerStageTable == "connector_snapshot_batches" {
			triggerPredicate = " AND batch_index=1000"
		}
		query := fmt.Sprintf(`SELECT
(SELECT count(*) FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=?),
(SELECT count(*) FROM connector_snapshot_promotions WHERE project_id=? AND connector_id=? AND table_name=?),
(SELECT count(*) FROM %s WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?%s)`, triggerStageTable, triggerPredicate)
		return conn.QueryRowContext(context.Background(), query,
			projectID, connectorID, table,
			projectID, connectorID, table,
			projectID, connectorID, table, generation).Scan(&live, &promotions, &triggerStage)
	}); err != nil {
		t.Fatal(err)
	}
	if live != wantLive || promotions != wantPromotions {
		t.Fatalf("live=%d promotions=%d, want live=%d promotions=%d", live, promotions, wantLive, wantPromotions)
	}
	if triggerStage != wantTriggerStage {
		t.Fatalf("trigger staging rows=%d, want %d", triggerStage, wantTriggerStage)
	}
}
