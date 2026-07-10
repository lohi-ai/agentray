package storage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds the read model for the agent monitoring console — the
// project-wide rollup and per-agent drill-down that power /agents/monitor. It is
// agent-agnostic: every aggregate keys on agents.id, and runs attribute to an
// agent via coalesce(agent_id, project_id) so the default agent (whose id equals
// its project_id by construction) and any future first-class agent are both
// covered with no per-agent code. Read-only and member-scoped; the run/trace
// writers live in agent_runtime.go and agent_trace.go.

// AgentMonitorRow is one agent plus its run rollup. Agent is embedded so the JSON
// is a single flat object (id, name, … run_count, cost_usd, …).
type AgentMonitorRow struct {
	Agent
	RunCount     int        `json:"run_count"`
	RunningCount int        `json:"running_count"`
	ErrorCount   int        `json:"error_count"`
	TokenInput   int        `json:"token_input"`
	TokenOutput  int        `json:"token_output"`
	CostUSD      float64    `json:"cost_usd"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
}

// monitorSelect is the shared projection: every agent column the Agent struct
// scans, plus the run aggregates. LEFT JOIN keeps agents with zero runs.
const monitorSelect = `
SELECT a.id::text, a.project_id::text, a.name, a.slug, a.is_default, a.enabled, a.autonomy, a.created_at, a.updated_at,
       count(r.id) AS run_count,
       count(r.id) FILTER (WHERE r.status = 'running') AS running_count,
       count(r.id) FILTER (WHERE r.status = 'error') AS error_count,
       coalesce(sum(r.token_input), 0) AS token_input,
       coalesce(sum(r.token_output), 0) AS token_output,
       coalesce(sum(r.cost_usd), 0) AS cost_usd,
       max(r.started_at) AS last_run_at
FROM agents a
LEFT JOIN agent_runs r ON coalesce(r.agent_id, r.project_id) = a.id`

const monitorGroupBy = `
GROUP BY a.id, a.project_id, a.name, a.slug, a.is_default, a.enabled, a.autonomy, a.created_at, a.updated_at`

func scanMonitorRow(row pgx.Row) (AgentMonitorRow, error) {
	var m AgentMonitorRow
	err := row.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Slug, &m.IsDefault, &m.Enabled, &m.Autonomy, &m.CreatedAt, &m.UpdatedAt,
		&m.RunCount, &m.RunningCount, &m.ErrorCount, &m.TokenInput, &m.TokenOutput, &m.CostUSD, &m.LastRunAt)
	return m, err
}

// ListAgentMonitor returns every agent in a project with its run rollup
// (default first, then by name) — the /agents/monitor overview. Member-readable.
func (s *Store) ListAgentMonitor(ctx context.Context, userID, projectID string) ([]AgentMonitorRow, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, monitorSelect+`
WHERE a.project_id = $1`+monitorGroupBy+`
ORDER BY a.is_default DESC, a.name ASC`, project.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMonitorRow{}
	for rows.Next() {
		m, err := scanMonitorRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetAgentMonitor returns one agent's rollup. The agent must belong to the
// project (enforced by the WHERE); a missing/foreign id is a not-found.
func (s *Store) GetAgentMonitor(ctx context.Context, userID, projectID, agentID string) (AgentMonitorRow, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentMonitorRow{}, err
	}
	row := s.pg.QueryRow(ctx, monitorSelect+`
WHERE a.project_id = $1 AND a.id = $2`+monitorGroupBy, project.ID, agentID)
	m, err := scanMonitorRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentMonitorRow{}, errAgentForbidden
	}
	if err != nil {
		return AgentMonitorRow{}, err
	}
	return m, nil
}

// ListAgentRunsForAgent returns one agent's recent runs (member-readable). Runs
// attribute via coalesce(agent_id, project_id) so the default agent's existing
// rows (which predate agent_id or carry it equal to project_id) are included.
func (s *Store) ListAgentRunsForAgent(ctx context.Context, userID, projectID, agentID string, limit int) ([]AgentRun, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, coalesce(agent_id, project_id)::text, trigger, status, token_input, token_output, cost_usd, summary, started_at, finished_at
FROM agent_runs
WHERE project_id = $1 AND coalesce(agent_id, project_id) = $2
ORDER BY started_at DESC LIMIT $3`, project.ID, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentRun{}
	for rows.Next() {
		var r AgentRun
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.AgentID, &r.Trigger, &r.Status, &r.TokenInput, &r.TokenOutput, &r.CostUSD, &r.Summary, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
