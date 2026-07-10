package storage

import (
	"context"
	"strings"
)

// Per-agent selectable tools (AgentGarden §6). A row records whether a
// registry tool is enabled for this project and its per-agent config JSON (for
// http_request, the host allowlist). The control plane validates the tool name
// against the agentruntime registry before writing; storage stays dumb about
// which names exist so it carries no dependency on the agent edge. At run start
// AgentToolsForRun returns the rows so the runner can build (or suppress) the
// matching tool, overriding the host-global default of the same name.

// AgentToolSelection is one project's choice for a selectable tool.
type AgentToolSelection struct {
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	ConfigJSON string `json:"config"`
}

// ListAgentTools returns the project's tool selections for any project member —
// the read surface the UI shows alongside the tool catalog.
func (s *Store) ListAgentTools(ctx context.Context, userID, projectID, agentID string) ([]AgentToolSelection, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `SELECT tool_name, enabled, config_json FROM agent_tools WHERE scope_id = $1 ORDER BY tool_name ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentToolSelection, 0)
	for rows.Next() {
		var sel AgentToolSelection
		if err := rows.Scan(&sel.Name, &sel.Enabled, &sel.ConfigJSON); err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	return out, rows.Err()
}

// UpsertAgentTool stores (or overwrites) a project's selection for a tool
// (owner/admin only). The caller validates the name against the registry and
// the config shape; storage normalizes an empty config to "{}" so the column
// stays valid JSON.
func (s *Store) UpsertAgentTool(ctx context.Context, userID, projectID, agentID, name string, enabled bool, configJSON string) error {
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
	if strings.TrimSpace(configJSON) == "" {
		configJSON = "{}"
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_tools (scope_id, tool_name, enabled, config_json)
VALUES ($1, $2, $3, $4)
ON CONFLICT (scope_id, tool_name) DO UPDATE SET enabled = EXCLUDED.enabled, config_json = EXCLUDED.config_json, updated_at = now()`,
		scopeID, name, enabled, configJSON)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.tool.update", "project", project.ID, project.Name, "{}")
	return nil
}

// DeleteAgentTool removes a project's selection for a tool, reverting it to the
// host-global default (owner/admin only).
func (s *Store) DeleteAgentTool(ctx context.Context, userID, projectID, agentID, name string) error {
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
	_, err = s.pg.Exec(ctx, `DELETE FROM agent_tools WHERE scope_id = $1 AND tool_name = $2`, scopeID, name)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.tool.delete", "project", project.ID, project.Name, "{}")
	return nil
}

// AgentToolsForRun returns the agent scope's tool selections for in-memory
// call-time use only. It mirrors AgentSecretsForRun: the run path is internal
// and already trusted, so there is no per-user ownership check here.
func (s *Store) AgentToolsForRun(ctx context.Context, scopeID string) ([]AgentToolSelection, error) {
	rows, err := s.pg.Query(ctx, `SELECT tool_name, enabled, config_json FROM agent_tools WHERE scope_id = $1`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentToolSelection, 0)
	for rows.Next() {
		var sel AgentToolSelection
		if err := rows.Scan(&sel.Name, &sel.Enabled, &sel.ConfigJSON); err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	return out, rows.Err()
}
