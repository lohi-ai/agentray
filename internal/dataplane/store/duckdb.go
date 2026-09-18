package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// DuckDB is the embedded analytics engine: one process-local database file
// holding the event log, the alias mirror, person profiles, and connector
// landing rows. PostgreSQL remains the control-plane store; this type owns
// the entire analytics write path.
//
// Concurrency: DuckDB is single-writer MVCC. Write serializes every mutation
// through a one-slot gate; Read admits up to maxDuckDBReaders snapshot readers.
// The *sql.DB pool is capped at 1+maxDuckDBReaders so a queued reader can never
// starve the writer of a physical connection. There is deliberately no
// *sql.DB accessor: every caller goes through Read/Write so admission stays
// bounded and the file is never touched outside this type.
type DuckDB struct {
	db      *sql.DB
	path    string
	writeCh chan struct{}
	readCh  chan struct{}
	closed  atomic.Bool
}

// maxDuckDBReaders bounds concurrent snapshot readers. Four is enough for the
// dashboard's parallel tiles without letting a query burst pin the file.
const maxDuckDBReaders = 4

// DuckDBSchemaVersion is the schema generation OpenDuckDB stamps into
// schema_meta. Bump it when the DDL below changes so a boot can tell a
// foundation-era file from a later one.
const DuckDBSchemaVersion = 3

// Table and view names exposed for the query-parity ticket (007): reads are
// ported against these names so the DDL and its consumers cannot drift.
const (
	DuckDBEventsName         = "events"
	DuckDBAliasesName        = "aliases"
	DuckDBPersonsName        = "persons"
	DuckDBExternalRowsName   = "external_rows"
	DuckDBResolvedEventsName = "resolved_events"
	DuckDBSessionsName       = "sessions"
)

// errDuckDBClosed is returned by Read/Write after Close. Callers treat it like
// any other engine error (the ingest batcher NAKs, which is correct: the
// process is going away and redelivery is moot).
var errDuckDBClosed = errors.New("duckdb: database is closed")

// OpenDuckDB opens (creating if needed) the embedded database at path and
// brings it to DuckDBSchemaVersion. The parent directory and its tmp/
// spill directory are created; the WAL lives at <path>.wal, managed by DuckDB.
//
// path must name a file. ":memory:" is rejected on purpose: each pooled
// connection would get its own empty database, so the reader pool would see
// nothing the writer committed.
func OpenDuckDB(ctx context.Context, path string) (*DuckDB, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return nil, fmt.Errorf("duckdb: path must name a database file, got %q", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("duckdb: create data dir: %w", err)
	}
	tmpDir := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("duckdb: create tmp dir: %w", err)
	}
	// temp_directory is per-connection, so it goes through the connector's init
	// hook: every pooled connection (writer and readers alike) spills to the
	// directory beside the database file rather than the process cwd.
	// TimeZone=UTC pins TIMESTAMPTZ bucketing (date_trunc, INTERVAL math) to
	// UTC regardless of the host's ICU zone — host-local bucketing would
	// shift every timeline/date boundary on a non-UTC deployment.
	connector, err := duckdb.NewConnector(path, func(execer driver.ExecerContext) error {
		for _, stmt := range []string{
			"SET temp_directory = '" + strings.ReplaceAll(tmpDir, "'", "''") + "'",
			// The cgroup is the real ceiling, but DuckDB does not know that: with
			// no limit of its own it treats the whole container as its budget.
			// This instance is the trusted writer and the dashboard reader, so it
			// gets its share of the envelope and spills the rest to disk — see
			// the sandbox budget in duckdb_sandbox.go.
			"SET memory_limit = '" + sandboxMainMemoryLimit + "'",
			"SET max_temp_directory_size = '" + sandboxMainTempSize + "'",
			"SET TimeZone = 'UTC'",
		} {
			if _, err := execer.ExecContext(context.Background(), stmt, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("duckdb: connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1 + maxDuckDBReaders)
	d := &DuckDB{
		db:      db,
		path:    path,
		writeCh: make(chan struct{}, 1),
		readCh:  make(chan struct{}, maxDuckDBReaders),
	}
	if err := d.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Path reports the configured database file this instance owns.
func (d *DuckDB) Path() string { return d.path }

// tmpDir returns the spill directory beside the database file, created by
// OpenDuckDB. Sandboxes point their temp_directory at it so a big GROUP BY
// spills to bounded disk rather than process memory.
func (d *DuckDB) tmpDir() string {
	return filepath.Join(filepath.Dir(d.path), "tmp")
}

// Write runs fn inside the single-writer gate on one transaction. fn receives
// the open *sql.Tx; a nil return commits, an error rolls back. The gate is
// context-aware so a caller whose deadline passes while queued fails instead
// of waiting behind the current writer.
func (d *DuckDB) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if d.closed.Load() {
		return errDuckDBClosed
	}
	select {
	case d.writeCh <- struct{}{}:
		defer func() { <-d.writeCh }()
	case <-ctx.Done():
		return ctx.Err()
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Read runs fn on a pooled connection under the reader gate (at most
// maxDuckDBReaders concurrent). Each call gets an MVCC snapshot: a reader
// never sees a half-committed ingest batch.
func (d *DuckDB) Read(ctx context.Context, fn func(conn *sql.Conn) error) error {
	if d.closed.Load() {
		return errDuckDBClosed
	}
	select {
	case d.readCh <- struct{}{}:
		defer func() { <-d.readCh }()
	case <-ctx.Done():
		return ctx.Err()
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn)
}

// Close checkpoints the WAL into the database file and closes the pool. It
// waits for the writer slot so an in-flight ingest transaction commits before
// the file closes; readers already admitted finish on their own connections.
// Idempotent.
func (d *DuckDB) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	// Take the writer slot so no transaction is mid-commit when the pool
	// closes, then force a checkpoint so the WAL is folded into the file.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case d.writeCh <- struct{}{}:
		_, _ = d.db.ExecContext(ctx, "CHECKPOINT")
		<-d.writeCh
	case <-ctx.Done():
	}
	return d.db.Close()
}

// Checkpoint takes the writer slot and folds the write-ahead log into the
// database file. Deleting rows only marks them; the checkpoint is what lets
// the freed blocks be reused, so the retention sweep runs it after a delete
// that removed anything.
func (d *DuckDB) Checkpoint(ctx context.Context) error {
	if d.closed.Load() {
		return errDuckDBClosed
	}
	select {
	case d.writeCh <- struct{}{}:
		defer func() { <-d.writeCh }()
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err := d.db.ExecContext(ctx, "CHECKPOINT")
	return err
}

// migrate creates the schema. Every statement is idempotent so a restart over
// an existing file is a no-op; schema_meta records the version so a later
// schema generation can detect what it is opening.
//
// The version is stamped, not merely inserted: a file created by an older build
// gains the new DDL on this boot, and a ledger still saying "1" would describe a
// schema it no longer has — the one reading the version exists to give.
func (d *DuckDB) migrate(ctx context.Context) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range duckDBSchema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("duckdb schema: %w", err)
			}
		}
		// utm_* columns are NOT NULL in the schema but arrive via ADD COLUMN,
		// which DuckDB cannot do with the constraint attached. Tighten only the
		// ones still nullable: ALTER ... SET NOT NULL writes a WAL entry that
		// crashes duckdb-go 2.10505's replay on the next open, so running it
		// unconditionally would brick every restart.
		rows, err := tx.QueryContext(ctx,
			`SELECT column_name FROM information_schema.columns
			 WHERE table_name = 'events' AND is_nullable = 'YES'
			   AND column_name IN ('utm_source','utm_medium','utm_campaign','utm_term','utm_content')`)
		if err != nil {
			return fmt.Errorf("duckdb schema: %w", err)
		}
		var nullable []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return fmt.Errorf("duckdb schema: %w", err)
			}
			nullable = append(nullable, name)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("duckdb schema: %w", err)
		}
		for _, col := range nullable {
			if _, err := tx.ExecContext(ctx,
				`ALTER TABLE events ALTER COLUMN `+col+` SET NOT NULL`); err != nil {
				return fmt.Errorf("duckdb schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_meta (name, version) VALUES ('schema', ?)`,
			DuckDBSchemaVersion); err != nil {
			return fmt.Errorf("duckdb schema version: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE schema_meta SET version = ? WHERE name = 'schema' AND version <> ?`,
			DuckDBSchemaVersion, DuckDBSchemaVersion); err != nil {
			return fmt.Errorf("duckdb schema version: %w", err)
		}
		return nil
	})
}

// duckDBSchema is the analytics schema: the event log, alias mirror, person
// profiles, connector landing rows, and the ingest position record, with two
// properties the embedded engine makes cheap:
//   - events has a real PRIMARY KEY on (project_id, event_id): the ingest
//     dedup contract is enforced by the engine, not by merge-time luck.
//   - persons is a plain transactional table (read-merge-write inside the
//     ingest transaction) instead of a ReplacingMergeTree that needed FINAL.
var duckDBSchema = []string{
	`CREATE TABLE IF NOT EXISTS schema_meta (
		name VARCHAR PRIMARY KEY,
		version INTEGER NOT NULL
	)`,
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
		utm_source VARCHAR NOT NULL DEFAULT '',
		utm_medium VARCHAR NOT NULL DEFAULT '',
		utm_campaign VARCHAR NOT NULL DEFAULT '',
		utm_term VARCHAR NOT NULL DEFAULT '',
		utm_content VARCHAR NOT NULL DEFAULT '',
		PRIMARY KEY (project_id, event_id)
	)`,
	// Column additions for files created before the UTM tags existed: CREATE
	// TABLE IF NOT EXISTS leaves an existing events table alone, so the same
	// three columns are added here idempotently. DuckDB cannot ADD COLUMN with
	// NOT NULL in one statement, so each column lands nullable-with-default
	// (existing rows backfill to '') and the constraint is applied after —
	// both statements are safe on a file that already has the column.
	// The sandbox schema (duckdb_sandbox.go) mirrors this column order
	// exactly — its refresh copies SELECT *, so the two lists must never drift.
	// Column additions for files created before the UTM tags existed: CREATE
	// TABLE IF NOT EXISTS leaves an existing events table alone, so the same
	// columns are added here idempotently. DuckDB cannot ADD COLUMN with
	// NOT NULL in one statement, so each column lands nullable-with-default
	// (existing rows backfill to '') and the constraint is applied after.
	// The SET NOT NULL itself is gated on the column still being nullable:
	// replaying an ALTER ... SET NOT NULL WAL entry crashes duckdb-go
	// 2.10505 on open, so the statement must only run when it changes
	// something — an unconditional one poisons every restart.
	// The sandbox schema (duckdb_sandbox.go) mirrors this column order
	// exactly — its refresh copies SELECT *, so the two lists must never drift.
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS utm_source VARCHAR DEFAULT ''`,
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS utm_medium VARCHAR DEFAULT ''`,
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS utm_campaign VARCHAR DEFAULT ''`,
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS utm_term VARCHAR DEFAULT ''`,
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS utm_content VARCHAR DEFAULT ''`,
	// aliases mirrors the Postgres source of truth (reconciled at boot,
	// upserted on write). resolved_events joins through it for canonical-id
	// stitching.
	`CREATE TABLE IF NOT EXISTS aliases (
		project_id UUID NOT NULL,
		anonymous_id VARCHAR NOT NULL,
		canonical_id VARCHAR NOT NULL,
		PRIMARY KEY (project_id, anonymous_id)
	)`,
	// persons is the merged trait profile, one row per (project, canonical id),
	// maintained inside the ingest transaction. $set is last-write-wins,
	// $set_once first-write-wins; version is last_seen in ms for debugging.
	`CREATE TABLE IF NOT EXISTS persons (
		project_id UUID NOT NULL,
		distinct_id VARCHAR NOT NULL,
		properties VARCHAR NOT NULL DEFAULT '{}',
		properties_once VARCHAR NOT NULL DEFAULT '{}',
		email VARCHAR NOT NULL DEFAULT '',
		name VARCHAR NOT NULL DEFAULT '',
		first_seen TIMESTAMPTZ,
		last_seen TIMESTAMPTZ,
		version UBIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (project_id, distinct_id)
	)`,
	// external_rows is the connector landing table: one JSON row per source
	// row, keyed so a re-sync replaces rather than duplicates.
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
	// ingest_position is the store-side half of the readiness contract: how far
	// this file's own writes have carried it along the durable stream, and the
	// gap a boot proved the file can never fill. It lives INSIDE the file the
	// claim is about, so a store that lost its volume cannot inherit a warm
	// durable's floor (see duckdb_position.go).
	`CREATE TABLE IF NOT EXISTS ingest_position (
		durable VARCHAR PRIMARY KEY,
		applied_seq UBIGINT NOT NULL DEFAULT 0,
		refused_missing UBIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// resolved_events is the canonical-identity view every person-scoped read
	// uses: raw distinct_id plus its stitched canonical id (self when no alias
	// exists). Replaces the dictGet(aliases_dict) expression.
	`CREATE VIEW IF NOT EXISTS resolved_events AS
	SELECT e.*, coalesce(a.canonical_id, e.distinct_id) AS canonical_distinct_id
	FROM events e
	LEFT JOIN aliases a
		ON a.project_id = e.project_id AND a.anonymous_id = e.distinct_id`,
	// sessions is the ingest-derived session rollup: events carry a session_id
	// minted by the 30-minute-gap sessionizer (or the client), and this view
	// aggregates them — the job sessions_mv did as a materialized view.
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
