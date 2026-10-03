package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

func TestSnapshotMillionRowPromotionAfterFragmentedStaging(t *testing.T) {
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
		entry := connector.SnapshotManifestEntry{Index: int64(i), BatchID: fmt.Sprintf("batch-%06d", i), PayloadSHA256: fmt.Sprintf("%064x", i+1), RowCount: count}
		entries[i] = entry
		lower := int64(i*1000 + 1)
		upper := lower + int64(count)
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
	manifest, err := connector.SnapshotManifestDigest(entries)
	if err != nil {
		t.Fatal(err)
	}
	finished := started.Add(time.Minute)
	complete := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: connectorID, Table: table,
		SyncID: syncID, RunID: runID, Generation: generation, GenerationSeq: 1, BindingDigest: binding, CaptureStartedAt: started,
		CaptureFinishedAt: &finished, Kind: connector.SnapshotKindComplete, ExpectedBatches: 1001, ExpectedRows: 1000001, BatchManifestSHA256: manifest}
	begin := time.Now()
	promotion, err := db.ApplySnapshotEnvelope(ctx, complete, AppliedMark{Durable: "qa", Seq: 1002})
	if err != nil {
		t.Fatalf("promote fragmented million-row generation after %s: %v", time.Since(begin), err)
	}
	if promotion == nil || promotion.ExpectedRows != 1000001 {
		t.Fatalf("promotion=%+v", promotion)
	}
	var count int64
	if err := db.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=?`, projectID, connectorID, table).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1000001 {
		t.Fatalf("live rows=%d, want 1000001", count)
	}
}
