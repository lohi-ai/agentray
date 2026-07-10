package storage

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// WorkspaceModelTiers is the workspace-shared model tier pool (AgentGarden model
// config): the 3 tiers every project and agent in the workspace draws from. The
// bare Provider/Model/BaseURL/HasKey are the flash (default) tier; lite/pro are
// additive and fall back to flash when unconfigured. Keys are never returned —
// only the *HasKey presence flags.
type WorkspaceModelTiers struct {
	WorkspaceID string `json:"workspace_id"`

	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	HasKey   bool   `json:"has_key"`

	LiteProvider string `json:"lite_provider"`
	LiteModel    string `json:"lite_model"`
	LiteBaseURL  string `json:"lite_base_url"`
	LiteHasKey   bool   `json:"lite_has_key"`
	ProProvider  string `json:"pro_provider"`
	ProModel     string `json:"pro_model"`
	ProBaseURL   string `json:"pro_base_url"`
	ProHasKey    bool   `json:"pro_has_key"`

	ModelFallback bool `json:"model_fallback"`
}

// WorkspaceModelTiersInput is the mutable subset accepted from an owner/admin.
// Each tier's APIKey is optional: empty leaves the stored key unchanged, "-"
// clears it, any other value is encrypted at rest (resolveCipherArg).
type WorkspaceModelTiersInput struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string

	LiteProvider string
	LiteModel    string
	LiteBaseURL  string
	LiteAPIKey   string
	ProProvider  string
	ProModel     string
	ProBaseURL   string
	ProAPIKey    string

	ModelFallback bool
}

// GetWorkspaceModelTiers returns the workspace tier pool (keys redacted) for any
// workspace member.
func (s *Store) GetWorkspaceModelTiers(ctx context.Context, userID, workspaceID string) (WorkspaceModelTiers, error) {
	member, err := s.userInWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	if !member {
		return WorkspaceModelTiers{}, errAgentForbidden
	}
	return s.readWorkspaceModelTiers(ctx, workspaceID)
}

// readWorkspaceModelTiers loads the row (or a default pool when absent) without
// any ciphertext.
func (s *Store) readWorkspaceModelTiers(ctx context.Context, workspaceID string) (WorkspaceModelTiers, error) {
	cfg := WorkspaceModelTiers{WorkspaceID: workspaceID, Provider: "openai", ModelFallback: true}
	var cipher, liteCipher, proCipher *string
	err := s.pg.QueryRow(ctx, `
SELECT provider, model, base_url, api_key_ciphertext,
       lite_provider, lite_model, lite_base_url, lite_api_key_ciphertext,
       pro_provider, pro_model, pro_base_url, pro_api_key_ciphertext, model_fallback
FROM workspace_model_tiers WHERE workspace_id = $1`, workspaceID).Scan(
		&cfg.Provider, &cfg.Model, &cfg.BaseURL, &cipher,
		&cfg.LiteProvider, &cfg.LiteModel, &cfg.LiteBaseURL, &liteCipher,
		&cfg.ProProvider, &cfg.ProModel, &cfg.ProBaseURL, &proCipher, &cfg.ModelFallback)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return cfg, nil // no row yet → default pool
		}
		return WorkspaceModelTiers{}, err
	}
	cfg.HasKey = cipher != nil && *cipher != ""
	cfg.LiteHasKey = liteCipher != nil && *liteCipher != ""
	cfg.ProHasKey = proCipher != nil && *proCipher != ""
	return cfg, nil
}

// UpsertWorkspaceModelTiers writes the workspace tier pool; workspace owner/admin
// only. The change is recorded in the workspace audit log.
func (s *Store) UpsertWorkspaceModelTiers(ctx context.Context, userID, workspaceID string, in WorkspaceModelTiersInput) (WorkspaceModelTiers, error) {
	canManage, err := s.userCanManageWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	if !canManage {
		return WorkspaceModelTiers{}, errAgentForbidden
	}

	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = "openai"
	}
	cipherArg, err := resolveCipherArg(in.APIKey)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	liteCipherArg, err := resolveCipherArg(in.LiteAPIKey)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	proCipherArg, err := resolveCipherArg(in.ProAPIKey)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}

	_, err = s.pg.Exec(ctx, `
INSERT INTO workspace_model_tiers (
	workspace_id, provider, model, base_url, api_key_ciphertext,
	lite_provider, lite_model, lite_base_url, lite_api_key_ciphertext,
	pro_provider, pro_model, pro_base_url, pro_api_key_ciphertext, model_fallback
) VALUES ($1,$2,$3,$4,COALESCE($5,''),$6,$7,$8,COALESCE($9,''),$10,$11,$12,COALESCE($13,''),$14)
ON CONFLICT (workspace_id) DO UPDATE SET
	provider = EXCLUDED.provider,
	model = EXCLUDED.model,
	base_url = EXCLUDED.base_url,
	api_key_ciphertext = COALESCE($5, workspace_model_tiers.api_key_ciphertext),
	lite_provider = EXCLUDED.lite_provider,
	lite_model = EXCLUDED.lite_model,
	lite_base_url = EXCLUDED.lite_base_url,
	lite_api_key_ciphertext = COALESCE($9, workspace_model_tiers.lite_api_key_ciphertext),
	pro_provider = EXCLUDED.pro_provider,
	pro_model = EXCLUDED.pro_model,
	pro_base_url = EXCLUDED.pro_base_url,
	pro_api_key_ciphertext = COALESCE($13, workspace_model_tiers.pro_api_key_ciphertext),
	model_fallback = EXCLUDED.model_fallback,
	updated_at = now()`,
		workspaceID, provider, strings.TrimSpace(in.Model), strings.TrimSpace(in.BaseURL), cipherArg,
		strings.TrimSpace(in.LiteProvider), strings.TrimSpace(in.LiteModel), strings.TrimSpace(in.LiteBaseURL), liteCipherArg,
		strings.TrimSpace(in.ProProvider), strings.TrimSpace(in.ProModel), strings.TrimSpace(in.ProBaseURL), proCipherArg,
		in.ModelFallback)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}

	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_tiers.update", "workspace", workspaceID, "", "{}")
	return s.readWorkspaceModelTiers(ctx, workspaceID)
}

// WorkspaceTiersForRun loads the workspace tier pool for a system-initiated run
// (no requesting user), returning the redacted-shape struct plus the decrypted
// per-tier keys ("lite"/"flash"/"pro"). For in-memory call-time use only — never
// expose the keys over an API.
func (s *Store) WorkspaceTiersForRun(ctx context.Context, workspaceID string) (WorkspaceModelTiers, map[string]string, error) {
	cfg := WorkspaceModelTiers{WorkspaceID: workspaceID, Provider: "openai", ModelFallback: true}
	var flashC, liteC, proC *string
	err := s.pg.QueryRow(ctx, `
SELECT provider, model, base_url, api_key_ciphertext,
       lite_provider, lite_model, lite_base_url, lite_api_key_ciphertext,
       pro_provider, pro_model, pro_base_url, pro_api_key_ciphertext, model_fallback
FROM workspace_model_tiers WHERE workspace_id = $1`, workspaceID).Scan(
		&cfg.Provider, &cfg.Model, &cfg.BaseURL, &flashC,
		&cfg.LiteProvider, &cfg.LiteModel, &cfg.LiteBaseURL, &liteC,
		&cfg.ProProvider, &cfg.ProModel, &cfg.ProBaseURL, &proC, &cfg.ModelFallback)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceModelTiers{}, nil, err
	}
	keys := make(map[string]string, 3)
	for tier, c := range map[string]*string{"flash": flashC, "lite": liteC, "pro": proC} {
		if c == nil || *c == "" {
			continue
		}
		plain, decErr := decryptAgentKey(*c)
		if decErr != nil {
			return WorkspaceModelTiers{}, nil, decErr
		}
		keys[tier] = plain
	}
	cfg.HasKey = keys["flash"] != ""
	cfg.LiteHasKey = keys["lite"] != ""
	cfg.ProHasKey = keys["pro"] != ""
	return cfg, keys, nil
}

// WorkspaceIDForProject resolves the workspace a project belongs to, for the run
// path (which is keyed on project but reads workspace-scoped model tiers).
func (s *Store) WorkspaceIDForProject(ctx context.Context, projectID string) (string, error) {
	var wsID string
	err := s.pg.QueryRow(ctx, `SELECT workspace_id::text FROM projects WHERE id = $1`, projectID).Scan(&wsID)
	return wsID, err
}

// userInWorkspace reports whether the user is a member of the workspace.
func (s *Store) userInWorkspace(ctx context.Context, userID, workspaceID string) (bool, error) {
	var ok bool
	err := s.pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_members WHERE user_id = $1 AND workspace_id = $2)`, userID, workspaceID).Scan(&ok)
	return ok, err
}
