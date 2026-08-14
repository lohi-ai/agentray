package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Per-agent budgets & quotas (#4). A budget is a ceiling on what one agent may
// spend within a rolling period ('day' | 'month') across three dimensions —
// model cost (USD), total tokens, and run count. Any dimension left at 0 is
// uncapped. Metering reuses the one trace-store cost path (agent_llm_calls
// joined to agent_runs); there is no second pricing derivation. Enforcement
// happens in the runner at run admission and at each turn boundary.

// errBudgetInvalidPeriod is returned for a period other than day/month.
var errBudgetInvalidPeriod = errors.New("budget period must be 'day' or 'month'")

// AgentBudget is one ceiling row. ScopeID keys the agent (default agent id ==
// project_id). A workspace default is the same shape with IsWorkspaceDefault set
// and ScopeID = the workspace id.
type AgentBudget struct {
	ScopeID            string    `json:"scope_id"`
	Period             string    `json:"period"` // 'day' | 'month'
	MaxCostUSD         float64   `json:"max_cost_usd"`
	MaxTokens          int64     `json:"max_tokens"`
	MaxRuns            int       `json:"max_runs"`
	IsWorkspaceDefault bool      `json:"is_workspace_default"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// BudgetSpend is the metered usage for one agent within the current period.
type BudgetSpend struct {
	Period    string    `json:"period"`
	CostUSD   float64   `json:"cost_usd"`
	Tokens    int64     `json:"tokens"`
	Runs      int       `json:"runs"`
	Since     time.Time `json:"since"`
	AsOf      time.Time `json:"as_of"`
}

// BudgetStatus pairs a resolved budget with current spend and whether any limit
// is breached — the shape both the runner (enforcement) and the UI (surfacing)
// read.
type BudgetStatus struct {
	Budget    AgentBudget `json:"budget"`
	Spend     BudgetSpend `json:"spend"`
	HasBudget bool        `json:"has_budget"` // false = uncapped (no row anywhere)
	Exceeded  bool        `json:"exceeded"`
	Reason    string      `json:"reason,omitempty"` // which dimension tripped
}

func normalizeBudgetPeriod(period string) (string, error) {
	switch period {
	case "", "day":
		return "day", nil
	case "month":
		return "month", nil
	default:
		return "", errBudgetInvalidPeriod
	}
}

// periodStart returns the UTC start of the current rolling period for a budget:
// midnight today for 'day', first of the month for 'month'.
func periodStart(period string, now time.Time) time.Time {
	now = now.UTC()
	switch period {
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
}

// GetAgentBudget returns the agent's own budget rows (member-readable). Empty
// slice means the agent inherits the workspace default (if any).
func (s *Store) GetAgentBudget(ctx context.Context, userID, projectID, agentID string) ([]AgentBudget, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	return s.budgetsForScope(ctx, scopeID)
}

func (s *Store) budgetsForScope(ctx context.Context, scopeID string) ([]AgentBudget, error) {
	rows, err := s.pg.Query(ctx, `
SELECT scope_id::text, period, max_cost_usd, max_tokens, max_runs, is_workspace_default, updated_at
FROM agent_budgets
WHERE scope_id = $1
ORDER BY period ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentBudget, 0)
	for rows.Next() {
		var b AgentBudget
		if err := rows.Scan(&b.ScopeID, &b.Period, &b.MaxCostUSD, &b.MaxTokens, &b.MaxRuns, &b.IsWorkspaceDefault, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpsertAgentBudget stores (or overwrites) one agent budget (owner/admin only).
func (s *Store) UpsertAgentBudget(ctx context.Context, userID, projectID, agentID string, b AgentBudget) (AgentBudget, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentBudget{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentBudget{}, err
	}
	if !canManage {
		return AgentBudget{}, errAgentForbidden
	}
	period, err := normalizeBudgetPeriod(b.Period)
	if err != nil {
		return AgentBudget{}, err
	}
	if b.MaxCostUSD < 0 || b.MaxTokens < 0 || b.MaxRuns < 0 {
		return AgentBudget{}, fmt.Errorf("budget limits must be non-negative")
	}
	var out AgentBudget
	err = s.pg.QueryRow(ctx, `
INSERT INTO agent_budgets (scope_id, period, max_cost_usd, max_tokens, max_runs, is_workspace_default)
VALUES ($1, $2, $3, $4, $5, false)
ON CONFLICT (scope_id, period) DO UPDATE
	SET max_cost_usd = EXCLUDED.max_cost_usd, max_tokens = EXCLUDED.max_tokens,
	    max_runs = EXCLUDED.max_runs, updated_at = now()
RETURNING scope_id::text, period, max_cost_usd, max_tokens, max_runs, is_workspace_default, updated_at`,
		scopeID, period, b.MaxCostUSD, b.MaxTokens, b.MaxRuns).
		Scan(&out.ScopeID, &out.Period, &out.MaxCostUSD, &out.MaxTokens, &out.MaxRuns, &out.IsWorkspaceDefault, &out.UpdatedAt)
	if err != nil {
		return AgentBudget{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.budget.update", "project", project.ID, project.Name, "{}")
	return out, nil
}

// DeleteAgentBudget removes one agent budget period (owner/admin only), reverting
// the agent to the workspace default (or uncapped).
func (s *Store) DeleteAgentBudget(ctx context.Context, userID, projectID, agentID, period string) error {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return err
	}
	if !canManage {
		return errAgentForbidden
	}
	period, err = normalizeBudgetPeriod(period)
	if err != nil {
		return err
	}
	_, err = s.pg.Exec(ctx, `DELETE FROM agent_budgets WHERE scope_id = $1 AND period = $2 AND NOT is_workspace_default`, scopeID, period)
	return err
}

// SetWorkspaceDefaultBudget stores the workspace-level fallback (owner/admin
// only), applied to any agent in the workspace without its own row.
func (s *Store) SetWorkspaceDefaultBudget(ctx context.Context, userID, workspaceID string, b AgentBudget) (AgentBudget, error) {
	canManage, err := s.userCanManageWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return AgentBudget{}, err
	}
	if !canManage {
		return AgentBudget{}, errAgentForbidden
	}
	period, err := normalizeBudgetPeriod(b.Period)
	if err != nil {
		return AgentBudget{}, err
	}
	var out AgentBudget
	err = s.pg.QueryRow(ctx, `
INSERT INTO agent_budgets (scope_id, period, max_cost_usd, max_tokens, max_runs, is_workspace_default)
VALUES ($1, $2, $3, $4, $5, true)
ON CONFLICT (scope_id, period) DO UPDATE
	SET max_cost_usd = EXCLUDED.max_cost_usd, max_tokens = EXCLUDED.max_tokens,
	    max_runs = EXCLUDED.max_runs, is_workspace_default = true, updated_at = now()
RETURNING scope_id::text, period, max_cost_usd, max_tokens, max_runs, is_workspace_default, updated_at`,
		workspaceID, period, b.MaxCostUSD, b.MaxTokens, b.MaxRuns).
		Scan(&out.ScopeID, &out.Period, &out.MaxCostUSD, &out.MaxTokens, &out.MaxRuns, &out.IsWorkspaceDefault, &out.UpdatedAt)
	return out, err
}

// EffectiveBudgetForRun resolves the ceiling that applies to an agent's run: the
// agent's own row for the period wins; otherwise the workspace default; otherwise
// none (uncapped). Internal trusted path used by the runner — no RBAC, callers
// are the run path. workspaceID may be empty (project without a workspace), in
// which case only the agent's own row is considered.
func (s *Store) EffectiveBudgetForRun(ctx context.Context, scopeID, workspaceID, period string) (AgentBudget, bool, error) {
	period, err := normalizeBudgetPeriod(period)
	if err != nil {
		return AgentBudget{}, false, err
	}
	var b AgentBudget
	err = s.pg.QueryRow(ctx, `
SELECT scope_id::text, period, max_cost_usd, max_tokens, max_runs, is_workspace_default, updated_at
FROM agent_budgets WHERE scope_id = $1 AND period = $2`, scopeID, period).
		Scan(&b.ScopeID, &b.Period, &b.MaxCostUSD, &b.MaxTokens, &b.MaxRuns, &b.IsWorkspaceDefault, &b.UpdatedAt)
	if err == nil {
		return b, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AgentBudget{}, false, err
	}
	if workspaceID == "" {
		return AgentBudget{}, false, nil
	}
	err = s.pg.QueryRow(ctx, `
SELECT scope_id::text, period, max_cost_usd, max_tokens, max_runs, is_workspace_default, updated_at
FROM agent_budgets WHERE scope_id = $1 AND period = $2 AND is_workspace_default`, workspaceID, period).
		Scan(&b.ScopeID, &b.Period, &b.MaxCostUSD, &b.MaxTokens, &b.MaxRuns, &b.IsWorkspaceDefault, &b.UpdatedAt)
	if err == nil {
		return b, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentBudget{}, false, nil
	}
	return AgentBudget{}, false, err
}

// SpendForAgentPeriod meters what an agent has spent since the start of the
// current period. Cost and tokens sum the trace store (agent_llm_calls) — the
// same one-per-LLM-call rows the monitoring console reads, so there is no second
// cost derivation — joined to agent_runs for the agent_id. Run count comes from
// agent_runs directly (a run with zero LLM calls still counts). Sub-agent spend
// already folds into the parent run's rows via the runner's usage accounting.
func (s *Store) SpendForAgentPeriod(ctx context.Context, scopeID, period string) (BudgetSpend, error) {
	period, err := normalizeBudgetPeriod(period)
	if err != nil {
		return BudgetSpend{}, err
	}
	now := time.Now().UTC()
	since := periodStart(period, now)
	spend := BudgetSpend{Period: period, Since: since, AsOf: now}
	// Cost + tokens from the trace rows within the period.
	err = s.pg.QueryRow(ctx, `
SELECT COALESCE(SUM(c.cost_usd), 0), COALESCE(SUM(c.token_input + c.token_output), 0)
FROM agent_llm_calls c
JOIN agent_runs r ON r.id = c.run_id
WHERE r.agent_id = $1 AND c.created_at >= $2`, scopeID, since).
		Scan(&spend.CostUSD, &spend.Tokens)
	if err != nil {
		return BudgetSpend{}, err
	}
	// Run count from agent_runs (runs started within the period).
	err = s.pg.QueryRow(ctx, `
SELECT COUNT(*) FROM agent_runs WHERE agent_id = $1 AND started_at >= $2`, scopeID, since).
		Scan(&spend.Runs)
	if err != nil {
		return BudgetSpend{}, err
	}
	return spend, nil
}

// BudgetStatusForRun resolves the effective budget and current spend and reports
// whether any dimension is breached. HasBudget=false means uncapped — the runner
// short-circuits enforcement. period defaults to 'day'.
func (s *Store) BudgetStatusForRun(ctx context.Context, scopeID, workspaceID, period string) (BudgetStatus, error) {
	budget, has, err := s.EffectiveBudgetForRun(ctx, scopeID, workspaceID, period)
	if err != nil {
		return BudgetStatus{}, err
	}
	if !has {
		return BudgetStatus{HasBudget: false}, nil
	}
	spend, err := s.SpendForAgentPeriod(ctx, scopeID, budget.Period)
	if err != nil {
		return BudgetStatus{}, err
	}
	status := BudgetStatus{Budget: budget, Spend: spend, HasBudget: true}
	status.Exceeded, status.Reason = budgetExceeded(budget, spend)
	return status, nil
}

// budgetExceeded reports whether spend has reached or passed any set limit, and
// which one. A 0 limit on a dimension is uncapped and never trips.
func budgetExceeded(b AgentBudget, spend BudgetSpend) (bool, string) {
	if b.MaxCostUSD > 0 && spend.CostUSD >= b.MaxCostUSD {
		return true, fmt.Sprintf("cost $%.4f ≥ cap $%.4f (%s)", spend.CostUSD, b.MaxCostUSD, b.Period)
	}
	if b.MaxTokens > 0 && spend.Tokens >= b.MaxTokens {
		return true, fmt.Sprintf("tokens %d ≥ cap %d (%s)", spend.Tokens, b.MaxTokens, b.Period)
	}
	if b.MaxRuns > 0 && spend.Runs >= b.MaxRuns {
		return true, fmt.Sprintf("runs %d ≥ cap %d (%s)", spend.Runs, b.MaxRuns, b.Period)
	}
	return false, ""
}
