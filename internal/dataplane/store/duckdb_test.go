package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func openTestDuckDB(t *testing.T) *DuckDB {
	t.Helper()
	d, err := OpenDuckDB(context.Background(), filepath.Join(t.TempDir(), "analytics.duckdb"))
	if err != nil {
		t.Fatalf("OpenDuckDB: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("DuckDB.Close: %v", err)
		}
	})
	return d
}

func duckEvent(projectID, eventID, distinctID string, at time.Time) Event {
	return Event{
		ProjectID:    projectID,
		EventID:      eventID,
		DistinctID:   distinctID,
		EventName:    "user.signed_up",
		EventType:    "user",
		Properties:   `{"$set":{"email":"reader@example.com","plan":"pro"},"$set_once":{"first_source":"landing"}}`,
		SessionID:    "session-1",
		Timestamp:    at.UTC(),
		VisitorClass: "human",
	}
}

func duckCount(t *testing.T, d *DuckDB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := d.Read(context.Background(), func(conn *sql.Conn) error {
		return conn.QueryRowContext(context.Background(), query, args...).Scan(&count)
	}); err != nil {
		t.Fatalf("query count: %v", err)
	}
	return count
}

// Boot creates the complete v1 schema from a missing database and exposes the
// version/name contract the query-parity ticket consumes.
func TestDuckDBBootCreatesSchema(t *testing.T) {
	d := openTestDuckDB(t)
	if got := duckCount(t, d, `SELECT count(*) FROM schema_meta WHERE name = 'schema' AND version = ?`, DuckDBSchemaVersion); got != 1 {
		t.Fatalf("schema_meta rows = %d, want 1", got)
	}
	for _, name := range []string{DuckDBEventsName, DuckDBAliasesName, DuckDBPersonsName, DuckDBExternalRowsName, DuckDBResolvedEventsName, DuckDBSessionsName} {
		if got := duckCount(t, d, `SELECT count(*) FROM information_schema.tables WHERE table_name = ?`, name); got != 1 {
			t.Fatalf("schema object %q missing", name)
		}
	}
}

// A second open over the same path observes the committed event and its person
// projection: clean close/restart has no bootstrap-only state.
func TestDuckDBRestartDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.duckdb")
	ctx := context.Background()
	d, err := OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	projectID, eventID := uuid.NewString(), uuid.NewString()
	if err := d.SinkEvents(ctx, []Event{duckEvent(projectID, eventID, "reader", time.Now())}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := duckCount(t, reopened, `SELECT count(*) FROM events WHERE project_id = ?`, projectID); got != 1 {
		t.Fatalf("events after restart = %d, want 1", got)
	}
	profiles, err := reopened.PersonProfilesByKeys(ctx, projectID, []string{"reader"})
	if err != nil {
		t.Fatalf("PersonProfilesByKeys: %v", err)
	}
	if profiles["reader"] == nil || profiles["reader"].Email != "reader@example.com" {
		t.Fatalf("person projection missing after restart: %#v", profiles["reader"])
	}
}

// The same post-commit/pre-ack message may be replayed. The durable event key
// and transactional person projection must make that exact replay a no-op.
func TestDuckDBRedeliveryIdempotent(t *testing.T) {
	d := openTestDuckDB(t)
	projectID, eventID := uuid.NewString(), uuid.NewString()
	event := duckEvent(projectID, eventID, "reader", time.Now())
	for i := 0; i < 2; i++ {
		if err := d.SinkEvents(context.Background(), []Event{event}); err != nil {
			t.Fatalf("SinkEvents replay %d: %v", i, err)
		}
	}
	if got := duckCount(t, d, `SELECT count(*) FROM events WHERE project_id = ?`, projectID); got != 1 {
		t.Fatalf("replayed event count = %d, want 1", got)
	}
	if got := duckCount(t, d, `SELECT count(*) FROM persons WHERE project_id = ?`, projectID); got != 1 {
		t.Fatalf("replayed person count = %d, want 1", got)
	}
}

// Aliases are mirrored before the ingest transaction; the transactional
// projection must use the canonical identity and sessions remain project-keyed.
func TestDuckDBProjectIsolation(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectA, projectB := uuid.NewString(), uuid.NewString()
	if err := d.UpsertAliases(ctx, [][3]string{{projectA, "anon", "reader"}}); err != nil {
		t.Fatalf("UpsertAliases: %v", err)
	}
	if err := d.SinkEvents(ctx, []Event{
		duckEvent(projectA, uuid.NewString(), "anon", time.Now()),
		duckEvent(projectB, uuid.NewString(), "anon", time.Now()),
	}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
	profilesA, err := d.PersonProfilesByKeys(ctx, projectA, []string{"reader"})
	if err != nil || profilesA["reader"] == nil {
		t.Fatalf("project A canonical profile: profiles=%#v err=%v", profilesA, err)
	}
	profilesB, err := d.PersonProfilesByKeys(ctx, projectB, []string{"anon"})
	if err != nil || profilesB["anon"] == nil {
		t.Fatalf("project B profile leaked/canonicalized: profiles=%#v err=%v", profilesB, err)
	}
	if got := duckCount(t, d, `SELECT count(*) FROM resolved_events WHERE project_id = ? AND canonical_distinct_id = 'reader'`, projectB); got != 0 {
		t.Fatalf("project B saw project A alias: %d rows", got)
	}
}

// Four reader slots can overlap a serialized writer. Readers see either the
// snapshot before or after the transaction, never a partial batch or a locked
// database error.
func TestDuckDBConcurrentReadWrite(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	if err := d.SinkEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "reader", time.Now())}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, maxDuckDBReaders+1)
	for range maxDuckDBReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- d.Read(ctx, func(conn *sql.Conn) error {
				var count int
				return conn.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE project_id = ?`, projectID).Scan(&count)
			})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- d.SinkEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "reader", time.Now())})
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation: %v", err)
		}
	}
	if got := duckCount(t, d, `SELECT count(*) FROM events WHERE project_id = ?`, projectID); got != 2 {
		t.Fatalf("events after concurrent write = %d, want 2", got)
	}
}
