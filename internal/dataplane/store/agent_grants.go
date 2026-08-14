package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Agent ownership model (AgentGarden, 2026-06): a workspace *owns* an agent (the
// company hires the analyst) and *grants* it into one or more projects (assigns
// it to products). The grant carries the per-project capability scopes — that is
// the access control surface ("can read kiem-lai, can suggest, can't write").
// agents.project_id remains the agent's home project (and the default agent's
// id==project_id identity); a non-default agent reaches other projects purely
// through agent_project_grants.

var errAgentWrongWorkspace = errors.New("agent does not belong to this project's workspace")
var errCannotRevokeHome = errors.New("cannot revoke the agent's home project grant")

// AgentGrant is one workspace-agent → project assignment with its per-project
// scope cap.
type AgentGrant struct {
	AgentID   string          `json:"agent_id"`
	ProjectID string          `json:"project_id"`
	Scopes    map[string]bool `json:"scopes"`
	CreatedAt time.Time       `json:"created_at"`
}

// agentWorkspace returns the workspace that owns an agent (empty when unknown).
func (s *Store) agentWorkspace(ctx context.Context, agentID string) (string, error) {
	var ws string
	err := s.pg.QueryRow(ctx, `SELECT COALESCE(workspace_id::text, '') FROM agents WHERE id = $1`, agentID).Scan(&ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return ws, err
}

// GrantAgentToProject assigns a workspace-owned agent into a project with the
// given scope cap (owner/admin of the workspace only). The agent must be owned
// by the same workspace as the project, so an agent can never reach a product in
// another company. Idempotent: re-granting updates the scopes.
func (s *Store) GrantAgentToProject(ctx context.Context, userID, agentID, projectID string, scopes map[string]bool) (AgentGrant, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentGrant{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentGrant{}, err
	}
	if !canManage {
		return AgentGrant{}, errAgentForbidden
	}
	ws, err := s.agentWorkspace(ctx, agentID)
	if err != nil {
		return AgentGrant{}, err
	}
	if ws == "" || ws != project.WorkspaceID {
		return AgentGrant{}, errAgentWrongWorkspace
	}
	normalized := normalizeScopes(scopes)
	payload, err := json.Marshal(normalized)
	if err != nil {
		return AgentGrant{}, err
	}
	var g AgentGrant
	g.Scopes = normalized
	err = s.pg.QueryRow(ctx, `
INSERT INTO agent_project_grants (agent_id, project_id, scopes)
VALUES ($1, $2, $3)
ON CONFLICT (agent_id, project_id) DO UPDATE SET scopes = EXCLUDED.scopes
RETURNING agent_id::text, project_id::text, created_at`, agentID, projectID, payload).
		Scan(&g.AgentID, &g.ProjectID, &g.CreatedAt)
	if err != nil {
		return AgentGrant{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.grant", "project", project.ID, project.Name, "{}")
	return g, nil
}

// RevokeAgentFromProject removes an agent's access to a project (owner/admin
// only). The default agent's home grant cannot be revoked — it owns the
// project's scope data and run path.
func (s *Store) RevokeAgentFromProject(ctx context.Context, userID, agentID, projectID string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
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
	if isDefaultAgent(project.ID, agentID) {
		return errCannotRevokeHome
	}
	if _, err := s.pg.Exec(ctx, `DELETE FROM agent_project_grants WHERE agent_id = $1 AND project_id = $2`, agentID, projectID); err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.revoke", "project", project.ID, project.Name, "{}")
	return nil
}

// ListWorkspaceAgents returns every agent the workspace owns (across all home
// projects), for the "hired agents" view. Any workspace member may read.
func (s *Store) ListWorkspaceAgents(ctx context.Context, userID, workspaceID string) ([]Agent, error) {
	ok, err := s.userCanAccessWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errAgentForbidden
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, slug, is_default, enabled, autonomy, created_at, updated_at
FROM agents WHERE workspace_id = $1 ORDER BY is_default DESC, name ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Agent, 0)
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.IsDefault, &a.Enabled, &a.Autonomy, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAgentGrants returns the projects an agent is granted into, with scopes.
func (s *Store) ListAgentGrants(ctx context.Context, userID, agentID string) ([]AgentGrant, error) {
	ws, err := s.agentWorkspace(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if ws == "" {
		return nil, errAgentForbidden
	}
	ok, err := s.userCanAccessWorkspace(ctx, userID, ws)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errAgentForbidden
	}
	rows, err := s.pg.Query(ctx, `
SELECT agent_id::text, project_id::text, scopes, created_at
FROM agent_project_grants WHERE agent_id = $1 ORDER BY created_at ASC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentGrant, 0)
	for rows.Next() {
		var g AgentGrant
		var raw []byte
		if err := rows.Scan(&g.AgentID, &g.ProjectID, &raw, &g.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &g.Scopes)
		g.Scopes = normalizeScopes(g.Scopes)
		out = append(out, g)
	}
	return out, rows.Err()
}

// grantScopes returns the per-project scope cap for an agent in a project. found
// is false when no grant exists (default agent, or agent not granted here), in
// which case the caller keeps the agent's own capabilities uncapped.
func (s *Store) grantScopes(ctx context.Context, projectID, agentID string) (map[string]bool, bool, error) {
	var raw []byte
	err := s.pg.QueryRow(ctx, `SELECT scopes FROM agent_project_grants WHERE agent_id = $1 AND project_id = $2`, agentID, projectID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	scopes := map[string]bool{}
	_ = json.Unmarshal(raw, &scopes)
	return normalizeScopes(scopes), true, nil
}

// anyScopeTrue reports whether a scope map grants at least one capability. An
// all-false grant is treated as "no explicit cap" (the agent keeps its own
// capabilities), not "deny everything" — to deny, revoke the grant instead.
func anyScopeTrue(m map[string]bool) bool {
	for _, v := range m {
		if v {
			return true
		}
	}
	return false
}

// intersectScopes caps `own` by `cap`: a capability is granted only when both
// allow it. Used to enforce a project grant's scope ceiling over an agent's own
// capabilities.
func intersectScopes(own, cap map[string]bool) map[string]bool {
	out := normalizeScopes(own)
	for k := range out {
		out[k] = out[k] && cap[k]
	}
	return out
}

// upsertAgentGrant writes a grant without RBAC (system path: marketplace install,
// seeding). The caller is already trusted and has resolved the project.
func (s *Store) upsertAgentGrant(ctx context.Context, agentID, projectID string, scopes map[string]bool) error {
	payload, err := json.Marshal(normalizeScopes(scopes))
	if err != nil {
		return err
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_project_grants (agent_id, project_id, scopes)
VALUES ($1, $2, $3)
ON CONFLICT (agent_id, project_id) DO UPDATE SET scopes = EXCLUDED.scopes`, agentID, projectID, payload)
	return err
}
