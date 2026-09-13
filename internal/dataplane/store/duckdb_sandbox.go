package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
//     the shared file's global limit); a process-wide semaphore bounds how
//     many sandboxes run a query at once.
//   - temp_directory is a per-sandbox dir under the main file's tmp/ — two
//     instances must never share one (DuckDB names spill files
//     duckdb_temp_storage-* and concurrent instances collide) — and
//     max_temp_directory_size caps the spill so a high-cardinality GROUP BY
//     cannot fill the volume the main database lives on.
//   - The caller's context deadline interrupts the query (go-duckdb maps ctx
//     cancellation to DuckDB's interrupt); sandboxQueryTimeout bounds it.
//   - Results materialize into Go under a hard row cap — the engine's memory
//     limit does not cover the []map[string]any the caller receives.
//
// Because the sandbox holds only the project's rows, a catalog escape
// (main.events, duckdb_tables(), a qualified name) can only ever see the
// project's own data — the rewrite in scopedReadonlySQL is a convenience
// layer (canonical_id, soft deletes), not the tenant boundary. The boundary
// is physical.
//
// Sandboxes are lazily created (single-flight per project), refreshed
// incrementally on each use, leased while a query is in flight so eviction
// cannot close one mid-read, and evicted by an LRU bound so tenant count
// cannot multiply memory without limit.

const (
	// sandboxMaxProjects bounds live sandboxes; the least-recently-used one
	// is evicted (closed) when a new project needs a slot.
	sandboxMaxProjects = 8
	// sandboxMemoryLimit caps each sandbox instance's memory.
	sandboxMemoryLimit = "128MB"
	// sandboxThreads caps each sandbox instance's worker threads.
	sandboxThreads = 1
	// sandboxMaxConcurrent bounds queries executing across ALL sandboxes —
	// per-instance limits alone would let N projects × 128MB exhaust the
	// container. Two concurrent untrusted queries is generous for a
	// dashboard; the rest queue.
	sandboxMaxConcurrent = 2
	// sandboxQueryTimeout bounds one untrusted query.
	sandboxQueryTimeout = 30 * time.Second
	// sandboxRefreshTimeout bounds the copy phase: a project whose events
	// take longer than this to mirror fails the query rather than holding
	// the sandbox mutex (and its lease) indefinitely.
	sandboxRefreshTimeout = 60 * time.Second
	// sandboxCopyBatch is the row count per INSERT batch during refresh.
	sandboxCopyBatch = 2048
	// sandboxMaxRows caps materialized result rows. The engine's memory
	// limit does not cover the Go maps the caller receives.
	sandboxMaxRows = 10_000
	// sandboxTempSize caps one sandbox's spill directory.
	sandboxTempSize = "512MB"
)

// sqlSandboxPool owns the per-project sandboxes for one Store.
type sqlSandboxPool struct {
	main *DuckDB

	// sem bounds concurrent untrusted queries across all sandboxes.
	sem chan struct{}

	mu sync.Mutex
	// lru is most-recently-used first.
	lru       []string
	sandboxes map[string]*sqlSandbox
	// opening single-flights a cold open per project so a burst of queries
	// for one new project doesn't open N sandboxes and discard N-1.
	opening map[string]*sandboxOpen
}

type sandboxOpen struct {
	done chan struct{}
	err  error
}

func newSQLSandboxPool(main *DuckDB) *sqlSandboxPool {
	return &sqlSandboxPool{
		main:      main,
		sem:       make(chan struct{}, sandboxMaxConcurrent),
		sandboxes: map[string]*sqlSandbox{},
		opening:   map[string]*sandboxOpen{},
	}
}

// closeAll closes every sandbox. Called from CloseDuckDB before the main
// engine closes.
func (p *sqlSandboxPool) closeAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	sandboxes := p.sandboxes
	p.sandboxes = map[string]*sqlSandbox{}
	p.lru = nil
	p.mu.Unlock()
	for _, sb := range sandboxes {
		sb.close()
	}
}

// query runs sqlText (already rewritten by scopedReadonlySQL) inside the
// project's sandbox and returns the rows. The sandbox is refreshed from the
// main file first, so a just-ingested batch is visible.
func (p *sqlSandboxPool) query(ctx context.Context, projectID, query string, args []any) ([]map[string]any, error) {
	sb, err := p.sandboxFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// sandboxFor returns the sandbox already leased, so eviction cannot close it
	// between lookup and this query starting.
	defer sb.release()

	rctx, rcancel := context.WithTimeout(ctx, sandboxRefreshTimeout)
	if err := sb.refresh(rctx); err != nil {
		rcancel()
		return nil, fmt.Errorf("sandbox refresh: %w", err)
	}
	rcancel()

	// Bound concurrent untrusted queries process-wide.
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return sb.run(ctx, query, args)
}

// sandboxFor returns the project's sandbox, creating and loading it on first
// use, and evicts the LRU entry past the cap. Creation is single-flighted per
// project and happens outside the pool lock so a cold open (schema + copy)
// never blocks other projects.
func (p *sqlSandboxPool) sandboxFor(ctx context.Context, projectID string) (*sqlSandbox, error) {
	for {
		p.mu.Lock()
		if sb, ok := p.sandboxes[projectID]; ok {
			sb.lease()
			p.touch(projectID)
			p.mu.Unlock()
			return sb, nil
		}
		if op, ok := p.opening[projectID]; ok {
			p.mu.Unlock()
			select {
			case <-op.done:
				if op.err != nil {
					return nil, op.err
				}
				// Re-enter through the pool lock: the opener's lease may have
				// ended and allowed eviction before this waiter woke.
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		op := &sandboxOpen{done: make(chan struct{})}
		p.opening[projectID] = op
		p.mu.Unlock()

		sb, err := openSQLSandbox(ctx, p.main, projectID)

		p.mu.Lock()
		delete(p.opening, projectID)
		if err == nil {
			sb.pool = p
			sb.lease()
			p.sandboxes[projectID] = sb
			p.lru = append([]string{projectID}, p.lru...)
			p.evictLocked()
		}
		op.err = err
		close(op.done)
		p.mu.Unlock()
		return sb, err
	}
}

// evictLocked closes LRU sandboxes past the cap. A sandbox with an active
// lease (refs > 0) is skipped — its releaser re-checks the cap. Callers hold
// p.mu.
func (p *sqlSandboxPool) evictLocked() {
	for len(p.lru) > sandboxMaxProjects {
		victim := p.lru[len(p.lru)-1]
		sb, ok := p.sandboxes[victim]
		if !ok {
			p.lru = p.lru[:len(p.lru)-1]
			continue
		}
		if sb.refs.Load() > 0 {
			// In flight — try the next-oldest instead.
			if len(p.lru) == 1 {
				return
			}
			// Rotate it to the front so it isn't picked again this pass.
			p.lru = append([]string{victim}, p.lru[:len(p.lru)-1]...)
			// If every sandbox is leased, stop.
			allLeased := true
			for _, id := range p.lru {
				if s, ok := p.sandboxes[id]; ok && s.refs.Load() == 0 {
					allLeased = false
					break
				}
			}
			if allLeased {
				return
			}
			continue
		}
		p.lru = p.lru[:len(p.lru)-1]
		delete(p.sandboxes, victim)
		// Close outside the pool lock would be nicer, but close() takes the
		// sandbox mutex — a leased sandbox is never here, so the only waiter
		// is a refresh/run that already holds refs > 0. Safe to close inline.
		sb.close()
	}
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
	pool      *sqlSandboxPool
	db        *sql.DB
	// feeder is the connection refresh writes through; runner is the
	// connection every untrusted query executes on. Both live on the same
	// locked-down instance — the split exists so a refresh and a query never
	// share a connection.
	feeder *sql.Conn
	runner *sql.Conn
	// tmpDir is this sandbox's private spill directory.
	tmpDir string
	// refs counts in-flight refresh/run pairs; eviction skips leased
	// sandboxes so a query can never observe a closed instance.
	refs   atomic.Int64
	mu     sync.Mutex
	closed bool
}

func (sb *sqlSandbox) lease() { sb.refs.Add(1) }

func (sb *sqlSandbox) release() {
	if sb.refs.Add(-1) != 0 || sb.pool == nil {
		return
	}
	// evictLocked may have deferred enforcement while every sandbox was
	// leased. Re-check as soon as one becomes idle so the pool returns to its
	// configured bound without waiting for another cold project.
	sb.pool.mu.Lock()
	sb.pool.evictLocked()
	sb.pool.mu.Unlock()
}

// openSQLSandbox creates the in-memory instance, builds the project-scoped
// tables and views, and locks the whole instance down before any data lands.
func openSQLSandbox(ctx context.Context, main *DuckDB, projectID string) (*sqlSandbox, error) {
	// Each sandbox gets its own spill dir — DuckDB names temp files
	// duckdb_temp_storage-* and two instances sharing one directory collide.
	tmpDir := ""
	if base := main.tmpDir(); base != "" {
		tmpDir = filepath.Join(base, "sandbox-"+projectID)
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			return nil, fmt.Errorf("sandbox tmp dir: %w", err)
		}
	}
	connector, err := duckdb.NewConnector("", func(execer driver.ExecerContext) error {
		stmts := []string{"SET TimeZone = 'UTC'"}
		if tmpDir != "" {
			stmts = append(stmts,
				"SET temp_directory = '"+strings.ReplaceAll(tmpDir, "'", "''")+"'",
				"SET max_temp_directory_size = '"+sandboxTempSize+"'")
		}
		for _, stmt := range stmts {
			if _, err := execer.ExecContext(context.Background(), stmt, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)

	sb := &sqlSandbox{projectID: projectID, main: main, db: db, tmpDir: tmpDir}
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
// Events are append-only, so the copy is incremental on (inserted_at,
// event_id); aliases and external_rows are small enough to reconcile
// wholesale. A row removed from the main file is removed here too — the
// count check keeps the common refresh cheap.
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

// refreshEvents appends only what arrived since the last refresh. The cursor
// is (inserted_at, event_id) — a bare inserted_at high-water mark skips rows
// that share the timestamp of the last copied row. A delete in the main file —
// the retention sweep does exactly that — shows up as the sandbox holding more
// rows than the main file has at or below that cursor, and rebuilds the table.
func (sb *sqlSandbox) refreshEvents(ctx context.Context) error {
	var highWater time.Time
	var highWaterID string
	if err := sb.feeder.QueryRowContext(ctx,
		`SELECT coalesce(max(inserted_at), TIMESTAMPTZ '1970-01-01') FROM events`).Scan(&highWater); err != nil {
		return err
	}
	if err := sb.feeder.QueryRowContext(ctx,
		`SELECT coalesce(max(event_id::VARCHAR), '') FROM events WHERE inserted_at = ?`, highWater).Scan(&highWaterID); err != nil {
		return err
	}
	var sandboxCount, mainAtOrBelowHighWater int64
	if err := sb.feeder.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&sandboxCount); err != nil {
		return err
	}
	if err := sb.main.Read(ctx, func(conn *sql.Conn) error {
		// Count only what the sandbox should already hold — everything at or
		// below its cursor. Comparing against the whole table instead lets an
		// equal number of newly arrived rows hide a delete: the sweep removing
		// D events while D new ones land leaves the two totals agreeing, and
		// the sandbox keeps answering queries with rows the main store deleted.
		return conn.QueryRowContext(ctx,
			`SELECT count(*) FROM events WHERE project_id = ?
			 AND (inserted_at < ? OR (inserted_at = ? AND event_id::VARCHAR <= ?))`,
			sb.projectID, highWater, highWater, highWaterID).Scan(&mainAtOrBelowHighWater)
	}); err != nil {
		return err
	}
	if sandboxCount > mainAtOrBelowHighWater {
		// Rows vanished from the main file; rebuild rather than probe per row.
		if _, err := sb.feeder.ExecContext(ctx, `DELETE FROM events`); err != nil {
			return err
		}
		highWater = time.Time{}
		highWaterID = ""
	}
	return sb.copyRows(ctx,
		`SELECT * FROM events WHERE project_id = ?
		 AND (inserted_at > ? OR (inserted_at = ? AND event_id::VARCHAR > ?))
		 ORDER BY inserted_at, event_id`,
		[]any{sb.projectID, highWater, highWater, highWaterID},
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
	results := []map[string]any{}
	for rows.Next() {
		if len(results) >= sandboxMaxRows {
			return nil, fmt.Errorf("query exceeded %d rows; add a LIMIT", sandboxMaxRows)
		}
		// Scan into *any: the driver's ScanType() reports the primitive type
		// without nullability, so a NULL in a VARCHAR column scanned into
		// *string errors. *any takes whatever the driver hands back.
		valuePtrs := make([]any, len(columns))
		for i := range columns {
			valuePtrs[i] = new(any)
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}
		item := make(map[string]any, len(columns))
		for i, column := range columns {
			item[column] = normalizeSQLValue(*(valuePtrs[i].(*any)))
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
	if sb.tmpDir != "" {
		_ = os.RemoveAll(sb.tmpDir)
	}
}

// sandboxSchema mirrors the analytics tables inside a sandbox. The project_id
// columns stay: the scoped CTEs still filter on them, and keeping the column
// makes the sandbox schema a strict subset of the main one.
var sandboxSchema = []string{
	// Column-for-column mirror of the main events table — refresh copies
	// SELECT *, so order and count must match exactly.
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
		cursor VARCHAR,
		data VARCHAR NOT NULL DEFAULT '{}',
		synced_at TIMESTAMPTZ NOT NULL,
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
