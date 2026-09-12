package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// This file is the DuckDB write edge: everything that used to land in
// ClickHouse through PrepareBatch lands here inside one gated transaction.
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

func insertEventsTx(ctx context.Context, tx *sql.Tx, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT OR IGNORE INTO events (
	project_id, event_id, distinct_id, session_id, event_name, event_type,
	properties, agent_id, tool_name, tool_input, tool_output, tokens_input,
	tokens_output, cost_usd, latency_ms, model_name, is_error, error_message,
	"timestamp", visitor_class, bot_name, referrer_host, referrer_channel,
	user_agent, insert_id, is_unplanned, platform
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, event := range events {
		projectID, err := uuid.Parse(event.ProjectID)
		if err != nil {
			return fmt.Errorf("event %q: project_id: %w", event.EventID, err)
		}
		eventID, err := uuid.Parse(event.EventID)
		if err != nil {
			return fmt.Errorf("event_id %q: %w", event.EventID, err)
		}
		if _, err := stmt.ExecContext(ctx,
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
		); err != nil {
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
// This replaces the old two-phase shape (ClickHouse insert, then a best-effort
// background applier that could drop a saturated hand-off): the profile can no
// longer be lost once the batch is acked.
func (d *DuckDB) SinkEvents(ctx context.Context, events []Event) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		if err := insertEventsTx(ctx, tx, events); err != nil {
			return err
		}
		return d.applyPersonUpdatesTx(ctx, tx, events)
	})
}

// applyPersonUpdatesTx folds the batch's identity traits into persons inside
// the caller's transaction. Canonical-id resolution reads the DuckDB aliases
// mirror (not Postgres) so the projection never leaves the write transaction.
func (d *DuckDB) applyPersonUpdatesTx(ctx context.Context, tx *sql.Tx, events []Event) error {
	// Load the alias map once per project touched by the batch; the resolver
	// closure then answers in-memory like the old Postgres-backed resolver did.
	projects := map[string]struct{}{}
	for _, e := range events {
		projects[e.ProjectID] = struct{}{}
	}
	aliasMaps := map[string]map[string]string{}
	loadAliases := func(projectID string) map[string]string {
		m, ok := aliasMaps[projectID]
		if !ok {
			m = map[string]string{}
			rows, err := tx.QueryContext(ctx,
				`SELECT anonymous_id, canonical_id FROM aliases WHERE project_id = ?`, projectID)
			if err == nil {
				for rows.Next() {
					var anon, canon string
					if rows.Scan(&anon, &canon) == nil {
						m[anon] = canon
					}
				}
				_ = rows.Close()
			}
			aliasMaps[projectID] = m
		}
		return m
	}
	resolve := func(projectID, distinctID string) string {
		if canon, ok := loadAliases(projectID)[distinctID]; ok {
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
	upsert, err := tx.PrepareContext(ctx, `
INSERT INTO persons (project_id, distinct_id, properties, properties_once, email, name, first_seen, last_seen, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, distinct_id) DO UPDATE SET
	properties = excluded.properties,
	properties_once = excluded.properties_once,
	email = excluded.email,
	name = excluded.name,
	first_seen = excluded.first_seen,
	last_seen = excluded.last_seen,
	version = excluded.version`)
	if err != nil {
		return err
	}
	defer upsert.Close()
	for projectID, ids := range byProject {
		existing, err := personProfilesByKeysTx(ctx, tx, projectID, ids)
		if err != nil {
			return err
		}
		pid, err := uuid.Parse(projectID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			d := deltas[personKey{projectID: projectID, distinctID: id}]
			merged := mergePersonDelta(existing[id], d)
			if _, err := upsert.ExecContext(ctx,
				pid,
				id,
				marshalTraitMap(merged.SetProps),
				marshalTraitMap(merged.OnceProps),
				merged.Email,
				merged.Name,
				merged.FirstSeen.UTC(),
				merged.LastSeen.UTC(),
				uint64(merged.LastSeen.UnixMilli()),
			); err != nil {
				return err
			}
		}
	}
	return nil
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
WHERE project_id = ? AND distinct_id IN (`+placeholders(len(distinctIDs))+`)`, args...)
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
// time instead of merge time.
func (d *DuckDB) InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow) error {
	if len(rows) == 0 {
		return nil
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
		stmt, err := tx.PrepareContext(ctx, `
INSERT OR REPLACE INTO external_rows (project_id, connector_id, table_name, row_key, cursor, data, synced_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		now := time.Now().UTC()
		for _, r := range rows {
			if _, err := stmt.ExecContext(ctx, pid, cid, table, r.Key, r.Cursor, r.DataJSON, now); err != nil {
				return err
			}
		}
		return nil
	})
}
