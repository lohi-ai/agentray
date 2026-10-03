package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
