package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// The sandbox is the security boundary for untrusted SQL: a per-project
// in-memory DuckDB holding only that project's rows, on a locked-down
// connection. These tests run the real machinery — no mocks — because the
// contract is exactly what a hostile or careless query can reach.

func TestSandboxSeesOnlyItsOwnProject(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	p1, p2 := uuid.NewString(), uuid.NewString()
	if err := d.InsertEvents(ctx, []Event{
		duckEvent(p1, uuid.NewString(), "user-a", time.Now()),
		duckEvent(p2, uuid.NewString(), "user-b", time.Now()),
	}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)

	rows, err := pool.query(ctx, p1, `SELECT count(*) AS n FROM events`, nil)
	if err != nil {
		t.Fatalf("sandbox query: %v", err)
	}
	if got := rows[0]["n"]; got != int64(1) {
		t.Fatalf("project p1 sees %v events, want 1", got)
	}
}

func TestSandboxRefreshPicksUpNewEvents(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	p1 := uuid.NewString()
	if err := d.InsertEvents(ctx, []Event{duckEvent(p1, uuid.NewString(), "user-a", time.Now())}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)

	rows, err := pool.query(ctx, p1, `SELECT count(*) AS n FROM events`, nil)
	if err != nil || rows[0]["n"] != int64(1) {
		t.Fatalf("first query: rows=%v err=%v", rows, err)
	}
	if err := d.InsertEvents(ctx, []Event{duckEvent(p1, uuid.NewString(), "user-a", time.Now())}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	rows, err = pool.query(ctx, p1, `SELECT count(*) AS n FROM events`, nil)
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	if got := rows[0]["n"]; got != int64(2) {
		t.Fatalf("after refresh project sees %v events, want 2", got)
	}
}

// external_rows is copied incrementally on (synced_at, connector_id,
// table_name, row_key) with a count comparison for deletions, so a sandbox
// query no longer recopies the project's whole landing table. The sentinel row
// below is the observable: only a wholesale recopy would erase an edit made in
// the sandbox itself.
func TestSandboxRefreshExternalRowsIncremental(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	p1, c1 := uuid.NewString(), uuid.NewString()
	land := func(rows ...connector.LandedRow) {
		t.Helper()
		if err := d.InsertExternalRows(ctx, p1, c1, "users", rows); err != nil {
			t.Fatalf("InsertExternalRows: %v", err)
		}
	}
	land(connector.LandedRow{Key: "a", DataJSON: `{"v":1}`})

	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)

	if got := sandboxExternalRowKeys(t, pool, ctx, p1); strings.Join(got, ",") != "a" {
		t.Fatalf("first query sees %v, want [a]", got)
	}

	// Rewrite the sandbox's own copy of row a.
	sb, err := pool.sandboxFor(ctx, p1)
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	_, err = sb.feeder.ExecContext(ctx, `UPDATE external_rows SET data = '{"sentinel":true}' WHERE row_key = 'a'`)
	sb.release()
	if err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	// A row landed after the last refresh is copied; the rows already there are
	// not re-read, so the sentinel survives.
	land(connector.LandedRow{Key: "b", DataJSON: `{"v":2}`})
	rows, err := pool.query(ctx, p1, `SELECT row_key, data FROM external_rows WHERE row_key = 'a'`, nil)
	if err != nil {
		t.Fatalf("query after second landing: %v", err)
	}
	if len(rows) != 1 || rows[0]["data"] != `{"sentinel":true}` {
		t.Fatalf("row a after incremental refresh = %v, want the sandbox's own value", rows)
	}
	if got := sandboxExternalRowKeys(t, pool, ctx, p1); strings.Join(got, ",") != "a,b" {
		t.Fatalf("after second landing sees %v, want [a b]", got)
	}

	// Equal-count drift: a is deleted upstream while c lands, so the count
	// comparison is the only thing that can notice a is gone.
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		_, derr := tx.ExecContext(ctx, `DELETE FROM external_rows WHERE project_id = ? AND row_key = 'a'`, p1)
		return derr
	}); err != nil {
		t.Fatalf("delete upstream row: %v", err)
	}
	land(connector.LandedRow{Key: "c", DataJSON: `{"v":3}`})

	if got := sandboxExternalRowKeys(t, pool, ctx, p1); strings.Join(got, ",") != "b,c" {
		t.Fatalf("after upstream delete sees %v, want [b c]", got)
	}
}

func sandboxExternalRowKeys(t *testing.T, pool *sqlSandboxPool, ctx context.Context, projectID string) []string {
	t.Helper()
	rows, err := pool.query(ctx, projectID, `SELECT row_key FROM external_rows ORDER BY row_key`, nil)
	if err != nil {
		t.Fatalf("query external_rows: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		key, _ := r["row_key"].(string)
		out = append(out, key)
	}
	return out
}

func TestSandboxRunnerCannotReadFiles(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	p1 := uuid.NewString()
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)

	for _, q := range []string{
		`SELECT * FROM read_csv('/etc/passwd')`,
		`SELECT * FROM '/etc/passwd'`,
		`ATTACH '/tmp/x.db' AS x`,
		`SET memory_limit = '1GB'`,
		`COPY (SELECT 1) TO '/tmp/x.csv'`,
	} {
		if _, err := pool.query(ctx, p1, q, nil); err == nil {
			t.Errorf("sandbox runner allowed %q", q)
		}
	}
}

func TestSandboxCanonicalIDStitchesAliases(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	p1 := uuid.NewString()
	if err := d.InsertEvents(ctx, []Event{
		duckEvent(p1, uuid.NewString(), "anon-1", time.Now()),
		duckEvent(p1, uuid.NewString(), "user-1", time.Now()),
	}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	if err := d.UpsertAliases(ctx, [][3]string{{p1, "anon-1", "user-1"}}); err != nil {
		t.Fatalf("UpsertAliases: %v", err)
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)

	rows, err := pool.query(ctx, p1,
		`SELECT count(DISTINCT canonical_id) AS people FROM (SELECT *, canonical_distinct_id AS canonical_id FROM resolved_events WHERE project_id = '`+p1+`')`, nil)
	if err != nil {
		t.Fatalf("sandbox query: %v", err)
	}
	if got := rows[0]["people"]; got != int64(1) {
		t.Fatalf("stitched people = %v, want 1 (anon folded onto user-1)", got)
	}
}

func TestRunSQLEndToEndThroughSandbox(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	p1 := uuid.NewString()
	if err := d.InsertEvents(ctx, []Event{
		duckEvent(p1, uuid.NewString(), "user-a", time.Now()),
	}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	s := &Store{duck: d, sandboxes: newSQLSandboxPool(d)}

	rows, err := s.RunSQL(ctx, p1, `SELECT event_name, count(*) AS n FROM events GROUP BY event_name`)
	if err != nil {
		t.Fatalf("RunSQL: %v", err)
	}
	if len(rows) != 1 || rows[0]["event_name"] != "user.signed_up" {
		t.Fatalf("rows = %v", rows)
	}

	// A query that names another project's id still sees only p1's rows.
	other := uuid.NewString()
	rows, err = s.RunSQL(ctx, p1, `SELECT count(*) AS n FROM events WHERE project_id = '`+other+`'`)
	if err != nil {
		t.Fatalf("RunSQL cross-project: %v", err)
	}
	if got := rows[0]["n"]; got != int64(0) {
		t.Fatalf("cross-project count = %v, want 0", got)
	}

	// The guard still rejects non-SELECT and file reads before the sandbox.
	for _, q := range []string{
		`DELETE FROM events`,
		`SELECT * FROM read_parquet('/tmp/x.parquet')`,
		`SELECT * FROM 'data.csv'`,
	} {
		if _, err := s.RunSQL(ctx, p1, q); err == nil {
			t.Errorf("RunSQL allowed %q", q)
		} else if !strings.Contains(err.Error(), "forbidden") && !strings.Contains(err.Error(), "not allowed") && !strings.Contains(err.Error(), "only SELECT") {
			t.Errorf("RunSQL(%q) error = %v, want a guard rejection", q, err)
		}
	}
}
