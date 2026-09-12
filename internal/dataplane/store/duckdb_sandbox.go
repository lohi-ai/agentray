package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// duckdb_sandbox.go — the advanced-SQL execution environment.
//
// Untrusted SQL (run_sql, saved queries, custom charts, alert rules) never
// runs against the shared analytics file. Each project gets an in-memory
// DuckDB instance that physically holds ONLY that project's events, aliases,
// and external rows — copied in through a read-only ATTACH of the main file,
// which is then detached. The query runs on a connection whose configuration
// is locked down:
//
//   - enable_external_access=false: no file reads (read_csv/parquet/json,
//     'file.csv' FROM sugar), no network (httpfs), no sqlite/postgres scans,
//     no ATTACH, no extension INSTALL/LOAD.
//   - lock_configuration=true: the query cannot undo any of that, nor raise
//     memory/threads.
//   - memory_limit + threads bound the sandbox instance (per-instance, unlike
//     the shared file's global limit).
//   - temp_directory points at the spill dir beside the database file so a
//     big GROUP BY spills to bounded disk, not process memory.
//   - The caller's context deadline interrupts the query (go-duckdb maps ctx
//     cancellation to DuckDB's interrupt).
//
// Because the sandbox holds only the project's rows, a catalog escape
// (main.events, duckdb_tables(), a qualified name) can only ever see the
// project's own data — the rewrite in scopedReadonlySQL is a convenience
// layer (canonical_id, soft deletes), not the tenant boundary. The boundary
// is physical.
//
// Sandboxes are lazily created, refreshed incrementally on each use
// (INSERT OR IGNORE / INSERT OR REPLACE through the read-only attach), and
// evicted by an LRU bound so tenant count cannot multiply memory without
// limit.

const (
	// sandboxMaxProjects bounds live sandboxes; the least-recently-used one
	// is evicted (closed) when a new project needs a slot.
	sandboxMaxProjects = 16
	// sandboxMemoryLimit caps each sandbox instance's memory.
	sandboxMemoryLimit = "256MB"
	// sandboxThreads caps each sandbox instance's worker threads.
	sandboxThreads = 2
	// sandboxQueryTimeout bounds one untrusted query.
	sandboxQueryTimeout = 30 * time.Second
)

// sqlSandboxPool owns the per-project sandboxes for one Store.
type sqlSandboxPool struct {
	main *DuckDB

	mu   sync.Mutex
	// lru is most-recently-used first.
	lru       []string
	sandboxes map[string]*sqlSandbox
}

func newSQLSandboxPool(main *DuckDB) *sqlSandboxPool {
	return &sqlSandboxPool{main: main, sandboxes: map[string]*sqlSandbox{}}
}

// closeAll closes every sandbox. Called from CloseDuckDB before the main
// engine closes.
func (p *sqlSandboxPool) closeAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sb := range p.sandboxes {
		sb.close()
	}
	p.sandboxes = map[string]*sqlSandbox{}
	p.lru = nil
}

// query runs sqlText (already rewritten by scopedReadonlySQL) inside the
// project's sandbox and returns the rows. The sandbox is refreshed from the
// main file first, so a just-ingested batch is visible.
func (p *sqlSandboxPool) query(ctx context.Context, projectID, query string, args []any) ([]map[string]any, error) {
	sb, err := p.sandboxFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if err := sb.refresh(ctx); err != nil {
		return nil, fmt.Errorf("sandbox refresh: %w", err)
	}
	return sb.run(ctx, query, args)
}

// sandboxFor returns the project's sandbox, creating and loading it on first
// use, and evicts the LRU entry past the cap. Creation happens outside the
// pool lock so a cold open (attach + copy) never blocks other projects.
func (p *sqlSandboxPool) sandboxFor(ctx context.Context, projectID string) (*sqlSandbox, error) {
	p.mu.Lock()
	if sb, ok := p.sandboxes[projectID]; ok {
		p.touch(projectID)
		p.mu.Unlock()
		return sb, nil
	}
	p.mu.Unlock()

	sb, err := openSQLSandbox(ctx, p.main, projectID)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.sandboxes[projectID]; ok {
		// A concurrent opener won; use it and drop ours.
		sb.close()
		p.touch(projectID)
		return existing, nil
	}
	p.sandboxes[projectID] = sb
	p.lru = append([]string{projectID}, p.lru...)
	for len(p.lru) > sandboxMaxProjects {
		victim := p.lru[len(p.lru)-1]
		p.lru = p.lru[:len(p.lru)-1]
		if evicted, ok := p.sandboxes[victim]; ok {
			evicted.close()
			delete(p.sandboxes, victim)
		}
	}
	return sb, nil
}

func (p *sqlSandboxPool) touch(projectID string) {
	for i, id := range p.lru {
		if id == projectID {
			copy(p.lru[1:i+1], p.lru[:i])
			p.lru[0] = projectID
			return
		}
	}
	p.lru = append([]string{projectID}, p.lru...)
}

// sqlSandbox is one project's isolated in-memory DuckDB.
type sqlSandbox struct {
	projectID string
	mainPath  string
	db        *sql.DB
	// feeder is the unlocked connection: it alone may ATTACH the main file
	// read-only to refresh the sandbox tables. runner is the locked-down
	// connection every untrusted query executes on.
	feeder *sql.Conn
	runner *sql.Conn
	mu     sync.Mutex
	closed bool
}

// openSQLSandbox creates the in-memory instance, builds the project-scoped
// tables and views, and locks down the runner connection.
func openSQLSandbox(ctx context.Context, main *DuckDB, projectID string) (*sqlSandbox, error) {
	tmpDir := main.tmpDir()
	connector, err := duckdb.NewConnector("", func(execer driver.ExecerContext) error {
		if tmpDir == "" {
			return nil
		}
		_, err := execer.ExecContext(context.Background(),
			"SET temp_directory = '"+strings.ReplaceAll(tmpDir, "'", "''")+"'", nil)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)

	sb := &sqlSandbox{projectID: projectID, mainPath: main.Path(), db: db}
	feeder, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	sb.feeder = feeder
	runner, err := db.Conn(ctx)
	if err != nil {
		sb.close()
		return nil, err
	}
	sb.runner = runner

	// Resource bounds live on the instance (memory_limit/threads are
	// database-wide in DuckDB, which is exactly what a per-project sandbox
	// wants) and the access lockdown lives on the runner connection.
	for _, stmt := range []string{
		"SET memory_limit = '" + sandboxMemoryLimit + "'",
		fmt.Sprintf("SET threads = %d", sandboxThreads),
	} {
		if _, err := feeder.ExecContext(ctx, stmt); err != nil {
			sb.close()
			return nil, fmt.Errorf("sandbox %q: %w", stmt, err)
		}
	}
	for _, stmt := range []string{
		"SET enable_external_access = false",
		"SET lock_configuration = true",
	} {
		if _, err := runner.ExecContext(ctx, stmt); err != nil {
			sb.close()
			return nil, fmt.Errorf("sandbox %q: %w", stmt, err)
		}
	}

	if err := sb.createTables(ctx); err != nil {
		sb.close()
		return nil, err
	}
	if err := sb.refresh(ctx); err != nil {
		sb.close()
		return nil, err
	}
	return sb, nil
}

// createTables builds the sandbox's schema: the same column shapes the main
// file has, plus the resolved_events and sessions views, so the SQL contract
// (events, external_rows, canonical_id via scoped_events) is identical.
func (sb *sqlSandbox) createTables(ctx context.Context) error {
	for _, stmt := range sandboxSchema {
		if _, err := sb.feeder.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sandbox schema: %w", err)
		}
	}
	return nil
}

// refresh copies the project's rows from the main file into the sandbox.
// INSERT OR IGNORE / INSERT OR REPLACE make it incremental and idempotent;
// the anti-join DELETEs catch rows removed from the main file. The attach is
// read-only and detached before returning, so the sandbox never holds a live
// handle into the shared store.
func (sb *sqlSandbox) refresh(ctx context.Context) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.closed {
		return errDuckDBClosed
	}
	attach := fmt.Sprintf("ATTACH '%s' AS agentray_src (READ_ONLY)", strings.ReplaceAll(sb.mainPath, "'", "''"))
	if _, err := sb.feeder.ExecContext(ctx, attach); err != nil {
		return fmt.Errorf("attach main store: %w", err)
	}
	defer sb.feeder.ExecContext(context.Background(), "DETACH agentray_src")

	stmts := []string{
		// Events are append-only in the main file, so the high-water mark on
		// inserted_at bounds the copy to what arrived since the last refresh.
		`INSERT OR IGNORE INTO events SELECT * FROM agentray_src.events WHERE project_id = ? AND inserted_at > (SELECT coalesce(max(inserted_at), TIMESTAMPTZ '1970-01-01') FROM events)`,
		`INSERT OR IGNORE INTO aliases SELECT * FROM agentray_src.aliases WHERE project_id = ?`,
		`INSERT OR REPLACE INTO external_rows SELECT * FROM agentray_src.external_rows WHERE project_id = ?`,
		// Hard deletes in the main file must not linger in the sandbox. The
		// anti-join only runs when the row counts disagree — events are
		// append-only today, so the common refresh pays two counts, not a
		// per-row probe.
		`DELETE FROM events WHERE (SELECT count(*) FROM events) > (SELECT count(*) FROM agentray_src.events s WHERE s.project_id = ?) AND NOT EXISTS (SELECT 1 FROM agentray_src.events s WHERE s.project_id = events.project_id AND s.event_id = events.event_id)`,
		`DELETE FROM aliases WHERE NOT EXISTS (SELECT 1 FROM agentray_src.aliases s WHERE s.project_id = aliases.project_id AND s.anonymous_id = aliases.anonymous_id)`,
		`DELETE FROM external_rows WHERE NOT EXISTS (SELECT 1 FROM agentray_src.external_rows s WHERE s.project_id = external_rows.project_id AND s.connector_id = external_rows.connector_id AND s.table_name = external_rows.table_name AND s.row_key = external_rows.row_key)`,
	}
	for _, stmt := range stmts {
		var err error
		if strings.HasPrefix(stmt, "INSERT") {
			_, err = sb.feeder.ExecContext(ctx, stmt, sb.projectID)
		} else if strings.HasPrefix(stmt, "DELETE FROM events") {
			_, err = sb.feeder.ExecContext(ctx, stmt, sb.projectID)
		} else {
			_, err = sb.feeder.ExecContext(ctx, stmt)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// run executes one already-validated, already-rewritten SELECT on the locked
// runner connection under the caller's deadline (bounded by
// sandboxQueryTimeout) and materializes the rows.
func (sb *sqlSandbox) run(ctx context.Context, query string, args []any) ([]map[string]any, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.closed {
		return nil, errDuckDBClosed
	}
	qctx, cancel := context.WithTimeout(ctx, sandboxQueryTimeout)
	defer cancel()
	rows, err := sb.runner.QueryContext(qctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	results := []map[string]any{}
	for rows.Next() {
		valuePtrs := make([]any, len(columns))
		for i := range columns {
			scanType := columnTypes[i].ScanType()
			if scanType == nil {
				scanType = reflect.TypeOf("")
			}
			valuePtrs[i] = reflect.New(scanType).Interface()
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}
		item := map[string]any{}
		for i, column := range columns {
			item[column] = normalizeSQLValue(valuePtrs[i])
		}
		results = append(results, item)
	}
	return results, rows.Err()
}

func (sb *sqlSandbox) close() {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.closed {
		return
	}
	sb.closed = true
	if sb.feeder != nil {
		_ = sb.feeder.Close()
	}
	if sb.runner != nil {
		_ = sb.runner.Close()
	}
	if sb.db != nil {
		_ = sb.db.Close()
	}
}

// sandboxSchema mirrors the analytics tables inside a sandbox. The project_id
// columns stay: the scoped CTEs still filter on them, and keeping the column
// makes the sandbox schema a strict subset of the main one.
var sandboxSchema = []string{
	`CREATE TABLE IF NOT EXISTS events (
		project_id UUID NOT NULL,
		event_id UUID NOT NULL,
		distinct_id VARCHAR NOT NULL DEFAULT '',
		session_id VARCHAR NOT NULL DEFAULT '',
		event_name VARCHAR NOT NULL DEFAULT '',
		event_type VARCHAR NOT NULL DEFAULT '',
		properties VARCHAR NOT NULL DEFAULT '',
		agent_id VARCHAR,
		tool_name VARCHAR,
		tool_input VARCHAR,
		tool_output VARCHAR,
		tokens_input UINTEGER,
		tokens_output UINTEGER,
		cost_usd FLOAT,
		latency_ms UINTEGER,
		model_name VARCHAR,
		is_error BOOLEAN NOT NULL DEFAULT false,
		error_message VARCHAR,
		"timestamp" TIMESTAMPTZ NOT NULL,
		inserted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		visitor_class VARCHAR NOT NULL DEFAULT 'human',
		bot_name VARCHAR,
		referrer_host VARCHAR,
		referrer_channel VARCHAR NOT NULL DEFAULT '',
		user_agent VARCHAR,
		insert_id VARCHAR,
		is_unplanned BOOLEAN NOT NULL DEFAULT false,
		platform VARCHAR NOT NULL DEFAULT '',
		PRIMARY KEY (project_id, event_id)
	)`,
	`CREATE TABLE IF NOT EXISTS aliases (
		project_id UUID NOT NULL,
		anonymous_id VARCHAR NOT NULL,
		canonical_id VARCHAR NOT NULL,
		PRIMARY KEY (project_id, anonymous_id)
	)`,
	`CREATE TABLE IF NOT EXISTS external_rows (
		project_id UUID NOT NULL,
		connector_id UUID NOT NULL,
		table_name VARCHAR NOT NULL,
		row_key VARCHAR NOT NULL,
		cursor VARCHAR NOT NULL DEFAULT '',
		data VARCHAR NOT NULL DEFAULT '',
		synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (project_id, connector_id, table_name, row_key)
	)`,
	`CREATE VIEW IF NOT EXISTS resolved_events AS
	SELECT e.*, coalesce(a.canonical_id, e.distinct_id) AS canonical_distinct_id
	FROM events e
	LEFT JOIN aliases a
		ON a.project_id = e.project_id AND a.anonymous_id = e.distinct_id`,
	`CREATE VIEW IF NOT EXISTS sessions AS
	SELECT
		project_id,
		session_id,
		distinct_id,
		min("timestamp") AS session_start,
		max("timestamp") AS session_end,
		count(*) AS event_count,
		sum(coalesce(tokens_input, 0)) AS total_tokens_in,
		sum(coalesce(tokens_output, 0)) AS total_tokens_out,
		sum(coalesce(cost_usd, 0)) AS total_cost_usd,
		max("timestamp") AS last_event_at
	FROM events
	WHERE session_id <> ''
	GROUP BY project_id, session_id, distinct_id`,
}
