package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A spill fixes temp_directory for the lifetime of the database. Opening a
// second pooled connection must still work, including ingestion checkpoints.
func TestDuckDBPoolConnectionAfterSpill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := OpenDuckDB(ctx, filepath.Join(t.TempDir(), "quote's directory", "analytics.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `CREATE TEMP TABLE spill_fixture AS SELECT i, repeat(md5(i::VARCHAR), 4) AS payload FROM range(2000000) t(i)`); err != nil {
		t.Fatal(err)
	}
	var spilled int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM duckdb_temporary_files()`).Scan(&spilled); err != nil {
		t.Fatal(err)
	}
	if spilled == 0 {
		t.Fatal("fixture did not spill; regression was not exercised")
	}
	if err = d.RecordPosition(ctx, AppliedMark{Durable: "spill-test", Seq: 42}); err != nil {
		t.Fatalf("ingestion checkpoint after spill: %v", err)
	}
	pos, err := d.AppliedPosition(ctx, "spill-test")
	if err != nil || pos.Seq != 42 {
		t.Fatalf("checkpoint: %+v, %v", pos, err)
	}
}
