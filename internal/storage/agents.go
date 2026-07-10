package storage

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// First-class agents (AgentGarden §3). A project owns many agents; the default
// agent's id equals its project_id, so the existing scope_id/project_id-keyed
// child tables (definitions, skills, secrets, tools, memory, runs) belong to it
// with no data migration. This file is the identity + CRUD layer; rewiring the
// run path to operate per non-default agent is a later increment.

// Agent is one configurable agent within a project.
type Agent struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	IsDefault bool      `json:"is_default"`
	Enabled   bool      `json:"enabled"`
	Autonomy  string    `json:"autonomy"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var errInvalidAgentSlug = errors.New("invalid agent slug (must match [a-z0-9][a-z0-9-]{0,63})")
var errCannotDeleteDefault = errors.New("the default agent cannot be deleted")

var agentSlugRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validAgentSlug reports whether slug is a legal agent slug. Pure, so the rule
// is unit-testable without a database.
func validAgentSlug(slug string) bool { return agentSlugRegex.MatchString(slug) }

// slugify derives a slug from a display name: lowercased, non-alphanumerics
// collapsed to single hyphens, trimmed. Pure and unit-testable.
func slugify(name string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// isDefaultAgent reports whether agentID is a project's default agent. The
// default agent's id equals its project_id by construction (see the backfill in
// migrateAgent), so the check needs no DB read. Pure and unit-testable.
func isDefaultAgent(projectID, agentID string) bool { return projectID == agentID }

// agentScope authorizes a control-plane request against a project and resolves
// the scope id to key per-agent data. An empty agentID (or one equal to the
// project, i.e. the default agent) resolves to the project id, preserving the
// existing single-agent behavior; a non-empty agentID must belong to the
// project or the request is refused. Returns the project (for workspace/audit)
// alongside the scope id.
func (s *Store) agentScope(ctx context.Context, userID, projectID, agentID string) (Project, string, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return Project{}, "", err
	}
	if agentID == "" || isDefaultAgent(project.ID, agentID) {
		return project, project.ID, nil
	}
	ok, err := s.agentInProject(ctx, project.ID, agentID)
	if err != nil {
		return Project{}, "", err
	}
	if !ok {
		return Project{}, "", errAgentForbidden
	}
	return project, agentID, nil
}

// AgentScopeForRun resolves a run's scope id without a requesting user (the run
// path is system-initiated and already trusted). An empty agentID resolves to
// the default agent (the project id); a non-empty agentID must belong to the
// project or the run is refused, so a stale/foreign id can never run.
func (s *Store) AgentScopeForRun(ctx context.Context, projectID, agentID string) (string, error) {
	if agentID == "" || isDefaultAgent(projectID, agentID) {
		return projectID, nil
	}
	ok, err := s.agentInProject(ctx, projectID, agentID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errAgentForbidden
	}
	return agentID, nil
}

// agentInProject reports whether agentID operates in projectID — either as its
// home agent (agents.project_id) or via a workspace grant. A granted agent owned
// by the same workspace can run in the project without being its home agent.
func (s *Store) agentInProject(ctx context.Context, projectID, agentID string) (bool, error) {
	var ok bool
	err := s.pg.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM agents WHERE id = $1 AND project_id = $2)
    OR EXISTS(SELECT 1 FROM agent_project_grants WHERE agent_id = $1 AND project_id = $2)`, agentID, projectID).Scan(&ok)
	return ok, err
}

// ListAgents returns a project's agents (default first, then by name) for any
// project member.
func (s *Store) ListAgents(ctx context.Context, userID, projectID string) ([]Agent, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	// Agents operating in the project: its home agents plus any workspace agent
	// granted into it (the "assigned to this product" set).
	rows, err := s.pg.Query(ctx, `
SELECT a.id::text, a.project_id::text, a.name, a.slug, a.is_default, a.enabled, a.autonomy, a.created_at, a.updated_at
FROM agents a
WHERE a.project_id = $1
   OR a.id IN (SELECT agent_id FROM agent_project_grants WHERE project_id = $1)
ORDER BY a.is_default DESC, a.name ASC`, project.ID)
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

// CreateAgent adds a non-default agent to a project (owner/admin only). The slug
// is derived from the name when not supplied and must satisfy the slug rule;
// uniqueness within the project is enforced by the table constraint.
func (s *Store) CreateAgent(ctx context.Context, userID, projectID, name, slug string) (Agent, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return Agent{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return Agent{}, err
	}
	if !canManage {
		return Agent{}, errAgentForbidden
	}
	name = strings.TrimSpace(name)
	if slug = strings.TrimSpace(slug); slug == "" {
		slug = slugify(name)
	}
	if !validAgentSlug(slug) {
		return Agent{}, errInvalidAgentSlug
	}
	var a Agent
	err = s.pg.QueryRow(ctx, `
INSERT INTO agents (project_id, workspace_id, name, slug, is_default, enabled, autonomy)
VALUES ($1, $4, $2, $3, false, true, 'suggest')
RETURNING id::text, project_id::text, name, slug, is_default, enabled, autonomy, created_at, updated_at`,
		project.ID, name, slug, project.WorkspaceID).
		Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.IsDefault, &a.Enabled, &a.Autonomy, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Agent{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.create", "project", project.ID, project.Name, "{}")
	return a, nil
}

// UpdateAgent renames and/or enables an agent (owner/admin only).
func (s *Store) UpdateAgent(ctx context.Context, userID, projectID, agentID, name string, enabled bool) (Agent, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return Agent{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return Agent{}, err
	}
	if !canManage {
		return Agent{}, errAgentForbidden
	}
	var a Agent
	err = s.pg.QueryRow(ctx, `
UPDATE agents SET name = $3, enabled = $4, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING id::text, project_id::text, name, slug, is_default, enabled, autonomy, created_at, updated_at`,
		project.ID, agentID, strings.TrimSpace(name), enabled).
		Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.IsDefault, &a.Enabled, &a.Autonomy, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Agent{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.update", "project", project.ID, project.Name, "{}")
	return a, nil
}

// DeleteAgent removes a non-default agent (owner/admin only). The default agent
// is refused: it owns the project's existing scope data and run path, so
// deleting it would orphan that data.
func (s *Store) DeleteAgent(ctx context.Context, userID, projectID, agentID string) error {
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
		return errCannotDeleteDefault
	}
	if _, err := s.pg.Exec(ctx, `DELETE FROM agents WHERE project_id = $1 AND id = $2 AND is_default = false`, project.ID, agentID); err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.delete", "project", project.ID, project.Name, "{}")
	return nil
}
