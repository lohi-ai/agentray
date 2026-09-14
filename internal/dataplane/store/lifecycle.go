package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// ErrSourceArchived rejects a mutation that would reactivate work under an
// archived connector — resume its syncs by unarchiving the source instead.
var ErrSourceArchived = errors.New("source is archived")

func (s *Store) migrateLifecycle(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE dashboards ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1`,
		`ALTER TABLE dashboards ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`,
		// Charts get the same contract: a per-chart revision fences update and
		// archive, and archived_at makes chart removal reversible. Existing
		// rows stay active (archived_at NULL) with revision 1.
		`ALTER TABLE charts ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1`,
		`ALTER TABLE charts ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`,
		// Source archive is reversible: the connector keeps its row and
		// credential reference, and its syncs are disabled transactionally.
		// disabled_by_archive remembers WHICH syncs the archive paused so
		// unarchive resumes exactly those — a sync the operator paused by hand
		// stays paused.
		`ALTER TABLE data_connectors ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`,
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS disabled_by_archive BOOLEAN NOT NULL DEFAULT false`,
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

const dashboardColumns = `id::text, project_id::text, name, description, revision, archived_at, created_at, updated_at, board_key, definition_updated_at`

func dashboardScanDest(d *Dashboard) []any {
	return []any{&d.ID, &d.ProjectID, &d.Name, &d.Description, &d.Revision, &d.ArchivedAt, &d.CreatedAt, &d.UpdatedAt, &d.BoardKey, &d.DefinitionUpdatedAt}
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

// DashboardForProject reads one dashboard, project-scoped, archived rows
// included — the adapter's revision lookup for callers that do not send one.
func (s *Store) DashboardForProject(ctx context.Context, projectID, dashboardID string) (Dashboard, error) {
	var d Dashboard
	err := s.pg.QueryRow(ctx,
		`SELECT `+dashboardColumns+` FROM dashboards WHERE project_id = $1 AND id = $2`,
		projectID, dashboardID).Scan(dashboardScanDest(&d)...)
	return d, err
}

// updateDashboardRevision applies a partial name/description update only when
// the row's revision still equals expectedRevision — the optimistic-
// concurrency check that makes a concurrent pair yield exactly one success.
// nil name/description leave the column untouched; an explicit "" clears the
// description, while an empty name falls back to "Untitled dashboard" (a
// dashboard is never nameless). The revision increments once per applied
// update. Runs on any pgQuerier so the idempotent claim wrapper can execute
// it inside the claim transaction.
func updateDashboardRevision(ctx context.Context, q pgQuerier, projectID, dashboardID string, name, description *string, expectedRevision int64) (Dashboard, error) {
	var d Dashboard
	err := q.QueryRow(ctx, `
UPDATE dashboards
SET name = CASE WHEN $3::text IS NULL THEN name
                WHEN $3 = '' THEN 'Untitled dashboard'
                ELSE $3 END,
    description = COALESCE($4::text, description),
    revision = revision + 1, updated_at = now()
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
func (s *Store) UpdateDashboardRevision(ctx context.Context, projectID, dashboardID string, name, description *string, expectedRevision int64) (Dashboard, error) {
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

// UnarchiveDashboard reverses a soft archive: clears archived_at under the
// same revision contract. An already-active row returns its current state
// untouched — a repeat is a no-op, not a revision bump.
func (s *Store) UnarchiveDashboard(ctx context.Context, projectID, dashboardID string, expectedRevision int64) (Dashboard, error) {
	return unarchiveDashboard(ctx, s.pg, projectID, dashboardID, expectedRevision)
}

func unarchiveDashboard(ctx context.Context, q pgQuerier, projectID, dashboardID string, expectedRevision int64) (Dashboard, error) {
	var d Dashboard
	err := q.QueryRow(ctx, `
UPDATE dashboards
SET archived_at = NULL, revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND archived_at IS NOT NULL AND revision = $3
RETURNING `+dashboardColumns, projectID, dashboardID, expectedRevision).
		Scan(dashboardScanDest(&d)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var existing Dashboard
			if qerr := q.QueryRow(ctx,
				`SELECT `+dashboardColumns+` FROM dashboards WHERE project_id = $1 AND id = $2`,
				projectID, dashboardID).Scan(dashboardScanDest(&existing)...); qerr != nil {
				return Dashboard{}, pgx.ErrNoRows
			} else if existing.ArchivedAt == nil {
				return existing, nil
			}
			return Dashboard{}, ErrRevisionConflict
		}
		return Dashboard{}, err
	}
	return d, nil
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
		// succeeded). Read what won THROUGH THE SAME TX — under READ COMMITTED
		// the next statement sees the winner's commit, and using the pool here
		// would deadlock at MaxConns=1 (this tx still holds its connection).
		var storedHash string
		var stored []byte
		if qerr := tx.QueryRow(ctx, `
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
func (s *Store) UpdateDashboardIdempotent(ctx context.Context, projectID, dashboardID string, name, description *string, expectedRevision int64, idemKey, requestHash string) (Dashboard, error) {
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

// UnarchiveDashboardIdempotent is UnarchiveDashboard under an idempotency
// claim — a retried restore replays the first receipt instead of mutating
// again after a re-archive.
func (s *Store) UnarchiveDashboardIdempotent(ctx context.Context, projectID, dashboardID string, expectedRevision int64, idemKey, requestHash string) (Dashboard, error) {
	raw, err := s.runIdempotent(ctx, projectID, "unarchive_dashboard", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			d, err := unarchiveDashboard(ctx, q, projectID, dashboardID, expectedRevision)
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
// Both directions count: the id may appear as the anonymous side (identify()
// linked it forward) or as the canonical side (the SDK sends the canonical id
// directly after linking).
func (s *Store) DistinctIDLinked(ctx context.Context, projectID, distinctID string) (bool, error) {
	var linked bool
	err := s.pg.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM aliases WHERE project_id = $1 AND (anonymous_id = $2 OR canonical_id = $2))`,
		projectID, distinctID).Scan(&linked)
	return linked, err
}

// RecentEventsForVerification is the bounded event read verify_sdk rides on.
// Verification asks "did my event ARRIVE", so it filters and orders on
// inserted_at (receipt time), not the client-supplied occurred timestamp —
// an offline device can deliver an event whose timestamp is days old.
func (s *Store) RecentEventsForVerification(ctx context.Context, projectID string, limit int, since time.Time) ([]Event, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	events := []Event{}
	err := s.duckQuery(ctx, `
SELECT
	project_id::VARCHAR, event_id::VARCHAR, distinct_id, session_id, event_name,
	event_type, properties, is_error, "timestamp", inserted_at, platform
FROM events
WHERE project_id = ? AND inserted_at >= ?
ORDER BY inserted_at DESC
LIMIT ?`, []any{projectID, since, limit}, func(rows *sql.Rows) error {
		var event Event
		var isError bool
		var inserted time.Time
		if err := rows.Scan(
			&event.ProjectID, &event.EventID, &event.DistinctID, &event.SessionID,
			&event.EventName, &event.EventType, &event.Properties, &isError,
			&event.Timestamp, &inserted, &event.Platform,
		); err != nil {
			return err
		}
		event.IsError = isError
		event.InsertedAt = &inserted
		events = append(events, event)
		return nil
	})
	return events, err
}

// --- chart lifecycle ---

// withTx runs fn inside a transaction: when q is already a pgx.Tx (the
// idempotent-claim transaction) it is used directly; otherwise a fresh
// transaction is begun on q — the no-key path runs on the pool and still
// needs atomicity for multi-statement mutations.
func withTx(ctx context.Context, q pgQuerier, fn func(tx pgx.Tx) error) error {
	if tx, ok := q.(pgx.Tx); ok {
		return fn(tx)
	}
	b, ok := q.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("withTx: querier cannot begin a transaction")
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const chartColumns = `id::text, dashboard_id::text, project_id::text, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order, col_span, revision, archived_at, created_at, updated_at`

func chartScanDest(c *Chart) []any {
	return []any{&c.ID, &c.DashboardID, &c.ProjectID, &c.Name, &c.Kind, &c.Metric, &c.EventName, &c.EventType, &c.SQL, &c.XField, &c.YField, &c.SortOrder, &c.ColSpan, &c.Revision, &c.ArchivedAt, &c.CreatedAt, &c.UpdatedAt}
}

// ListChartsFiltered lists one dashboard's charts; includeArchived=false omits
// archived rows (the operation default), true returns everything.
func (s *Store) ListChartsFiltered(ctx context.Context, projectID, dashboardID string, includeArchived bool) ([]Chart, error) {
	q := `
SELECT ` + chartColumns + `
FROM charts
WHERE project_id = $1 AND dashboard_id = $2`
	if !includeArchived {
		q += ` AND archived_at IS NULL`
	}
	q += ` ORDER BY sort_order ASC, created_at ASC`
	rows, err := s.pg.Query(ctx, q, projectID, dashboardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	charts := []Chart{}
	for rows.Next() {
		var c Chart
		if err := rows.Scan(chartScanDest(&c)...); err != nil {
			return nil, err
		}
		charts = append(charts, c)
	}
	return charts, rows.Err()
}

// ChartForProject reads one chart, project-scoped, archived rows included —
// the adapter's revision lookup for callers that do not send one.
func (s *Store) ChartForProject(ctx context.Context, projectID, chartID string) (Chart, error) {
	var c Chart
	err := s.pg.QueryRow(ctx,
		`SELECT `+chartColumns+` FROM charts WHERE project_id = $1 AND id = $2`,
		projectID, chartID).Scan(chartScanDest(&c)...)
	return c, err
}

// updateChartRevision applies a full-field chart update only when the row's
// revision still equals expectedRevision — the same optimistic-concurrency
// contract dashboards carry. Runs on any pgQuerier so the idempotent claim
// wrapper can execute it inside the claim transaction.
func updateChartRevision(ctx context.Context, q pgQuerier, chart Chart, expectedRevision int64) (Chart, error) {
	if chart.Name == "" {
		chart.Name = "Untitled chart"
	}
	if chart.Kind == "" {
		chart.Kind = "line"
	}
	if chart.Metric == "" {
		chart.Metric = "events"
	}
	chart.ColSpan = clampSpan(chart.ColSpan)
	var c Chart
	err := q.QueryRow(ctx, `
UPDATE charts
SET name = $3, kind = $4, metric = $5, event_name = $6, event_type = $7, sql = $8, x_field = $9, y_field = $10, col_span = $11,
    revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND revision = $12 AND archived_at IS NULL
RETURNING `+chartColumns,
		chart.ProjectID, chart.ID, chart.Name, chart.Kind, chart.Metric, chart.EventName, chart.EventType, chart.SQL, chart.XField, chart.YField, chart.ColSpan, expectedRevision).
		Scan(chartScanDest(&c)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if qerr := q.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM charts WHERE project_id = $1 AND id = $2)`,
				chart.ProjectID, chart.ID).Scan(&exists); qerr == nil && exists {
				return Chart{}, ErrRevisionConflict
			}
			return Chart{}, pgx.ErrNoRows
		}
		return Chart{}, err
	}
	return c, nil
}

// UpdateChartRevision is the non-transactional edge for callers that do not
// need an idempotency receipt.
func (s *Store) UpdateChartRevision(ctx context.Context, chart Chart, expectedRevision int64) (Chart, error) {
	return updateChartRevision(ctx, s.pg, chart, expectedRevision)
}

// UpdateChartIdempotent is updateChartRevision under an idempotency claim.
func (s *Store) UpdateChartIdempotent(ctx context.Context, chart Chart, expectedRevision int64, idemKey, requestHash string) (Chart, error) {
	raw, err := s.runIdempotent(ctx, chart.ProjectID, "update_chart", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			c, err := updateChartRevision(ctx, q, chart, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return Chart{}, err
	}
	var c Chart
	if err := json.Unmarshal(raw, &c); err != nil {
		return Chart{}, fmt.Errorf("stored chart receipt unreadable: %w", err)
	}
	return c, nil
}

// archiveChart soft-archives a chart: sets archived_at without deleting the
// row. The FIRST mutation requires the current revision; an already-archived
// row returns its current state untouched (a repeat is a no-op).
func archiveChart(ctx context.Context, q pgQuerier, projectID, chartID string, expectedRevision int64) (Chart, error) {
	var c Chart
	err := q.QueryRow(ctx, `
UPDATE charts
SET archived_at = now(), revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND archived_at IS NULL AND revision = $3
RETURNING `+chartColumns, projectID, chartID, expectedRevision).
		Scan(chartScanDest(&c)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var existing Chart
			if qerr := q.QueryRow(ctx,
				`SELECT `+chartColumns+` FROM charts WHERE project_id = $1 AND id = $2`,
				projectID, chartID).Scan(chartScanDest(&existing)...); qerr != nil {
				return Chart{}, pgx.ErrNoRows
			} else if existing.ArchivedAt != nil {
				return existing, nil
			}
			return Chart{}, ErrRevisionConflict
		}
		return Chart{}, err
	}
	return c, nil
}

// ArchiveChart is the non-transactional edge for callers that do not need an
// idempotency receipt.
func (s *Store) ArchiveChart(ctx context.Context, projectID, chartID string, expectedRevision int64) (Chart, error) {
	return archiveChart(ctx, s.pg, projectID, chartID, expectedRevision)
}

// ArchiveChartIdempotent is archiveChart under an idempotency claim.
func (s *Store) ArchiveChartIdempotent(ctx context.Context, projectID, chartID string, expectedRevision int64, idemKey, requestHash string) (Chart, error) {
	raw, err := s.runIdempotent(ctx, projectID, "archive_chart", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			c, err := archiveChart(ctx, q, projectID, chartID, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return Chart{}, err
	}
	var c Chart
	if err := json.Unmarshal(raw, &c); err != nil {
		return Chart{}, fmt.Errorf("stored chart receipt unreadable: %w", err)
	}
	return c, nil
}

// unarchiveChart reverses a soft archive under the same revision contract; an
// already-active row returns its current state untouched.
func unarchiveChart(ctx context.Context, q pgQuerier, projectID, chartID string, expectedRevision int64) (Chart, error) {
	var c Chart
	err := q.QueryRow(ctx, `
UPDATE charts
SET archived_at = NULL, revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND archived_at IS NOT NULL AND revision = $3
RETURNING `+chartColumns, projectID, chartID, expectedRevision).
		Scan(chartScanDest(&c)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var existing Chart
			if qerr := q.QueryRow(ctx,
				`SELECT `+chartColumns+` FROM charts WHERE project_id = $1 AND id = $2`,
				projectID, chartID).Scan(chartScanDest(&existing)...); qerr != nil {
				return Chart{}, pgx.ErrNoRows
			} else if existing.ArchivedAt == nil {
				return existing, nil
			}
			return Chart{}, ErrRevisionConflict
		}
		return Chart{}, err
	}
	return c, nil
}

// UnarchiveChart is the non-transactional edge for callers that do not need
// an idempotency receipt.
func (s *Store) UnarchiveChart(ctx context.Context, projectID, chartID string, expectedRevision int64) (Chart, error) {
	return unarchiveChart(ctx, s.pg, projectID, chartID, expectedRevision)
}

// UnarchiveChartIdempotent is unarchiveChart under an idempotency claim.
func (s *Store) UnarchiveChartIdempotent(ctx context.Context, projectID, chartID string, expectedRevision int64, idemKey, requestHash string) (Chart, error) {
	raw, err := s.runIdempotent(ctx, projectID, "unarchive_chart", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			c, err := unarchiveChart(ctx, q, projectID, chartID, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return Chart{}, err
	}
	var c Chart
	if err := json.Unmarshal(raw, &c); err != nil {
		return Chart{}, fmt.Errorf("stored chart receipt unreadable: %w", err)
	}
	return c, nil
}

// reorderCharts persists a new board order inside one transaction whose FIRST
// statement bumps the dashboard revision under the expected-revision check —
// the dashboard revision is the single optimistic fence for the whole board,
// so a reorder racing a dashboard update (or another reorder) yields exactly
// one success. Each listed chart id gets sort_order = its index; ids outside
// the dashboard are ignored (the UPDATE simply matches nothing). Returns the
// bumped dashboard so the caller holds the new fence value.
func reorderCharts(ctx context.Context, q pgQuerier, projectID, dashboardID string, chartIDs []string, expectedRevision int64) (Dashboard, error) {
	var d Dashboard
	err := withTx(ctx, q, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
UPDATE dashboards
SET revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND revision = $3
RETURNING `+dashboardColumns, projectID, dashboardID, expectedRevision).
			Scan(dashboardScanDest(&d)...)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var exists bool
				if qerr := tx.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM dashboards WHERE project_id = $1 AND id = $2)`,
					projectID, dashboardID).Scan(&exists); qerr == nil && exists {
					return ErrRevisionConflict
				}
				return pgx.ErrNoRows
			}
			return err
		}
		for i, id := range chartIDs {
			if _, err := tx.Exec(ctx, `
UPDATE charts SET sort_order = $4, updated_at = now()
WHERE project_id = $1 AND dashboard_id = $2 AND id = $3 AND archived_at IS NULL`, projectID, dashboardID, id, i); err != nil {
				return err
			}
		}
		return nil
	})
	return d, err
}

// ReorderChartsIdempotent is reorderCharts under an idempotency claim: the
// claim, the dashboard fence bump, and every sort_order write commit in one
// transaction — a retried reorder replays the first receipt instead of
// bumping the dashboard revision twice.
func (s *Store) ReorderChartsIdempotent(ctx context.Context, projectID, dashboardID string, chartIDs []string, expectedRevision int64, idemKey, requestHash string) (Dashboard, error) {
	raw, err := s.runIdempotent(ctx, projectID, "reorder_charts", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			d, err := reorderCharts(ctx, q, projectID, dashboardID, chartIDs, expectedRevision)
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

// --- source (connector) archive ---

// dataConnectorColumns is the full read shape including the credential
// reference presence flag and archive metadata. HasDSN is true for either a
// legacy inline ciphertext or a credential reference — the secret itself is
// never selected.
const dataConnectorColumns = `id::text, project_id::text, name, kind, dsn_ciphertext != '' OR credential_id IS NOT NULL, revision, archived_at, created_at, updated_at`

func dataConnectorScanDest(c *DataConnector) []any {
	return []any{&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.HasDSN, &c.Revision, &c.ArchivedAt, &c.CreatedAt, &c.UpdatedAt}
}

// ListDataConnectorsFiltered lists the project's connectors;
// includeArchived=false omits archived rows, true returns everything.
func (s *Store) ListDataConnectorsFiltered(ctx context.Context, projectID string, includeArchived bool) ([]DataConnector, error) {
	q := `
SELECT ` + dataConnectorColumns + `
FROM data_connectors
WHERE project_id = $1`
	if !includeArchived {
		q += ` AND archived_at IS NULL`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.pg.Query(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DataConnector{}
	for rows.Next() {
		var c DataConnector
		if err := rows.Scan(dataConnectorScanDest(&c)...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DataConnectorForProject reads one connector, project-scoped, archived rows
// included — the adapter's revision lookup for callers that do not send one.
func (s *Store) DataConnectorForProject(ctx context.Context, projectID, connectorID string) (DataConnector, error) {
	var c DataConnector
	err := s.pg.QueryRow(ctx,
		`SELECT `+dataConnectorColumns+` FROM data_connectors WHERE project_id = $1 AND id = $2`,
		projectID, connectorID).Scan(dataConnectorScanDest(&c)...)
	return c, err
}

// archiveDataConnector soft-archives a connector and, in the same
// transaction, disables its syncs and cancels the runs it already admitted —
// an archived source can never keep landing rows. Queued runs end cancelled
// inside this transaction; a running run is asked to stop and does so at its
// next heartbeat (the engine's 10-second poll), finishing cancelled.
// Syncs it disables are marked disabled_by_archive so unarchive resumes
// exactly those; a sync the operator paused by hand stays paused. The FIRST
// mutation requires the current revision; an already-archived row returns its
// current state untouched.
func archiveDataConnector(ctx context.Context, q pgQuerier, projectID, connectorID string, expectedRevision int64) (DataConnector, error) {
	var c DataConnector
	err := withTx(ctx, q, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
UPDATE data_connectors
SET archived_at = now(), revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND archived_at IS NULL AND revision = $3
RETURNING `+dataConnectorColumns, projectID, connectorID, expectedRevision).
			Scan(dataConnectorScanDest(&c)...)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var existing DataConnector
				if qerr := tx.QueryRow(ctx,
					`SELECT `+dataConnectorColumns+` FROM data_connectors WHERE project_id = $1 AND id = $2`,
					projectID, connectorID).Scan(dataConnectorScanDest(&existing)...); qerr != nil {
					return pgx.ErrNoRows
				} else if existing.ArchivedAt != nil {
					c = existing
					return nil
				}
				return ErrRevisionConflict
			}
			return err
		}
		_, err = tx.Exec(ctx, `
UPDATE connector_syncs
SET enabled = false, disabled_by_archive = true, revision = revision + 1, updated_at = now()
WHERE connector_id = $1 AND project_id = $2 AND enabled`, connectorID, projectID)
		if err != nil {
			return err
		}
		// An archived source must never keep landing rows, and pausing its
		// syncs only stops the runs it would start next. Stop the runs it
		// already admitted too, in the same transaction: a queued run is
		// terminal here, and a running run carries the flag its worker reads
		// back on its next heartbeat before it finishes cancelled.
		_, err = tx.Exec(ctx, `
UPDATE connector_runs
SET cancel_requested = true,
    status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
    finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END
WHERE connector_id = $1 AND project_id = $2 AND status IN ('queued', 'running')`, connectorID, projectID)
		return err
	})
	return c, err
}

// ArchiveDataConnectorIdempotent is archiveDataConnector under an idempotency
// claim — the claim, the archive, and the sync pause commit in one
// transaction.
func (s *Store) ArchiveDataConnectorIdempotent(ctx context.Context, projectID, connectorID string, expectedRevision int64, idemKey, requestHash string) (DataConnector, error) {
	raw, err := s.runIdempotent(ctx, projectID, "archive_source", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			c, err := archiveDataConnector(ctx, q, projectID, connectorID, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return DataConnector{}, err
	}
	var c DataConnector
	if err := json.Unmarshal(raw, &c); err != nil {
		return DataConnector{}, fmt.Errorf("stored connector receipt unreadable: %w", err)
	}
	return c, nil
}

// unarchiveDataConnector reverses a source archive: clears archived_at and
// re-enables exactly the syncs the archive disabled (disabled_by_archive),
// leaving operator-paused syncs paused. Same revision contract; an
// already-active row returns its current state untouched.
func unarchiveDataConnector(ctx context.Context, q pgQuerier, projectID, connectorID string, expectedRevision int64) (DataConnector, error) {
	var c DataConnector
	err := withTx(ctx, q, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
UPDATE data_connectors
SET archived_at = NULL, revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND archived_at IS NOT NULL AND revision = $3
RETURNING `+dataConnectorColumns, projectID, connectorID, expectedRevision).
			Scan(dataConnectorScanDest(&c)...)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var existing DataConnector
				if qerr := tx.QueryRow(ctx,
					`SELECT `+dataConnectorColumns+` FROM data_connectors WHERE project_id = $1 AND id = $2`,
					projectID, connectorID).Scan(dataConnectorScanDest(&existing)...); qerr != nil {
					return pgx.ErrNoRows
				} else if existing.ArchivedAt == nil {
					c = existing
					return nil
				}
				return ErrRevisionConflict
			}
			return err
		}
		_, err = tx.Exec(ctx, `
UPDATE connector_syncs
SET enabled = true, disabled_by_archive = false, revision = revision + 1, updated_at = now()
WHERE connector_id = $1 AND project_id = $2 AND disabled_by_archive`, connectorID, projectID)
		return err
	})
	return c, err
}

// UnarchiveDataConnectorIdempotent is unarchiveDataConnector under an
// idempotency claim.
func (s *Store) UnarchiveDataConnectorIdempotent(ctx context.Context, projectID, connectorID string, expectedRevision int64, idemKey, requestHash string) (DataConnector, error) {
	raw, err := s.runIdempotent(ctx, projectID, "unarchive_source", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			c, err := unarchiveDataConnector(ctx, q, projectID, connectorID, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return DataConnector{}, err
	}
	var c DataConnector
	if err := json.Unmarshal(raw, &c); err != nil {
		return DataConnector{}, fmt.Errorf("stored connector receipt unreadable: %w", err)
	}
	return c, nil
}
