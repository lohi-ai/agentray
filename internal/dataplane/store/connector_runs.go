package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// Persistent connector-run contract (redesign slice 2). One row per run with
// queued → running → succeeded/failed/cancelled, an idempotency key, and a
// cancel flag — replacing the engine's process-local running map as the
// client-visible contract. At most one active (queued|running) run per sync,
// enforced by a partial unique index, so concurrent enqueues can never
// double-run a sync.

// ConnectorRun is one durable sync-run record; the type lives in the
// connector package so the engine's Store interface can name it without
// importing storage.
type ConnectorRun = connector.Run

const connectorRunColumns = `id::text, project_id::text, sync_id::text, connector_id::text,
	status, idempotency_key, cancel_requested, rows, cursor, cursor_key, error,
	queued_at, started_at, finished_at`

func connectorRunScanDest(r *ConnectorRun) []any {
	return []any{&r.ID, &r.ProjectID, &r.SyncID, &r.ConnectorID, &r.Status,
		&r.IdempotencyKey, &r.CancelRequested, &r.Rows, &r.Cursor, &r.CursorKey,
		&r.Error, &r.QueuedAt, &r.StartedAt, &r.FinishedAt}
}

func (s *Store) migrateConnectorRuns(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS connector_runs (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	sync_id UUID NOT NULL REFERENCES connector_syncs(id) ON DELETE CASCADE,
	connector_id UUID NOT NULL REFERENCES data_connectors(id) ON DELETE CASCADE,
	status VARCHAR(16) NOT NULL DEFAULT 'queued',
	idempotency_key VARCHAR(128) NOT NULL DEFAULT '',
	cancel_requested BOOLEAN NOT NULL DEFAULT false,
	rows INT NOT NULL DEFAULT 0,
	cursor TEXT NOT NULL DEFAULT '',
	cursor_key TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	owner TEXT NOT NULL DEFAULT '',
	heartbeat_at TIMESTAMPTZ,
	queued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	started_at TIMESTAMPTZ,
	finished_at TIMESTAMPTZ
)`,
		`ALTER TABLE connector_runs ADD COLUMN IF NOT EXISTS owner TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE connector_runs ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ`,
		// One active run per sync — the DB-level guarantee that replaces the
		// engine's in-memory running map for the client contract.
		`CREATE UNIQUE INDEX IF NOT EXISTS connector_runs_one_active
	ON connector_runs (sync_id) WHERE status IN ('queued','running')`,
		// Idempotent enqueue: same (sync, key) returns the same run.
		`CREATE UNIQUE INDEX IF NOT EXISTS connector_runs_idem
	ON connector_runs (sync_id, idempotency_key) WHERE idempotency_key <> ''`,
		`CREATE INDEX IF NOT EXISTS connector_runs_project_idx
	ON connector_runs (project_id, queued_at DESC)`,
		// Sync rows gain a revision for the same optimistic-concurrency
		// contract dashboards have (pause/update carry the expected revision).
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// ErrSyncPaused rejects enqueue on a paused (disabled) sync.
var ErrSyncPaused = errors.New("sync is paused")

// EnqueueConnectorRun inserts a queued run for the sync. The enabled check is
// INSIDE the insert — a pause landing between check and insert cannot slip a
// run through. An identical idempotency key replays the original run row; an
// already-active run returns it with enqueued=false (at most one active run
// per sync — the second caller observes, it does not pile on).
func (s *Store) EnqueueConnectorRun(ctx context.Context, projectID, syncID, idemKey string) (run ConnectorRun, enqueued bool, err error) {
	err = s.pg.QueryRow(ctx, `
INSERT INTO connector_runs (project_id, sync_id, connector_id, idempotency_key)
SELECT $1, $2, connector_id, $3 FROM connector_syncs
WHERE id = $2 AND project_id = $1 AND enabled
ON CONFLICT DO NOTHING
RETURNING `+connectorRunColumns, projectID, syncID, idemKey).
		Scan(connectorRunScanDest(&run)...)
	if err == nil {
		return run, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ConnectorRun{}, false, err
	}
	// No row inserted: idempotency replay, an active run, a paused sync, or a
	// missing sync — in that order of precedence. Every fallback is scoped to
	// THIS project: a foreign sync id must never return another tenant's run
	// metadata, and a read error fails closed rather than falling through.
	if idemKey != "" {
		var existing ConnectorRun
		if qerr := s.pg.QueryRow(ctx,
			`SELECT `+connectorRunColumns+` FROM connector_runs WHERE sync_id = $1 AND project_id = $2 AND idempotency_key = $3`,
			syncID, projectID, idemKey).Scan(connectorRunScanDest(&existing)...); qerr == nil {
			return existing, false, nil
		} else if !errors.Is(qerr, pgx.ErrNoRows) {
			return ConnectorRun{}, false, qerr
		}
	}
	var active ConnectorRun
	if qerr := s.pg.QueryRow(ctx,
		`SELECT `+connectorRunColumns+` FROM connector_runs WHERE sync_id = $1 AND project_id = $2 AND status IN ('queued','running')`,
		syncID, projectID).Scan(connectorRunScanDest(&active)...); qerr != nil {
		if !errors.Is(qerr, pgx.ErrNoRows) {
			return ConnectorRun{}, false, qerr
		}
		var enabled bool
		if qerr2 := s.pg.QueryRow(ctx,
			`SELECT enabled FROM connector_syncs WHERE id = $1 AND project_id = $2`,
			syncID, projectID).Scan(&enabled); errors.Is(qerr2, pgx.ErrNoRows) {
			return ConnectorRun{}, false, pgx.ErrNoRows
		} else if qerr2 != nil {
			return ConnectorRun{}, false, qerr2
		} else if !enabled {
			return ConnectorRun{}, false, ErrSyncPaused
		}
		return ConnectorRun{}, false, fmt.Errorf("run conflict but no active run found: %w", qerr)
	}
	return active, false, nil
}

// ClaimConnectorRun moves a queued run to running under the caller's owner
// identity and starts the heartbeat lease. Returns false when the row is not
// queued anymore — already claimed, finished, or cancel_requested (a cancel
// that landed while queued wins: the run ends cancelled without starting).
func (s *Store) ClaimConnectorRun(ctx context.Context, runID, owner string) (ConnectorRun, bool, error) {
	var r ConnectorRun
	err := s.pg.QueryRow(ctx, `
UPDATE connector_runs
SET status = 'running', started_at = now(), owner = $2, heartbeat_at = now()
WHERE id = $1 AND status = 'queued' AND NOT cancel_requested
RETURNING `+connectorRunColumns, runID, owner).Scan(connectorRunScanDest(&r)...)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "cancelled while queued" from "claimed by someone else".
		var cancelled bool
		if qerr := s.pg.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM connector_runs WHERE id = $1 AND status = 'queued' AND cancel_requested)`,
			runID).Scan(&cancelled); qerr == nil && cancelled {
			_, _ = s.pg.Exec(ctx,
				`UPDATE connector_runs SET status = 'cancelled', finished_at = now() WHERE id = $1 AND status = 'queued'`, runID)
		}
		return ConnectorRun{}, false, nil
	}
	if err != nil {
		return ConnectorRun{}, false, err
	}
	return r, true, nil
}

// FinishConnectorRun persists the terminal outcome on the run row and folds
// the same result into the sync's last_* columns — one transaction, so a
// client reading either sees a consistent end state. Only a running row
// still owned by the caller can finish: a fenced (reconciled) or cancelled
// row refuses, so a stale owner cannot overwrite the terminal state a peer
// already wrote.
func (s *Store) FinishConnectorRun(ctx context.Context, runID, syncID, owner string, result connector.SyncResult, cancelled bool) error {
	status := "succeeded"
	errText := result.Err
	switch {
	case cancelled:
		status = "cancelled"
	case errText != "":
		status = "failed"
		if len(errText) > 500 {
			errText = errText[:500]
		}
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
UPDATE connector_runs
SET status = $2, rows = $3, cursor = $4, cursor_key = $5, error = $6, finished_at = now()
WHERE id = $1 AND status = 'running' AND owner = $7`, runID, status, result.Rows, result.Cursor, result.CursorKey, errText, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %s is not running under this owner — finished, cancelled, or fenced", runID)
	}
	legacyStatus := "ok"
	if status != "succeeded" {
		legacyStatus = "error"
	}
	if _, err := tx.Exec(ctx, `
UPDATE connector_syncs SET
	cursor = CASE WHEN $2 THEN $3 ELSE cursor END,
	cursor_key = CASE WHEN $2 THEN $4 ELSE cursor_key END,
	last_run_at = now(), last_status = $5, last_error = $6, last_rows = $7::int,
	total_rows = total_rows + $7::bigint, revision = revision + 1, updated_at = now()
WHERE id = $1`, syncID, result.AdvanceCursor, result.Cursor, result.CursorKey, legacyStatus, errText, result.Rows); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// HeartbeatConnectorRun renews a running row's lease and reports whether
// cancellation was requested — one round trip per tick keeps the liveness
// fence fresh AND observes cross-process cancels. Returns false when the row
// is no longer running (finished/cancelled/reconciled underneath us).
func (s *Store) HeartbeatConnectorRun(ctx context.Context, runID string) (cancelRequested bool, stillRunning bool, err error) {
	err = s.pg.QueryRow(ctx, `
UPDATE connector_runs SET heartbeat_at = now()
WHERE id = $1 AND status = 'running'
RETURNING cancel_requested`, runID).Scan(&cancelRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return cancelRequested, true, err
}

// CancelConnectorRun flags a run for cancellation. Queued runs end cancelled
// immediately; running runs get cancel_requested and the worker observes it
// (in-process cancel func, or the heartbeat for a worker in another process).
// Terminal rows are returned unchanged — cancel cannot rewrite history.
func (s *Store) CancelConnectorRun(ctx context.Context, projectID, runID string) (ConnectorRun, error) {
	var r ConnectorRun
	err := s.pg.QueryRow(ctx, `
UPDATE connector_runs
SET cancel_requested = true,
    status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
    finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END
WHERE id = $1 AND project_id = $2 AND status IN ('queued','running')
RETURNING `+connectorRunColumns, runID, projectID).Scan(connectorRunScanDest(&r)...)
	if errors.Is(err, pgx.ErrNoRows) {
		// Terminal or absent: return the row as-is (idempotent cancel) or
		// not-found when the id does not exist in this project at all.
		var existing ConnectorRun
		if qerr := s.pg.QueryRow(ctx,
			`SELECT `+connectorRunColumns+` FROM connector_runs WHERE id = $1 AND project_id = $2`,
			runID, projectID).Scan(connectorRunScanDest(&existing)...); qerr != nil {
			return ConnectorRun{}, pgx.ErrNoRows
		} else {
			return existing, nil
		}
	}
	if err != nil {
		return ConnectorRun{}, err
	}
	return r, nil
}

// ConnectorRunForProject reads one run scoped to the project — an id from
// another tenant is not-found, never a leak.
func (s *Store) ConnectorRunForProject(ctx context.Context, projectID, runID string) (ConnectorRun, error) {
	var r ConnectorRun
	err := s.pg.QueryRow(ctx,
		`SELECT `+connectorRunColumns+` FROM connector_runs WHERE id = $1 AND project_id = $2`,
		runID, projectID).Scan(connectorRunScanDest(&r)...)
	return r, err
}

// LatestConnectorRun returns the sync's most recent run, for source_status.
func (s *Store) LatestConnectorRun(ctx context.Context, projectID, syncID string) (ConnectorRun, error) {
	var r ConnectorRun
	err := s.pg.QueryRow(ctx,
		`SELECT `+connectorRunColumns+` FROM connector_runs WHERE sync_id = $1 AND project_id = $2 ORDER BY queued_at DESC LIMIT 1`,
		syncID, projectID).Scan(connectorRunScanDest(&r)...)
	return r, err
}

// ReconcileConnectorRuns runs at boot and fails only runs whose lease proves
// the owner is gone: running rows with a heartbeat older than staleBefore,
// and queued rows older than staleBefore that no worker ever claimed. A live
// process's runs have fresh heartbeats and survive another process's boot —
// the unique-active guard is never freed under a live owner.
func (s *Store) ReconcileConnectorRuns(ctx context.Context, staleBefore time.Time) (int, error) {
	tag, err := s.pg.Exec(ctx, `
UPDATE connector_runs
SET status = 'failed', error = 'process restarted before the run finished', finished_at = now()
WHERE (status = 'running' AND (heartbeat_at IS NULL OR heartbeat_at < $1))
   OR (status = 'queued' AND queued_at < $1)`, staleBefore)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// --- project-scoped connector reads/writes for the operation layer ---
// The REST routes authorize via user→workspace role; the operation layer's
// principal already carries that check, so these take a project directly.

// ConnectorDSNForProject resolves kind + decrypted DSN for a project-scoped
// connector. The DSN stays inside the process — callers probe, never return it.
func (s *Store) ConnectorDSNForProject(ctx context.Context, projectID, connectorID string) (kind, dsn string, err error) {
	var ciphertext string
	err = s.pg.QueryRow(ctx,
		`SELECT kind, dsn_ciphertext FROM data_connectors WHERE id = $1 AND project_id = $2`,
		connectorID, projectID).Scan(&kind, &ciphertext)
	if err != nil {
		return "", "", err
	}
	dsn, err = decryptAgentKey(ciphertext)
	if err != nil {
		return "", "", err
	}
	return kind, dsn, nil
}

// ListDataConnectorsForProject lists a project's connectors (no DSN material).
func (s *Store) ListDataConnectorsForProject(ctx context.Context, projectID string) ([]DataConnector, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, kind, dsn_ciphertext <> '', created_at, updated_at
FROM data_connectors WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DataConnector{}
	for rows.Next() {
		var c DataConnector
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.HasDSN, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListConnectorSyncsForProject lists one connector's syncs, project-scoped.
func (s *Store) ListConnectorSyncsForProject(ctx context.Context, projectID, connectorID string) ([]ConnectorSync, error) {
	rows, err := s.pg.Query(ctx, `
SELECT `+connectorSyncColumns+`, revision
FROM connector_syncs WHERE project_id = $1 AND connector_id = $2 ORDER BY created_at`, projectID, connectorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConnectorSync{}
	for rows.Next() {
		var cs ConnectorSync
		dest := append(syncScanDest(&cs), &cs.Revision)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// ConnectorSyncForProject reads one sync, project-scoped.
func (s *Store) ConnectorSyncForProject(ctx context.Context, projectID, syncID string) (ConnectorSync, error) {
	var cs ConnectorSync
	dest := append(syncScanDest(&cs), &cs.Revision)
	err := s.pg.QueryRow(ctx,
		`SELECT `+connectorSyncColumns+`, revision FROM connector_syncs WHERE id = $1 AND project_id = $2`,
		syncID, projectID).Scan(dest...)
	return cs, err
}

// SetConnectorSyncEnabledIdempotent is SetConnectorSyncEnabled under an
// idempotency claim — a retried pause with the original revision replays the
// first result instead of conflicting on the bumped revision.
func (s *Store) SetConnectorSyncEnabledIdempotent(ctx context.Context, projectID, syncID string, enabled bool, expectedRevision int64, idemKey, requestHash string) (ConnectorSync, error) {
	raw, err := s.runIdempotent(ctx, projectID, "pause_source", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			var cs ConnectorSync
			dest := append(syncScanDest(&cs), &cs.Revision)
			err := q.QueryRow(ctx, `
UPDATE connector_syncs SET enabled = $3, revision = revision + 1, updated_at = now()
WHERE id = $1 AND project_id = $2 AND revision = $4
RETURNING `+connectorSyncColumns+`, revision`, syncID, projectID, enabled, expectedRevision).Scan(dest...)
			if errors.Is(err, pgx.ErrNoRows) {
				var exists bool
				if qerr := q.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM connector_syncs WHERE id = $1 AND project_id = $2)`,
					syncID, projectID).Scan(&exists); qerr == nil && exists {
					return nil, ErrRevisionConflict
				}
				return nil, pgx.ErrNoRows
			}
			if err != nil {
				return nil, err
			}
			return json.Marshal(cs)
		})
	if err != nil {
		return ConnectorSync{}, err
	}
	var cs ConnectorSync
	if err := json.Unmarshal(raw, &cs); err != nil {
		return ConnectorSync{}, fmt.Errorf("stored sync receipt unreadable: %w", err)
	}
	return cs, nil
}

// SetConnectorSyncEnabled pauses/resumes a sync with a revision check —
// pause_source's mutation. Pausing never kills an active run; it only stops
// future enqueue (EnqueueConnectorRun refuses disabled syncs).
func (s *Store) SetConnectorSyncEnabled(ctx context.Context, projectID, syncID string, enabled bool, expectedRevision int64) (ConnectorSync, error) {
	var cs ConnectorSync
	dest := append(syncScanDest(&cs), &cs.Revision)
	err := s.pg.QueryRow(ctx, `
UPDATE connector_syncs SET enabled = $3, revision = revision + 1, updated_at = now()
WHERE id = $1 AND project_id = $2 AND revision = $4
RETURNING `+connectorSyncColumns+`, revision`, syncID, projectID, enabled, expectedRevision).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if qerr := s.pg.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM connector_syncs WHERE id = $1 AND project_id = $2)`,
			syncID, projectID).Scan(&exists); qerr == nil && exists {
			return ConnectorSync{}, ErrRevisionConflict
		}
		return ConnectorSync{}, pgx.ErrNoRows
	}
	return cs, err
}
