package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// This file is the DuckDB write edge: every captured batch lands here inside
// one gated transaction.
// The dedup contract is the events PRIMARY KEY — (project_id, event_id) —
// enforced by INSERT OR IGNORE, so a redelivered JetStream batch re-commits as
// a no-op instead of double-counting.

// InsertEvents durably stores one batch of events. Idempotent on
// (project_id, event_id): a replayed batch inserts nothing and returns nil.
// Person projection is NOT applied here — this is the raw-event sink used by
// pipeline self-metrics; the ingest worker's path is SinkEvents.
func (d *DuckDB) InsertEvents(ctx context.Context, events []Event) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		return insertEventsTx(ctx, tx, events)
	})
}

// insertEventsChunk is how many rows one statement carries. The write path
// used to execute one prepared INSERT per event, and DuckDB holds a row-group
// sized buffer per statement inside the transaction until commit: measured at
// ~1.1 MB per event on this 27-column table (the engine failed at row 108 of a
// 500-row batch under a 128 MB memory_limit, and a 384 MiB container would have
// gone down the same way with no sandbox query in play). One statement per
// chunk keeps the transaction's memory proportional to the chunk instead.
const insertEventsChunk = 256

// insertEventsChunkBytes is the other half of that bound: a row count alone
// says nothing when the rows are fat (tool_output and properties are arbitrary
// text), so a chunk is also cut when its values reach this size.
const insertEventsChunkBytes = 4 << 20

// eventValueBytes estimates one event's contribution to a statement. It is an
// estimate on purpose — the point is to stop a chunk from growing without
// bound, not to predict DuckDB's allocation.
func eventValueBytes(e Event) int {
	n := len(e.Properties) + len(e.ToolInput) + len(e.ToolOutput) + len(e.ErrorMessage) +
		len(e.DistinctID) + len(e.SessionID) + len(e.EventName) + len(e.EventType) +
		len(e.AgentID) + len(e.ToolName) + len(e.ModelName) + len(e.BotName) +
		len(e.ReferrerHost) + len(e.UserAgent) + len(e.InsertID) + len(e.Platform) +
		len(e.VisitorClass) + len(e.ReferrerChannel)
	return n + 64
}

// insertEventsTx writes the batch as chunked multi-row INSERT OR IGNORE
// statements: the (project_id, event_id) primary key stays the dedup contract,
// and the transaction still covers the whole batch.
func insertEventsTx(ctx context.Context, tx *sql.Tx, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	const cols = 27
	row := placeholders(cols)
	prefix := `INSERT OR IGNORE INTO events (
	project_id, event_id, distinct_id, session_id, event_name, event_type,
	properties, agent_id, tool_name, tool_input, tool_output, tokens_input,
	tokens_output, cost_usd, latency_ms, model_name, is_error, error_message,
	"timestamp", visitor_class, bot_name, referrer_host, referrer_channel,
	user_agent, insert_id, is_unplanned, platform
) VALUES `
	for start := 0; start < len(events); {
		end, bytes := start, 0
		for end < len(events) && end-start < insertEventsChunk {
			size := eventValueBytes(events[end])
			if end > start && bytes+size > insertEventsChunkBytes {
				break
			}
			bytes += size
			end++
		}
		chunk := events[start:end]
		start = end
		args := make([]any, 0, len(chunk)*cols)
		for _, event := range chunk {
			projectID, err := uuid.Parse(event.ProjectID)
			if err != nil {
				return fmt.Errorf("event %q: project_id: %w", event.EventID, err)
			}
			eventID, err := uuid.Parse(event.EventID)
			if err != nil {
				return fmt.Errorf("event_id %q: event_id: %w", event.EventID, err)
			}
			args = append(args,
				projectID,
				eventID,
				event.DistinctID,
				event.SessionID,
				event.EventName,
				event.EventType,
				event.Properties,
				nullableString(event.AgentID),
				nullableString(event.ToolName),
				nullableString(event.ToolInput),
				nullableString(event.ToolOutput),
				event.TokensInput,
				event.TokensOutput,
				event.CostUSD,
				event.LatencyMS,
				nullableString(event.ModelName),
				event.IsError,
				nullableString(event.ErrorMessage),
				event.Timestamp.UTC(),
				event.VisitorClass,
				nullableString(event.BotName),
				nullableString(event.ReferrerHost),
				event.ReferrerChannel,
				nullableString(event.UserAgent),
				nullableString(event.InsertID),
				event.IsUnplanned,
				event.Platform,
			)
		}
		stmt := prefix + strings.TrimSuffix(strings.Repeat(row+",", len(chunk)), ",")
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

// SinkEvents is the ingest worker's durable write: the coalesced events AND
// their person-profile projection commit in one transaction, so the batcher's
// ack means both halves are durable. A crash before commit rolls both back and
// the message redelivers; a crash after commit but before ack redelivers into
// the (project_id, event_id) dedup key and the person merge re-applies
// idempotently (mergePersonDelta's freshness guard makes the re-fold a no-op).
//
// The person projection commits in the same transaction as the events, so the
// profile can no longer be lost once the batch is acked.
//
// mark is the durable position this batch lands; it commits in the SAME
// transaction, which is what lets /readyz compare the durable's floor against
// what this file can show it applied (see duckdb_position.go). An empty mark is
// a write that did not come off the durable stream.
func (d *DuckDB) SinkEvents(ctx context.Context, events []Event, mark AppliedMark) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		if err := insertEventsTx(ctx, tx, events); err != nil {
			return err
		}
		if err := d.applyPersonUpdatesTx(ctx, tx, events); err != nil {
			return err
		}
		return advancePositionTx(ctx, tx, mark)
	})
}

// applyPersonUpdatesTx folds the batch's identity traits into persons inside
// the caller's transaction. Canonical-id resolution reads the DuckDB aliases
// mirror (not Postgres) so the projection never leaves the write transaction.
func (d *DuckDB) applyPersonUpdatesTx(ctx context.Context, tx *sql.Tx, events []Event) error {
	// Load the alias map for every project the batch touches, up front: the
	// resolver closure then answers in-memory like the old Postgres-backed
	// resolver did. A failed read is fatal — folding a profile under a raw
	// distinct_id would silently split one person into two rows.
	aliasMaps := map[string]map[string]string{}
	for _, e := range events {
		if _, ok := aliasMaps[e.ProjectID]; ok {
			continue
		}
		m, err := loadAliasMap(ctx, tx, e.ProjectID)
		if err != nil {
			return err
		}
		aliasMaps[e.ProjectID] = m
	}
	resolve := func(projectID, distinctID string) string {
		if canon, ok := aliasMaps[projectID][distinctID]; ok {
			return canon
		}
		return distinctID
	}
	deltas := extractPersonDeltas(events, resolve)
	if len(deltas) == 0 {
		return nil
	}
	byProject := map[string][]string{}
	for k := range deltas {
		byProject[k.projectID] = append(byProject[k.projectID], k.distinctID)
	}
	// One statement per identity, like the events insert used to be, would hold
	// a row-group-sized buffer per person until commit — a 500-identity batch
	// is the same shape that exhausted the engine's memory limit. Chunked
	// multi-row upserts instead.
	const personCols = 9
	personRow := placeholders(personCols)
	personPrefix := `INSERT INTO persons (project_id, distinct_id, properties, properties_once, email, name, first_seen, last_seen, version) VALUES `
	personSuffix := `
ON CONFLICT (project_id, distinct_id) DO UPDATE SET
	properties = excluded.properties,
	properties_once = excluded.properties_once,
	email = excluded.email,
	name = excluded.name,
	first_seen = excluded.first_seen,
	last_seen = excluded.last_seen,
	version = excluded.version`
	for projectID, ids := range byProject {
		existing, err := personProfilesByKeysTx(ctx, tx, projectID, ids)
		if err != nil {
			return err
		}
		pid, err := uuid.Parse(projectID)
		if err != nil {
			return err
		}
		for start := 0; start < len(ids); start += insertEventsChunk {
			chunk := ids[start:min(start+insertEventsChunk, len(ids))]
			args := make([]any, 0, len(chunk)*personCols)
			for _, id := range chunk {
				d := deltas[personKey{projectID: projectID, distinctID: id}]
				merged := mergePersonDelta(existing[id], d)
				args = append(args,
					pid,
					id,
					marshalTraitMap(merged.SetProps),
					marshalTraitMap(merged.OnceProps),
					merged.Email,
					merged.Name,
					merged.FirstSeen.UTC(),
					merged.LastSeen.UTC(),
					uint64(merged.LastSeen.UnixMilli()),
				)
			}
			stmt := personPrefix + strings.TrimSuffix(strings.Repeat(personRow+",", len(chunk)), ",") + personSuffix
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadAliasMap reads one project's anonymous→canonical alias pairs inside the
// caller's transaction. The rows are drained and closed before returning, so
// the transaction is free for the next statement.
func loadAliasMap(ctx context.Context, tx *sql.Tx, projectID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT anonymous_id, canonical_id FROM aliases WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var anon, canon string
		if err := rows.Scan(&anon, &canon); err != nil {
			return nil, err
		}
		m[anon] = canon
	}
	return m, rows.Err()
}

// duckQueryer is the query surface shared by *sql.Tx (inside the write gate)
// and *sql.Conn (inside the read gate) so person reads work on either.
type duckQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// personProfilesByKeysTx reads merged profiles for a set of canonical ids in
// one project. persons is a plain table keyed by (project_id, distinct_id), so
// this is a point lookup — no FINAL collapse like the old ReplacingMergeTree.
func personProfilesByKeysTx(ctx context.Context, q duckQueryer, projectID string, distinctIDs []string) (map[string]*personRow, error) {
	out := map[string]*personRow{}
	if len(distinctIDs) == 0 {
		return out, nil
	}
	args := []any{projectID}
	for _, id := range distinctIDs {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, `
SELECT distinct_id, properties, properties_once, email, name, first_seen, last_seen
FROM persons
WHERE project_id = ? AND distinct_id IN `+placeholders(len(distinctIDs)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, props, once, email, name string
			firstSeen, lastSeen          time.Time
		)
		if err := rows.Scan(&id, &props, &once, &email, &name, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		out[id] = &personRow{
			SetProps:  parseTraitMap(props),
			OnceProps: parseTraitMap(once),
			Email:     email,
			Name:      name,
			FirstSeen: firstSeen,
			LastSeen:  lastSeen,
		}
	}
	return out, rows.Err()
}

// PersonProfilesByKeys reads merged profiles for a set of canonical ids in one
// project through the read gate. Callers resolve canonical ids first (Postgres
// is the alias source of truth).
func (d *DuckDB) PersonProfilesByKeys(ctx context.Context, projectID string, distinctIDs []string) (map[string]*personRow, error) {
	var out map[string]*personRow
	err := d.Read(ctx, func(conn *sql.Conn) error {
		var err error
		out, err = personProfilesByKeysTx(ctx, conn, projectID, distinctIDs)
		return err
	})
	return out, err
}

// UpsertAliases mirrors one or more Postgres alias writes into the DuckDB
// aliases table. Called after the Postgres commit in CreateAlias; idempotent
// by the (project_id, anonymous_id) primary key.
func (d *DuckDB) UpsertAliases(ctx context.Context, rows [][3]string) error {
	if len(rows) == 0 {
		return nil
	}
	return d.Write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR REPLACE INTO aliases (project_id, anonymous_id, canonical_id) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			if _, err := stmt.ExecContext(ctx, r[0], r[1], r[2]); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReconcileAliases makes the DuckDB mirror exactly match the Postgres source
// of truth: delete-then-insert inside one transaction so a restart never
// leaves a stale stitch behind. Postgres aliases are never deleted today, but
// the mirror must not depend on that staying true.
func (d *DuckDB) ReconcileAliases(ctx context.Context, rows [][3]string) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM aliases`); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO aliases (project_id, anonymous_id, canonical_id) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			if _, err := stmt.ExecContext(ctx, r[0], r[1], r[2]); err != nil {
				return err
			}
		}
		return nil
	})
}

// InsertExternalRows lands one connector batch. The (project, connector,
// table, row_key) primary key makes snapshot re-syncs and retried batches
// idempotent — the job ReplacingMergeTree(synced_at) did, enforced at write
// time instead of merge time. mark lands in the same transaction, as it does
// for events: one durable consumer carries both subjects, so the position it
// records covers both (see duckdb_position.go).
func (d *DuckDB) InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow, mark AppliedMark) error {
	if len(rows) == 0 {
		// An empty batch still settles its message, and settling advances the
		// consumer's ack floor: record the position so the file does not fall
		// behind a floor built of deliveries that carried no rows at all.
		return d.RecordPosition(ctx, mark)
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return err
	}
	cid, err := uuid.Parse(connectorID)
	if err != nil {
		return err
	}
	return d.Write(ctx, func(tx *sql.Tx) error {
		// Chunked multi-row statements, like the events insert: one statement
		// per row keeps a row-group-sized buffer alive until commit, and a
		// connector batch is up to 1,000 rows.
		const cols = 7
		row := placeholders(cols)
		prefix := `INSERT OR REPLACE INTO external_rows (project_id, connector_id, table_name, row_key, cursor, data, synced_at) VALUES `
		now := time.Now().UTC()
		for start := 0; start < len(rows); start += insertEventsChunk {
			chunk := rows[start:min(start+insertEventsChunk, len(rows))]
			args := make([]any, 0, len(chunk)*cols)
			for _, r := range chunk {
				args = append(args, pid, cid, table, r.Key, r.Cursor, r.DataJSON, now)
			}
			stmt := prefix + strings.TrimSuffix(strings.Repeat(row+",", len(chunk)), ",")
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return err
			}
		}
		return advancePositionTx(ctx, tx, mark)
	})
}
