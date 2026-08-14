package storage

import (
	"context"
	"time"
)

// AgentCore Lab persistence: saved test cases (AC5 "a saved case re-runs to the
// same verdict shape"). A case is just an input + an expected output the builder
// pins to an agent; running it is the Lab service's job, this layer only stores
// the case and the last verdict. It is keyed by scope_id like agent_secrets, so
// a case belongs to one agent (the default agent when agentID is empty). No new
// per-step persistence lives here — replay reconstructs steps from the existing
// agent_llm_calls trace (see agent_trace.go); only the case definition is new.

// AgentLabCase is one saved test case: an input run against an agent and the
// expected final output to compare against, plus the last run's verdict.
type AgentLabCase struct {
	ID         string    `json:"id"`
	ScopeID    string    `json:"scope_id"`
	Name       string    `json:"name"`
	Input      string    `json:"input"`
	Expected   string    `json:"expected"`
	LastStatus string    `json:"last_status"` // "", "pass", "fail"
	LastRunID  string    `json:"last_run_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// migrateAgentLab creates the saved-case table. Kept separate from migrateAgent
// so the Lab surface evolves independently; idempotent per repo convention and
// called from Store.migrate after migrateAgentTrace.
func (s *Store) migrateAgentLab(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_lab_cases (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	scope_id UUID NOT NULL,
	name VARCHAR(200) NOT NULL DEFAULT '',
	input TEXT NOT NULL DEFAULT '',
	expected TEXT NOT NULL DEFAULT '',
	last_status VARCHAR(16) NOT NULL DEFAULT '',
	last_run_id UUID,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS agent_lab_cases_scope_idx ON agent_lab_cases (scope_id, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// ListAgentLabCases returns the saved cases for an agent (any project member).
func (s *Store) ListAgentLabCases(ctx context.Context, userID, projectID, agentID string) ([]AgentLabCase, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, scope_id::text, name, input, expected, last_status,
       COALESCE(last_run_id::text, ''), created_at, updated_at
FROM agent_lab_cases WHERE scope_id = $1 ORDER BY created_at DESC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentLabCase{}
	for rows.Next() {
		var c AgentLabCase
		if err := rows.Scan(&c.ID, &c.ScopeID, &c.Name, &c.Input, &c.Expected,
			&c.LastStatus, &c.LastRunID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveAgentLabCase inserts a new case (owner/admin only) and returns it.
func (s *Store) SaveAgentLabCase(ctx context.Context, userID, projectID, agentID, name, input, expected string) (AgentLabCase, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentLabCase{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentLabCase{}, err
	}
	if !canManage {
		return AgentLabCase{}, errAgentForbidden
	}
	var c AgentLabCase
	err = s.pg.QueryRow(ctx, `
INSERT INTO agent_lab_cases (scope_id, name, input, expected)
VALUES ($1, $2, $3, $4)
RETURNING id::text, scope_id::text, name, input, expected, last_status,
          COALESCE(last_run_id::text, ''), created_at, updated_at`,
		scopeID, name, input, expected).Scan(&c.ID, &c.ScopeID, &c.Name, &c.Input,
		&c.Expected, &c.LastStatus, &c.LastRunID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return AgentLabCase{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.lab.case.save", "project", project.ID, project.Name, "{}")
	return c, nil
}

// UpdateAgentLabCaseVerdict records the outcome of running a case (owner/admin
// only): the last status and the run it produced, so the case list shows the
// latest verdict shape.
func (s *Store) UpdateAgentLabCaseVerdict(ctx context.Context, userID, projectID, agentID, caseID, status, runID string) error {
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
	var runArg any
	if runID != "" {
		runArg = runID
	}
	_, err = s.pg.Exec(ctx, `
UPDATE agent_lab_cases SET last_status = $3, last_run_id = $4, updated_at = now()
WHERE id = $1 AND scope_id = $2`, caseID, scopeID, status, runArg)
	return err
}

// DeleteAgentLabCase removes a saved case (owner/admin only).
func (s *Store) DeleteAgentLabCase(ctx context.Context, userID, projectID, agentID, caseID string) error {
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
	_, err = s.pg.Exec(ctx, `DELETE FROM agent_lab_cases WHERE id = $1 AND scope_id = $2`, caseID, scopeID)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.lab.case.delete", "project", project.ID, project.Name, "{}")
	return nil
}
