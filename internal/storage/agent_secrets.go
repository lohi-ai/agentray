package storage

import (
	"context"

	"github.com/lohi-ai/agentray/internal/credential"
)

// Per-agent named secrets (AgentGarden §5). Values are AES-encrypted at rest
// with the same agentEncKey path as the LLM API keys. They are NEVER returned
// over an API — only their names are listed (mirroring AgentConfig.HasKey). At
// run start AgentSecretsForRun decrypts them into a credential.Vault so
// {{cred:NAME}} placeholders resolve at the trust boundary, with the literal
// secret never entering the model context, the trace, or logs.

// ListAgentSecretNames returns the agent's secret names (never the values), for
// any project member — this is the names-only read surface the UI shows. An
// empty agentID targets the project's default agent.
func (s *Store) ListAgentSecretNames(ctx context.Context, userID, projectID, agentID string) ([]string, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `SELECT name FROM agent_secrets WHERE scope_id = $1 ORDER BY name ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// UpsertAgentSecret stores (or overwrites) a named secret for a project
// (owner/admin only). The name must satisfy the shared credential naming rule
// so it can never be stored yet fail to resolve; an empty value is rejected for
// the same fail-fast reason credential.Vault.Put rejects it. The plaintext is
// AES-encrypted before it touches the row.
func (s *Store) UpsertAgentSecret(ctx context.Context, userID, projectID, agentID, name, value string) error {
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
	if !credential.ValidName(name) {
		return errInvalidSecretName
	}
	if value == "" {
		return errEmptySecretValue
	}
	ciphertext, err := encryptAgentKey(value)
	if err != nil {
		return err
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_secrets (scope_id, name, value_ciphertext)
VALUES ($1, $2, $3)
ON CONFLICT (scope_id, name) DO UPDATE SET value_ciphertext = EXCLUDED.value_ciphertext, updated_at = now()`,
		scopeID, name, ciphertext)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.secret.update", "project", project.ID, project.Name, "{}")
	return nil
}

// DeleteAgentSecret removes a named secret (owner/admin only).
func (s *Store) DeleteAgentSecret(ctx context.Context, userID, projectID, agentID, name string) error {
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
	_, err = s.pg.Exec(ctx, `DELETE FROM agent_secrets WHERE scope_id = $1 AND name = $2`, scopeID, name)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.secret.delete", "project", project.ID, project.Name, "{}")
	return nil
}

// AgentSecretsForRun returns the decrypted name→value map for an agent scope,
// for in-memory call-time use only — never expose this over an API. Like
// WorkspaceTiersForRun, the run path is internal and already trusted, so there
// is no per-user ownership check here.
func (s *Store) AgentSecretsForRun(ctx context.Context, scopeID string) (map[string]string, error) {
	rows, err := s.pg.Query(ctx, `SELECT name, value_ciphertext FROM agent_secrets WHERE scope_id = $1`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, cipher string
		if err := rows.Scan(&name, &cipher); err != nil {
			return nil, err
		}
		if cipher == "" {
			continue
		}
		plain, decErr := decryptAgentKey(cipher)
		if decErr != nil {
			return nil, decErr
		}
		out[name] = plain
	}
	return out, rows.Err()
}
