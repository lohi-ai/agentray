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
// and external rows — copied in through Go reads on the main store, because
// the sandbox instance itself is opened with enable_external_access=false:
// it cannot read a file, open a network connection, ATTACH, or INSTALL/LOAD
// an extension, and lock_configuration=true means the query cannot undo any
// of that or raise memory/threads.
//
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
// Sandboxes are lazily created, refreshed incrementally on each use, and
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
	// sandboxCopyBatch is the row count per INSERT batch during refresh.
	sandboxCopyBatch = 2048
)

// sqlSandboxPool owns the per-project sandboxes for one Store.
type sqlSandboxPool struct {
	main *DuckDB

	mu sync.Mutex
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
// pool lock so a cold open (schema + copy) never blocks other projects.
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
	main      *DuckDB
	db        *sql.DB
	// feeder is the connection refresh writes through; runner is the
	// connection every untrusted query executes on. Both live on the same
	// locked-down instance — the split exists so a refresh and a query never
	// share a connection.
	feeder *sql.Conn
	runner *sql.Conn
	mu     sync.Mutex
	closed bool
}

// openSQLSandbox creates the in-memory instance, builds the project-scoped
// tables and views, and locks the whole instance down before any data lands.
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

	sb := &sqlSandbox{projectID: projectID, main: main, db: db}
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

	// Lockdown is instance-wide: enable_external_access is a global setting in
	// DuckDB, so it must be set before the feeder needs no files (it never
	// does — refresh copies rows through Go). After this the instance cannot
	// touch the filesystem or network at all.
	for _, stmt := range []string{
		"SET memory_limit = '" + sandboxMemoryLimit + "'",
		fmt.Sprintf("SET threads = %d", sandboxThreads),
		"SET enable_external_access = false",
		"SET lock_configuration = true",
	} {
		if _, err := feeder.ExecContext(ctx, stmt); err != nil {
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

// refresh copies the project's rows from the main store into the sandbox.
// Events are append-only, so the copy is incremental on inserted_at; aliases
// and external_rows are small enough to reconcile wholesale. A row removed
// from the main file is removed here too — the count check keeps the common
// refresh cheap.
func (sb *sqlSandbox) refresh(ctx context.Context) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.closed {
		return errDuckDBClosed
	}
	if err := sb.refreshEvents(ctx); err != nil {
		return fmt.Errorf("refresh events: %w", err)
	}
	if err := sb.refreshTable(ctx, "aliases",
		`SELECT project_id, anonymous_id, canonical_id FROM aliases WHERE project_id = ?`, 3); err != nil {
		return fmt.Errorf("refresh aliases: %w", err)
	}
	if err := sb.refreshTable(ctx, "external_rows",
		`SELECT project_id, connector_id, table_name, row_key, cursor, data, synced_at FROM external_rows WHERE project_id = ?`, 7); err != nil {
		return fmt.Errorf("refresh external_rows: %w", err)
	}
	return nil
}

// refreshEvents appends only what arrived since the last refresh. A count
// drift (a delete in the main file — not possible today) rebuilds the table.
func (sb *sqlSandbox) refreshEvents(ctx context.Context) error {
	var highWater time.Time
	if err := sb.feeder.QueryRowContext(ctx,
		`SELECT coalesce(max(inserted_at), TIMESTAMPTZ '1970-01-01') FROM events`).Scan(&highWater); err != nil {
		return err
	}
	var sandboxCount, mainCount int64
	if err := sb.feeder.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&sandboxCount); err != nil {
		return err
	}
	if err := sb.main.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx,
			`SELECT count(*) FROM events WHERE project_id = ?`, sb.projectID).Scan(&mainCount)
	}); err != nil {
		return err
	}
	if sandboxCount > mainCount {
		// Rows vanished from the main file; rebuild rather than probe per row.
		if _, err := sb.feeder.ExecContext(ctx, `DELETE FROM events`); err != nil {
			return err
		}
		highWater = time.Time{}
	}
	return sb.copyRows(ctx,
		`SELECT * FROM events WHERE project_id = ? AND inserted_at > ? ORDER BY inserted_at`,
		[]any{sb.projectID, highWater},
		"events", 28)
}

// refreshTable reconciles a small table wholesale: delete-then-copy so a row
// removed upstream disappears here too. Both tables are small (aliases are
// one row per identify, external_rows one per synced source row).
func (sb *sqlSandbox) refreshTable(ctx context.Context, table, selectSQL string, nCols int) error {
	if _, err := sb.feeder.ExecContext(ctx, `DELETE FROM `+table); err != nil {
		return err
	}
	return sb.copyRows(ctx, selectSQL, []any{sb.projectID}, table, nCols)
}

// copyRows streams rows out of the main store and inserts them into the
// sandbox in batches. The sandbox never sees the file — only values.
func (sb *sqlSandbox) copyRows(ctx context.Context, selectSQL string, selectArgs []any, table string, nCols int) error {
	var rows *sql.Rows
	err := sb.main.Read(ctx, func(conn *sql.Conn) error {
		var err error
		rows, err = conn.QueryContext(ctx, selectSQL, selectArgs...)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		if len(cols) != nCols {
			return fmt.Errorf("column count mismatch: %d != %d", len(cols), nCols)
		}
		// Build the batched INSERT once.
		placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", nCols), ",") + ")"
		insertSQL := fmt.Sprintf("INSERT INTO %s VALUES %s", table,
			strings.TrimSuffix(strings.Repeat(placeholder+",", sandboxCopyBatch), ","))
		stmt, err := sb.feeder.PrepareContext(ctx, insertSQL)
		if err != nil {
			return err
		}
		defer stmt.Close()
		single, err := sb.feeder.PrepareContext(ctx,
			fmt.Sprintf("INSERT INTO %s VALUES %s", table, placeholder))
		if err != nil {
			return err
		}
		defer single.Close()

		batch := make([]any, 0, sandboxCopyBatch*nCols)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			if _, err := stmt.ExecContext(ctx, batch...); err != nil {
				return err
			}
			batch = batch[:0]
			return nil
		}
		for rows.Next() {
			dest := make([]any, nCols)
			for i := range dest {
				dest[i] = new(any)
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			for i := range dest {
				batch = append(batch, *(dest[i].(*any)))
			}
			if len(batch) == sandboxCopyBatch*nCols {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Tail rows that didn't fill a batch.
		for len(batch) > 0 {
			n := nCols
			if len(batch) < n {
				n = len(batch)
			}
			if _, err := single.ExecContext(ctx, batch[:n]...); err != nil {
				return err
			}
			batch = batch[n:]
		}
		return nil
	})
	return err
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
		item := make(map[string]any, len(columns))
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
