package ingestion

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func TestDataRecoveryRestartReplayKeepsExactBusinessTotal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.duckdb")
	projectID, eventID := uuid.NewString(), uuid.NewString()
	amount := `{"amount":125}`
	event := storage.Event{ProjectID: projectID, EventID: eventID, DistinctID: "buyer", EventName: "purchase", EventType: "user",
		Properties: amount, Timestamp: time.Now().UTC()}

	d, err := storage.OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SinkEvents(ctx, []storage.Event{event}, storage.AppliedMark{Durable: "blue", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = storage.OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Crash-after-commit-before-ack redelivers the identical business event.
	if err := d.SinkEvents(ctx, []storage.Event{event}, storage.AppliedMark{Durable: "blue", Seq: 2}); err != nil {
		t.Fatal(err)
	}
	var count, total int64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*), sum(CAST(json_extract_string(properties, '$.amount') AS BIGINT)) FROM events WHERE project_id = ?`, projectID).Scan(&count, &total)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 || total != 125 {
		t.Fatalf("count/total = %d/%d, want 1/125", count, total)
	}
	pos, err := d.AppliedPosition(ctx, "blue")
	if err != nil {
		t.Fatal(err)
	}
	if !pos.Known || pos.Seq != 2 {
		t.Fatalf("position = %+v", pos)
	}
}
