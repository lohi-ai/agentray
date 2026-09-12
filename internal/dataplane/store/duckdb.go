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
// everything the old ClickHouse write path owned.
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
const DuckDBSchemaVersion = 1

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
	connector, err := duckdb.NewConnector(path, func(execer driver.ExecerContext) error {
		_, err := execer.ExecContext(context.Background(),
			"SET temp_directory = '"+strings.ReplaceAll(tmpDir, "'", "''")+"'", nil)
		return err
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

// migrate creates the v1 schema. Every statement is idempotent so a restart
// over an existing file is a no-op; schema_meta records the version so a later
// schema generation can detect what it is opening.
func (d *DuckDB) migrate(ctx context.Context) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range duckDBSchema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("duckdb schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_meta (name, version) VALUES ('schema', ?)`,
			DuckDBSchemaVersion); err != nil {
			return fmt.Errorf("duckdb schema version: %w", err)
		}
		return nil
	})
}

// duckDBSchema is the v1 analytics schema. It mirrors the ClickHouse tables it
// replaces column-for-column where the product contract reads them, with two
// deliberate upgrades the embedded engine makes cheap:
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
		PRIMARY KEY (project_id, event_id)
	)`,
	// aliases mirrors the Postgres source of truth (reconciled at boot,
	// upserted on write). resolved_events joins through it for canonical-id
	// stitching — the job the ClickHouse aliases_dict dictionary did.
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
