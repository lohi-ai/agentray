package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A full ingest batch must fit inside the trusted engine's own memory_limit.
// The write path used to run one INSERT statement per event, and DuckDB holds a
// row-group-sized buffer per statement until the transaction commits: at
// memory_limit=128MB a 500-row batch of this shape died with "Out of Memory
// Error" around row 108 — which is the production ingest batcher's flush size.
// The batch is inserted one statement per chunk instead, so this is the
// regression guard for that.
func TestIngestBatchFitsTheMainEngineLimit(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	// ~200 bytes of properties: an ordinary SDK payload, not an adversarial one.
	props := `{"path":"/pricing","tool":"search","latency_ms":142,"model":"flash","note":"` +
		strings.Repeat("x", 120) + `"}`
	events := make([]Event, 0, 500)
	for i := range cap(events) {
		events = append(events, Event{
			ProjectID: projectID, EventID: uuid.NewString(), EventName: "batch",
			DistinctID: fmt.Sprintf("u%d", i%64), SessionID: "s1",
			Properties: props, Timestamp: time.Now().UTC(), EventType: "user",
		})
	}
	if err := d.InsertEvents(ctx, events); err != nil {
		t.Fatalf("InsertEvents with a full batch: %v", err)
	}
	// The ingest worker's path adds the person fold to the same transaction.
	if err := d.SinkEvents(ctx, events); err != nil {
		t.Fatalf("SinkEvents with a full batch: %v", err)
	}
	var n int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(events) {
		t.Fatalf("stored %d rows, want %d (the re-sink must dedup on the primary key)", n, len(events))
	}
}
