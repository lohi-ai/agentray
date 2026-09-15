package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- Deterministic findings engine substrate (ticket 001) ---
//
// The findings engine (internal/dataplane/findings) is a rule-based detector
// pass that writes agent_recommendations rows with source='engine' — no agent,
// no LLM. This file holds the two tables that exist only for it:
//
//   finding_scan_state — one row per project, the conditional-UPDATE claim
//     that bounds the scheduled pass to one scan per project per interval
//     (the ClaimAlertRuleForEval shape: cheap optimistic lock, crash-safe
//     because a lost claim just re-runs on the next tick).
//   funnel_watches — the declared funnels the drop-off detector re-runs.
//     There is no saved-funnel entity anywhere else (run_funnel takes inline
//     steps), so the watch row is the minimal honest substrate: the detector
//     needs a durable "this sequence matters" declaration to compare windows
//     against, and watch_funnel/list_funnel_watches are how a user makes one.

// migrateFindings creates the findings-engine tables. New tables only — no
// rewrite of anything existing. Called from migratePostgres after migrateAgent
// so agent_recommendations (which the engine writes, not these tables) already
// exists.
func (s *Store) migrateFindings(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS finding_scan_state (
	project_id UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
	last_scan_at TIMESTAMPTZ NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS funnel_watches (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	steps TEXT[] NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS funnel_watches_project_idx ON funnel_watches (project_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// FindingScanProjects returns every project id the scheduled pass should
// consider, minus excludeID (the shared demo project — scanning it would file
// findings on demo traffic). Internal trusted path: no RBAC, called only by
// the in-process scanner.
func (s *Store) FindingScanProjects(ctx context.Context, excludeID string) ([]string, error) {
	rows, err := s.pg.Query(ctx, `SELECT id::text FROM projects WHERE id::text <> $1 ORDER BY id`, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ClaimFindingScan attempts to claim a project for a findings scan at now,
// returning true only if this caller won the claim. The upsert's WHERE makes
// the claim conditional on the previous scan being older than due, so two
// replicas (or a manual run racing the tick) cannot both scan — and a crashed
// scan is retried once the interval elapses, never locked out forever.
func (s *Store) ClaimFindingScan(ctx context.Context, projectID string, now, due time.Time) (bool, error) {
	var claimed string
	err := s.pg.QueryRow(ctx, `
INSERT INTO finding_scan_state (project_id, last_scan_at) VALUES ($1, $2)
ON CONFLICT (project_id) DO UPDATE SET last_scan_at = $2
WHERE finding_scan_state.last_scan_at < $3
RETURNING project_id::text`, projectID, now, due).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// FunnelWatch is one declared funnel the drop-off detector re-runs each scan.
type FunnelWatch struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Steps     []string  `json:"steps"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateFunnelWatch declares a funnel for the drop-off detector. Re-declaring
// the same ordered steps returns the existing watch rather than stacking a
// duplicate — two watches over one sequence would double every finding.
func (s *Store) CreateFunnelWatch(ctx context.Context, projectID, name string, steps []string) (FunnelWatch, error) {
	clean := []string{}
	for _, step := range steps {
		if step = strings.TrimSpace(step); step != "" {
			clean = append(clean, step)
		}
	}
	if len(clean) < 2 {
		return FunnelWatch{}, fmt.Errorf("a funnel watch needs at least 2 steps, got %d", len(clean))
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.Join(clean, " → ")
	}
	var w FunnelWatch
	err := s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, name, steps, created_at
FROM funnel_watches WHERE project_id = $1 AND steps = $2
ORDER BY created_at ASC LIMIT 1`, projectID, clean).
		Scan(&w.ID, &w.ProjectID, &w.Name, &w.Steps, &w.CreatedAt)
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return FunnelWatch{}, err
	}
	err = s.pg.QueryRow(ctx, `
INSERT INTO funnel_watches (project_id, name, steps) VALUES ($1, $2, $3)
RETURNING id::text, project_id::text, name, steps, created_at`,
		projectID, name, clean).
		Scan(&w.ID, &w.ProjectID, &w.Name, &w.Steps, &w.CreatedAt)
	return w, err
}

// FunnelWatchesForProject lists a project's declared funnels, oldest first.
// Internal trusted path — the scanner and the list_funnel_watches op both read
// through it.
func (s *Store) FunnelWatchesForProject(ctx context.Context, projectID string) ([]FunnelWatch, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, steps, created_at
FROM funnel_watches WHERE project_id = $1 ORDER BY created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FunnelWatch{}
	for rows.Next() {
		var w FunnelWatch
		if err := rows.Scan(&w.ID, &w.ProjectID, &w.Name, &w.Steps, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
