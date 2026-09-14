package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
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
	if err := d.SinkEvents(ctx, events, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents with a full batch: %v", err)
	}
	// The fold writes one row per identity-bearing distinct id, so a batch of
	// identity updates is the same shape: every event here carries traits and
	// its own distinct id.
	identified := make([]Event, 0, 500)
	for i := range cap(identified) {
		identified = append(identified, Event{
			ProjectID: projectID, EventID: uuid.NewString(), EventName: "$identify",
			DistinctID: fmt.Sprintf("person-%d", i), SessionID: "s1",
			Properties: `{"$set":{"plan":"pro","note":"` + strings.Repeat("x", 120) + `"}}`,
			Timestamp:  time.Now().UTC(), EventType: "user",
		})
	}
	if err := d.SinkEvents(ctx, identified, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents with 500 distinct identities: %v", err)
	}
	// The connector landing path takes up to 1,000 rows per batch.
	landed := make([]connector.LandedRow, 0, 1000)
	for i := range cap(landed) {
		landed = append(landed, connector.LandedRow{
			Key:      fmt.Sprintf("row-%d", i),
			Cursor:   fmt.Sprintf("cursor-%d", i),
			DataJSON: `{"note":"` + strings.Repeat("x", 200) + `"}`,
		})
	}
	if err := d.InsertExternalRows(ctx, projectID, uuid.NewString(), "landed", landed, AppliedMark{}); err != nil {
		t.Fatalf("InsertExternalRows with a full connector batch: %v", err)
	}
	var n int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(events)+len(identified) {
		t.Fatalf("stored %d rows, want %d (the re-sink must dedup on the primary key)", n, len(events)+len(identified))
	}
	var landedRows int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM external_rows`).Scan(&landedRows)
	}); err != nil {
		t.Fatalf("count external_rows: %v", err)
	}
	if landedRows != len(landed) {
		t.Fatalf("landed %d connector rows, want %d", landedRows, len(landed))
	}
}
