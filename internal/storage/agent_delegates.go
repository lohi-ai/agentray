package storage

import (
	"context"
	"errors"
)

// Cross-agent delegation grants (ARCHITECT-AGENT-TEAM delegate, pulled forward
// without teams/kanban). A row says "this agent may hand tasks to that agent"
// through spawn_subagent's agent parameter. Self-delegation is built into the
// harness (a spawn with no agent argument forks the caller) and needs no row.
// The control plane validates the delegate operates in the same project; the
// run path re-validates via the join in AgentDelegatesForRun, so a deleted or
// disabled delegate silently drops off the roster instead of failing the run.

var errDelegateSelf = errors.New("an agent can always invoke itself — no grant needed")
var errDelegateNotInProject = errors.New("delegate agent does not operate in this project")

// AgentDelegateSelection is one grant row as the settings UI sees it: the
// delegate's identity resolved for display alongside the enabled flag.
type AgentDelegateSelection struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	Enabled bool   `json:"enabled"`
}

// ListAgentDelegates returns the agent's delegation grants for any project
// member — the read surface the UI shows alongside the project's agent list.
func (s *Store) ListAgentDelegates(ctx context.Context, userID, projectID, agentID string) ([]AgentDelegateSelection, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT d.delegate_agent_id::text, a.name, a.slug, d.enabled
FROM agent_delegates d
JOIN agents a ON a.id = d.delegate_agent_id
WHERE d.scope_id = $1
ORDER BY a.name ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentDelegateSelection, 0)
	for rows.Next() {
		var sel AgentDelegateSelection
		if err := rows.Scan(&sel.AgentID, &sel.Name, &sel.Slug, &sel.Enabled); err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	return out, rows.Err()
}

// UpsertAgentDelegate stores (or overwrites) one delegation grant (owner/admin
// only). The delegate must operate in the same project (home agent or granted
// in) and cannot be the agent itself — self-delegation is always available.
func (s *Store) UpsertAgentDelegate(ctx context.Context, userID, projectID, agentID, delegateAgentID string, enabled bool) error {
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
	if delegateAgentID == scopeID {
		return errDelegateSelf
	}
	ok, err := s.agentInProject(ctx, project.ID, delegateAgentID)
	if err != nil {
		return err
	}
	if !ok && !isDefaultAgent(project.ID, delegateAgentID) {
		return errDelegateNotInProject
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_delegates (scope_id, delegate_agent_id, enabled)
VALUES ($1, $2, $3)
ON CONFLICT (scope_id, delegate_agent_id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`,
		scopeID, delegateAgentID, enabled)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.delegate.update", "project", project.ID, project.Name, "{}")
	return nil
}

// DeleteAgentDelegate removes one delegation grant (owner/admin only).
func (s *Store) DeleteAgentDelegate(ctx context.Context, userID, projectID, agentID, delegateAgentID string) error {
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
	_, err = s.pg.Exec(ctx, `DELETE FROM agent_delegates WHERE scope_id = $1 AND delegate_agent_id = $2`, scopeID, delegateAgentID)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.delegate.delete", "project", project.ID, project.Name, "{}")
	return nil
}

// AgentDelegateTarget is a resolved, currently-valid delegation target for the
// run path: identity plus a one-line persona hint (the first line of the
// delegate's soul) so the model can pick the right teammate.
type AgentDelegateTarget struct {
	AgentID     string
	Name        string
	Slug        string
	Description string
}

// AgentDelegatesForRun returns the enabled, still-valid delegation targets for
// a run (internal trusted path, mirrors AgentToolsForRun). The join enforces
// the target still exists and is enabled, so a deleted/disabled delegate drops
// off the roster fail-closed. The persona hint comes from the delegate's own
// definition (scope_id = delegate agent id).
func (s *Store) AgentDelegatesForRun(ctx context.Context, scopeID string) ([]AgentDelegateTarget, error) {
	rows, err := s.pg.Query(ctx, `
SELECT d.delegate_agent_id::text, a.name, a.slug,
       COALESCE(split_part(def.soul_md, E'\n', 1), '')
FROM agent_delegates d
JOIN agents a ON a.id = d.delegate_agent_id AND a.enabled
LEFT JOIN agent_definitions def ON def.scope_id = a.id
WHERE d.scope_id = $1 AND d.enabled
ORDER BY a.name ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentDelegateTarget, 0)
	for rows.Next() {
		var t AgentDelegateTarget
		if err := rows.Scan(&t.AgentID, &t.Name, &t.Slug, &t.Description); err != nil {
			return nil, err
		}
		if r := []rune(t.Description); len(r) > 160 {
			t.Description = string(r[:160])
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
