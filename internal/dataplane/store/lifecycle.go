package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Dashboard lifecycle + write idempotency (redesign slice 2). Dashboards gain
// a monotonically increasing revision for optimistic concurrency and a soft
// archived_at — archive is reversible and never deletes charts. Retryable
// writes across the operation surface record receipts in idempotency_keys so
// a retried mutation returns its first result instead of applying twice.

// ErrRevisionConflict is returned when an expected-revision check fails: the
// row changed under the caller. Distinct from "not found" so a client can
// re-read and retry rather than assume the resource is gone.
var ErrRevisionConflict = errors.New("revision conflict: resource changed")

// ErrIdempotencyConflict is returned when an idempotency key is reused with a
// different request payload.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")

func (s *Store) migrateLifecycle(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE dashboards ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1`,
		`ALTER TABLE dashboards ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`,
		`CREATE TABLE IF NOT EXISTS idempotency_keys (
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	operation VARCHAR(64) NOT NULL,
	idempotency_key VARCHAR(128) NOT NULL,
	request_hash VARCHAR(64) NOT NULL,
	result JSONB,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (project_id, operation, idempotency_key)
)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

const dashboardColumns = `id::text, project_id::text, name, description, revision, archived_at, created_at, updated_at`

func dashboardScanDest(d *Dashboard) []any {
	return []any{&d.ID, &d.ProjectID, &d.Name, &d.Description, &d.Revision, &d.ArchivedAt, &d.CreatedAt, &d.UpdatedAt}
}

// ListDashboardsFiltered lists the project's dashboards; includeArchived=false
// omits archived rows (the operation default), true returns everything.
func (s *Store) ListDashboardsFiltered(ctx context.Context, projectID string, includeArchived bool) ([]Dashboard, error) {
	query := `SELECT ` + dashboardColumns + ` FROM dashboards WHERE project_id = $1`
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.pg.Query(ctx, query, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dashboards := []Dashboard{}
	for rows.Next() {
		var d Dashboard
		if err := rows.Scan(dashboardScanDest(&d)...); err != nil {
			return nil, err
		}
		dashboards = append(dashboards, d)
	}
	return dashboards, rows.Err()
}

// updateDashboardRevision applies a name/description update only when the
// row's revision still equals expectedRevision — the optimistic-concurrency
// check that makes a concurrent pair yield exactly one success. The revision
// increments once per applied update. Runs on any pgQuerier so the idempotent
// claim wrapper can execute it inside the claim transaction.
func updateDashboardRevision(ctx context.Context, q pgQuerier, projectID, dashboardID, name, description string, expectedRevision int64) (Dashboard, error) {
	if name == "" {
		name = "Untitled dashboard"
	}
	var d Dashboard
	err := q.QueryRow(ctx, `
UPDATE dashboards
SET name = $3, description = $4, revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND revision = $5 AND archived_at IS NULL
RETURNING `+dashboardColumns, projectID, dashboardID, name, description, expectedRevision).
		Scan(dashboardScanDest(&d)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish "gone" from "changed under you" so the client knows
			// whether to retry or give up.
			var exists bool
			if qerr := q.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM dashboards WHERE project_id = $1 AND id = $2)`,
				projectID, dashboardID).Scan(&exists); qerr == nil && exists {
				return Dashboard{}, ErrRevisionConflict
			}
			return Dashboard{}, pgx.ErrNoRows
		}
		return Dashboard{}, err
	}
	return d, nil
}

// UpdateDashboardRevision is the non-transactional edge for callers that do
// not need an idempotency receipt.
func (s *Store) UpdateDashboardRevision(ctx context.Context, projectID, dashboardID, name, description string, expectedRevision int64) (Dashboard, error) {
	return updateDashboardRevision(ctx, s.pg, projectID, dashboardID, name, description, expectedRevision)
}

// archiveDashboard soft-archives a dashboard: sets archived_at without
// deleting the row or its charts. The FIRST mutation requires the current
// revision; an already-archived row returns its current state untouched (a
// repeat is a no-op, not a revision bump).
func archiveDashboard(ctx context.Context, q pgQuerier, projectID, dashboardID string, expectedRevision int64) (Dashboard, error) {
	var d Dashboard
	err := q.QueryRow(ctx, `
UPDATE dashboards
SET archived_at = now(), revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND archived_at IS NULL AND revision = $3
RETURNING `+dashboardColumns, projectID, dashboardID, expectedRevision).
		Scan(dashboardScanDest(&d)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already archived → idempotent repeat: return the row as-is.
			var existing Dashboard
			if qerr := q.QueryRow(ctx,
				`SELECT `+dashboardColumns+` FROM dashboards WHERE project_id = $1 AND id = $2`,
				projectID, dashboardID).Scan(dashboardScanDest(&existing)...); qerr != nil {
				return Dashboard{}, pgx.ErrNoRows
			} else if existing.ArchivedAt != nil {
				return existing, nil
			}
			return Dashboard{}, ErrRevisionConflict
		}
		return Dashboard{}, err
	}
	return d, nil
}

// ArchiveDashboard is the non-transactional edge for callers that do not need
// an idempotency receipt.
func (s *Store) ArchiveDashboard(ctx context.Context, projectID, dashboardID string, expectedRevision int64) (Dashboard, error) {
	return archiveDashboard(ctx, s.pg, projectID, dashboardID, expectedRevision)
}

// runIdempotent executes mutate under an atomic claim on
// (project, operation, key): the claim row, the mutation, and the result
// receipt commit in ONE transaction, so a crash mid-operation leaves no
// half-applied write and no orphaned claim — a retry simply re-claims.
//
// Concurrency: a second caller inserting the same key blocks on the winner's
// uncommitted row, then reads the committed receipt — it never runs its own
// mutation. Same payload replays the stored result; a different payload under
// the same key is ErrIdempotencyConflict. An empty key skips the machinery
// entirely (the mutation runs once, unrecorded).
func (s *Store) runIdempotent(ctx context.Context, projectID, operation, key, requestHash string, mutate func(ctx context.Context, q pgQuerier) (json.RawMessage, error)) (json.RawMessage, error) {
	if key == "" {
		return mutate(ctx, s.pg)
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var claimed string
	err = tx.QueryRow(ctx, `
INSERT INTO idempotency_keys (project_id, operation, idempotency_key, request_hash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, operation, idempotency_key) DO NOTHING
RETURNING request_hash`, projectID, operation, key, requestHash).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another transaction holds or held this key. Its row is committed by
		// the time our INSERT unblocks, so the receipt is either complete or
		// the winner rolled back (row gone — but then our insert would have
		// succeeded). Read what won.
		var storedHash string
		var stored []byte
		if qerr := s.pg.QueryRow(ctx, `
SELECT request_hash, result FROM idempotency_keys
WHERE project_id = $1 AND operation = $2 AND idempotency_key = $3`,
			projectID, operation, key).Scan(&storedHash, &stored); qerr != nil {
			return nil, qerr
		}
		if storedHash != requestHash {
			return nil, ErrIdempotencyConflict
		}
		if stored == nil {
			return nil, fmt.Errorf("idempotent claim %s/%s is incomplete — retry", operation, key)
		}
		return stored, nil
	}
	if err != nil {
		return nil, err
	}

	result, err := mutate(ctx, tx)
	if err != nil {
		return nil, err // rollback drops the claim AND the mutation together
	}
	if _, err := tx.Exec(ctx, `
UPDATE idempotency_keys SET result = $5
WHERE project_id = $1 AND operation = $2 AND idempotency_key = $3 AND request_hash = $4`,
		projectID, operation, key, requestHash, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateDashboardIdempotent is updateDashboardRevision under an idempotency
// claim: one transaction claims the key, mutates, and records the receipt.
func (s *Store) UpdateDashboardIdempotent(ctx context.Context, projectID, dashboardID, name, description string, expectedRevision int64, idemKey, requestHash string) (Dashboard, error) {
	raw, err := s.runIdempotent(ctx, projectID, "update_dashboard", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			d, err := updateDashboardRevision(ctx, q, projectID, dashboardID, name, description, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(d)
		})
	if err != nil {
		return Dashboard{}, err
	}
	var d Dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		return Dashboard{}, fmt.Errorf("stored dashboard receipt unreadable: %w", err)
	}
	return d, nil
}

// ArchiveDashboardIdempotent is archiveDashboard under an idempotency claim.
func (s *Store) ArchiveDashboardIdempotent(ctx context.Context, projectID, dashboardID string, expectedRevision int64, idemKey, requestHash string) (Dashboard, error) {
	raw, err := s.runIdempotent(ctx, projectID, "archive_dashboard", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			d, err := archiveDashboard(ctx, q, projectID, dashboardID, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(d)
		})
	if err != nil {
		return Dashboard{}, err
	}
	var d Dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		return Dashboard{}, fmt.Errorf("stored dashboard receipt unreadable: %w", err)
	}
	return d, nil
}

// DistinctIDLinked reports whether an explicit identify/alias link exists for
// the distinct id — the honest answer to "is this event's actor identified",
// since SDK anonymous id shapes vary and cannot be sniffed by prefix.
func (s *Store) DistinctIDLinked(ctx context.Context, projectID, distinctID string) (bool, error) {
	var linked bool
	err := s.pg.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM aliases WHERE project_id = $1 AND anonymous_id = $2)`,
		projectID, distinctID).Scan(&linked)
	return linked, err
}

// RecentEventsForVerification is the bounded event read verify_sdk rides on —
// the same RecentEvents path the analytics surface uses, scoped to the
// project, capped small. Declared separately so the operation's contract is
// visible in one place.
func (s *Store) RecentEventsForVerification(ctx context.Context, projectID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	return s.RecentEvents(ctx, projectID, limit)
}
