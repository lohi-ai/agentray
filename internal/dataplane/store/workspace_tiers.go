package storage

import (
	"context"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// WorkspaceModelTiers is the workspace-shared model tier pool (AgentGarden model
// config): the 3 tiers every project and agent in the workspace draws from. The
// bare Provider/Model/BaseURL/HasKey are the flash (default) tier; lite/pro are
// additive and fall back to flash when unconfigured. Keys are never returned —
// the *HasKey flags mean the tier is credential-ready (including OAuth account
// pools and explicit local engines that need no key).
type WorkspaceModelTiers struct {
	WorkspaceID string `json:"workspace_id"`

	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	HasKey   bool   `json:"has_key"`
	// ContextWindow overrides the model's input window in tokens, which caps how
	// large the transcript may grow before the agent compacts it. 0 — the normal
	// case — means the window is worked out from the model id, and an operator
	// only sets it for an endpoint no catalog can know (a self-hosted model, or a
	// gateway that serves a model truncated).
	ContextWindow int                         `json:"context_window,omitempty"`
	Capabilities  agentcore.ModelCapabilities `json:"capabilities,omitempty"`
	// FallbackModel is an optional second model the run retries on when the
	// primary model fails. FallbackProviderID blank means the model lives on
	// the tier's own provider (in-tier fallback); set means the rung runs on
	// that provider row — the cross-provider leg of the escalation ladder.
	// FallbackProvider/BaseURL/HasKey are the resolved provider's redacted
	// fields, filled by ResolveWorkspaceRun (empty for same-provider).
	FallbackModel            string                      `json:"fallback_model,omitempty"`
	FallbackProviderID       string                      `json:"fallback_provider_id,omitempty"`
	FallbackProvider         string                      `json:"fallback_provider,omitempty"`
	FallbackBaseURL          string                      `json:"fallback_base_url,omitempty"`
	FallbackHasKey           bool                        `json:"fallback_has_key,omitempty"`
	FallbackCapabilities     agentcore.ModelCapabilities `json:"fallback_capabilities,omitempty"`
	LiteProvider             string                      `json:"lite_provider"`
	LiteModel                string                      `json:"lite_model"`
	LiteBaseURL              string                      `json:"lite_base_url"`
	LiteHasKey               bool                        `json:"lite_has_key"`
	LiteContextWindow        int                         `json:"lite_context_window,omitempty"`
	LiteCapabilities         agentcore.ModelCapabilities `json:"lite_capabilities,omitempty"`
	LiteFallbackModel        string                      `json:"lite_fallback_model,omitempty"`
	LiteFallbackProviderID   string                      `json:"lite_fallback_provider_id,omitempty"`
	LiteFallbackProvider     string                      `json:"lite_fallback_provider,omitempty"`
	LiteFallbackBaseURL      string                      `json:"lite_fallback_base_url,omitempty"`
	LiteFallbackHasKey       bool                        `json:"lite_fallback_has_key,omitempty"`
	LiteFallbackCapabilities agentcore.ModelCapabilities `json:"lite_fallback_capabilities,omitempty"`
	ProProvider              string                      `json:"pro_provider"`
	ProModel                 string                      `json:"pro_model"`
	ProBaseURL               string                      `json:"pro_base_url"`
	ProHasKey                bool                        `json:"pro_has_key"`
	ProContextWindow         int                         `json:"pro_context_window,omitempty"`
	ProCapabilities          agentcore.ModelCapabilities `json:"pro_capabilities,omitempty"`
	ProFallbackModel         string                      `json:"pro_fallback_model,omitempty"`
	ProFallbackProviderID    string                      `json:"pro_fallback_provider_id,omitempty"`
	ProFallbackProvider      string                      `json:"pro_fallback_provider,omitempty"`
	ProFallbackBaseURL       string                      `json:"pro_fallback_base_url,omitempty"`
	ProFallbackHasKey        bool                        `json:"pro_fallback_has_key,omitempty"`
	ProFallbackCapabilities  agentcore.ModelCapabilities `json:"pro_fallback_capabilities,omitempty"`
	ModelFallback            bool                        `json:"model_fallback"`
	// HostedDefault is true when the workspace is using the process-level
	// default model (no BYOK key). Settings can say "using the hosted model"
	// instead of pretending the tenant pasted a key.
	HostedDefault bool `json:"hosted_default"`

	// Multi-provider config: the 3 tiers select a model of an active
	// configured provider. Providers is the redacted list (no keys).
	Providers       []WorkspaceProvider `json:"providers,omitempty"`
	FlashProviderID string              `json:"flash_provider_id,omitempty"`
	LiteProviderID  string              `json:"lite_provider_id,omitempty"`
	ProProviderID   string              `json:"pro_provider_id,omitempty"`
}

// HostModelDefaults is the optional process-level model pool a workspace
// inherits when it has not pasted its own key.
type HostModelDefaults struct {
	Provider string
	BaseURL  string
	APIKey   string
	Flash    string
	Lite     string
	Pro      string
}

// HostModelDefaultsFromConfig copies the hosted-model fields off Config.
func HostModelDefaultsFromConfig(cfg config.Config) HostModelDefaults {
	provider := strings.TrimSpace(cfg.DefaultModelProvider)
	if provider == "" {
		provider = "openai"
	}
	flash := strings.TrimSpace(cfg.DefaultModelFlash)
	if flash == "" {
		flash = "flash"
	}
	lite := strings.TrimSpace(cfg.DefaultModelLite)
	if lite == "" {
		lite = "plus"
	}
	pro := strings.TrimSpace(cfg.DefaultModelPro)
	if pro == "" {
		pro = "pro"
	}
	return HostModelDefaults{
		Provider: provider,
		BaseURL:  strings.TrimSpace(cfg.DefaultModelBaseURL),
		APIKey:   strings.TrimSpace(cfg.DefaultModelAPIKey),
		Flash:    flash,
		Lite:     lite,
		Pro:      pro,
	}
}

// applyHostModelFallback fills an empty workspace pool from the hosted default.
// Pure so unit tests can cover it without Postgres. A workspace that already
// has a flash key is left alone (BYOK wins).
//
// A tier that receives the host key also takes the host's provider/model/base
// URL wholesale — never the workspace's. base_url is tenant-editable
// (WorkspaceModelTiersInput), so filling only the empty fields would ship the
// platform's API key to an endpoint an owner/admin chose.
func applyHostModelFallback(cfg WorkspaceModelTiers, keys map[string]string, host HostModelDefaults) (WorkspaceModelTiers, map[string]string) {
	if host.APIKey == "" {
		return cfg, keys
	}
	if keys == nil {
		keys = map[string]string{}
	}
	if keys["flash"] != "" || cfg.HasKey {
		return cfg, keys
	}
	cfg.Provider = host.Provider
	cfg.BaseURL = host.BaseURL
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = host.Flash
	}
	keys["flash"] = host.APIKey
	cfg.HasKey = true
	if keys["lite"] == "" {
		keys["lite"] = host.APIKey
		cfg.LiteProvider = host.Provider
		cfg.LiteBaseURL = host.BaseURL
		if strings.TrimSpace(cfg.LiteModel) == "" {
			cfg.LiteModel = host.Lite
		}
		cfg.LiteHasKey = true
	}
	if keys["pro"] == "" {
		keys["pro"] = host.APIKey
		cfg.ProProvider = host.Provider
		cfg.ProBaseURL = host.BaseURL
		if strings.TrimSpace(cfg.ProModel) == "" {
			cfg.ProModel = host.Pro
		}
		cfg.ProHasKey = true
	}
	cfg.HostedDefault = true
	return cfg, keys
}

// WorkspaceModelTiersInput is the mutable subset accepted from an owner/admin.
// Each tier's APIKey is optional: empty leaves the stored key unchanged, "-"
// clears it, any other value is encrypted at rest (resolveCipherArg).
type WorkspaceModelTiersInput struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	// ContextWindow (and its lite/pro siblings) override the model's input
	// window in tokens; 0 restores "work it out from the model id". Unlike
	// APIKey there is no separate clear sentinel, because 0 already means
	// "no override".
	ContextWindow int
	Capabilities  agentcore.ModelCapabilities

	LiteProvider      string
	LiteModel         string
	LiteBaseURL       string
	LiteAPIKey        string
	LiteContextWindow int
	ProProvider       string
	ProModel          string
	ProBaseURL        string
	ProAPIKey         string
	ProContextWindow  int
	LiteCapabilities  agentcore.ModelCapabilities
	ProCapabilities   agentcore.ModelCapabilities
	ModelFallback     bool
	// Per-tier fallbacks — a (provider, model) pair tried when the tier's
	// model call fails. A blank provider id keeps the fallback on the tier's
	// own provider; a set one crosses providers.
	FallbackModel            string
	FallbackProviderID       string
	FallbackCapabilities     agentcore.ModelCapabilities
	LiteFallbackModel        string
	LiteFallbackProviderID   string
	LiteFallbackCapabilities agentcore.ModelCapabilities
	ProFallbackModel         string
	ProFallbackProviderID    string
	ProFallbackCapabilities  agentcore.ModelCapabilities

	// Provider-id form (preferred). When set, these win over the legacy
	// per-tier vendor/key columns.
	FlashProviderID string
	LiteProviderID  string
	ProProviderID   string
}

// GetWorkspaceModelTiers returns the workspace tier pool (keys redacted) for any
// workspace member.
func (s *Store) GetWorkspaceModelTiers(ctx context.Context, userID, workspaceID string) (WorkspaceModelTiers, error) {
	member, err := s.userInWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	if !member {
		return WorkspaceModelTiers{}, ErrAgentForbidden
	}
	return s.readWorkspaceModelTiers(ctx, workspaceID)
}

// readWorkspaceModelTiers loads the row (or a default pool when absent) without
// any ciphertext. Prefer configured workspace_providers; fall back to the
// pre-upgrade one-row-per-workspace columns so existing keys still resolve.
func (s *Store) readWorkspaceModelTiers(ctx context.Context, workspaceID string) (WorkspaceModelTiers, error) {
	book, err := s.loadBook(ctx, workspaceID, false)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	cfg, _ := book.Resolve()
	cfg.WorkspaceID = workspaceID
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	// A workspace that already has a BYOK provider must not pick up the
	// hosted lite/pro model IDs. GET is redacted (no decrypted keys), so
	// host fallback used to see an empty pool and inject those IDs — Save
	// then persisted them as overrides and broke blank-lite/pro inherit.
	if providerBookHasKey(book) {
		return cfg, nil
	}
	cfg, _ = applyHostModelFallback(cfg, nil, s.hostModel)
	return cfg, nil
}

// providerBookHasKey reports whether any configured provider already has
// credentials: a stored key (ciphertext present, or a decrypted key on the run
// path) or, for OAuth vendors, at least one connected account. An OAuth
// provider never carries a key — its tokens live in
// workspace_provider_accounts — so without the account check a workspace keyed
// purely by subscription accounts would look credential-less and get its tiers
// overwritten by the host fallback.
func providerBookHasKey(book *WorkspaceProviderBook) bool {
	if book == nil {
		return false
	}
	for _, p := range book.Providers {
		if p.HasKey || p.APIKey != "" || p.AccountCount > 0 || ai.APIKeyOptional(p.Vendor) {
			return true
		}
	}
	return false
}

// UpsertWorkspaceModelTiers writes the workspace tier pool; workspace owner/admin
// only. The change is recorded in the workspace audit log.
func (s *Store) UpsertWorkspaceModelTiers(ctx context.Context, userID, workspaceID string, in WorkspaceModelTiersInput) (WorkspaceModelTiers, error) {
	canManage, err := s.userCanManageWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return WorkspaceModelTiers{}, err
	}
	if !canManage {
		return WorkspaceModelTiers{}, ErrAgentForbidden
	}

	// Preferred path: tiers point at configured providers. An already-migrated
	// workspace (provider rows exist) always saves the selection, even when
	// lite/pro (or flash) are blank — blank means inherit.
	if in.FlashProviderID != "" || in.LiteProviderID != "" || in.ProProviderID != "" {
		return s.SaveWorkspaceTierSelection(ctx, userID, workspaceID, WorkspaceTierSelection{
			FlashProviderID: strings.TrimSpace(in.FlashProviderID),
			FlashModel:      strings.TrimSpace(in.Model),
			LiteProviderID:  strings.TrimSpace(in.LiteProviderID),
			LiteModel:       strings.TrimSpace(in.LiteModel),
			ProProviderID:   strings.TrimSpace(in.ProProviderID),
			ProModel:        strings.TrimSpace(in.ProModel),
			ModelFallback:   in.ModelFallback,

			FlashContextWindow: in.ContextWindow,
			FlashCapabilities:  in.Capabilities,
			LiteContextWindow:  in.LiteContextWindow,
			LiteCapabilities:   in.LiteCapabilities,
			ProContextWindow:   in.ProContextWindow,
			ProCapabilities:    in.ProCapabilities,

			FlashFallbackModel:        strings.TrimSpace(in.FallbackModel),
			FlashFallbackProviderID:   strings.TrimSpace(in.FallbackProviderID),
			FlashFallbackCapabilities: in.FallbackCapabilities,
			LiteFallbackModel:         strings.TrimSpace(in.LiteFallbackModel),
			LiteFallbackProviderID:    strings.TrimSpace(in.LiteFallbackProviderID),
			LiteFallbackCapabilities:  in.LiteFallbackCapabilities,
			ProFallbackModel:          strings.TrimSpace(in.ProFallbackModel),
			ProFallbackProviderID:     strings.TrimSpace(in.ProFallbackProviderID),
			ProFallbackCapabilities:   in.ProFallbackCapabilities,
		})
	}
	if existing, lerr := s.loadBook(ctx, workspaceID, false); lerr == nil && len(existing.Providers) > 0 {
		return s.SaveWorkspaceTierSelection(ctx, userID, workspaceID, WorkspaceTierSelection{
			FlashProviderID: strings.TrimSpace(in.FlashProviderID),
			FlashModel:      strings.TrimSpace(in.Model),
			LiteProviderID:  strings.TrimSpace(in.LiteProviderID),
			LiteModel:       strings.TrimSpace(in.LiteModel),
			ProProviderID:   strings.TrimSpace(in.ProProviderID),
			ProModel:        strings.TrimSpace(in.ProModel),
			ModelFallback:   in.ModelFallback,

			FlashContextWindow: in.ContextWindow,
			FlashCapabilities:  in.Capabilities,
			LiteContextWindow:  in.LiteContextWindow,
			LiteCapabilities:   in.LiteCapabilities,
			ProContextWindow:   in.ProContextWindow,
			ProCapabilities:    in.ProCapabilities,

			FlashFallbackModel:        strings.TrimSpace(in.FallbackModel),
			FlashFallbackProviderID:   strings.TrimSpace(in.FallbackProviderID),
			FlashFallbackCapabilities: in.FallbackCapabilities,
			LiteFallbackModel:         strings.TrimSpace(in.LiteFallbackModel),
			LiteFallbackProviderID:    strings.TrimSpace(in.LiteFallbackProviderID),
			LiteFallbackCapabilities:  in.LiteFallbackCapabilities,
			ProFallbackModel:          strings.TrimSpace(in.ProFallbackModel),
			ProFallbackProviderID:     strings.TrimSpace(in.ProFallbackProviderID),
			ProFallbackCapabilities:   in.ProFallbackCapabilities,
		})
	}

	// Legacy write: one-row-per-workspace vendor+key columns. Still accepted so
	// existing clients keep working; we persist the columns AND upsert matching
	// provider rows so the next read goes through the multi-provider path.
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
	if err := s.syncLegacyProviders(ctx, workspaceID); err != nil {
		return WorkspaceModelTiers{}, err
	}

	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "agent.workspace_tiers.update", "workspace", workspaceID, "", "{}")
	return s.readWorkspaceModelTiers(ctx, workspaceID)
}

// syncLegacyProviders creates/updates provider rows from the denormalized
// workspace_model_tiers columns after a legacy-shaped write, so a subsequent
// read goes through the multi-provider book.
func (s *Store) syncLegacyProviders(ctx context.Context, workspaceID string) error {
	var n int
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM workspace_providers WHERE workspace_id = $1`, workspaceID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		// Providers already exist — leave them; the denormalized columns are
		// just the tier display. A later SaveWorkspaceTierSelection owns the
		// pointers.
		return nil
	}
	return s.backfillWorkspaceProviders(ctx)
}

// WorkspaceTiersForRun loads the workspace tier pool for a system-initiated run
// (no requesting user), returning the redacted-shape struct plus the decrypted
// per-tier keys ("lite"/"flash"/"pro"). For in-memory call-time use only — never
// expose the keys over an API.
func (s *Store) WorkspaceTiersForRun(ctx context.Context, workspaceID string) (WorkspaceModelTiers, map[string]string, error) {
	book, err := s.loadBook(ctx, workspaceID, true)
	if err != nil {
		return WorkspaceModelTiers{}, nil, err
	}
	cfg, keys := book.Resolve()
	cfg.WorkspaceID = workspaceID
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	if providerBookHasKey(book) {
		return cfg, keys, nil
	}
	cfg, keys = applyHostModelFallback(cfg, keys, s.hostModel)
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

// WorkspaceProviderVendor returns one provider row's vendor for a workspace
// member — the cheap read the OAuth login flow needs, where loadBook's full
// providers+tiers+account-count scan would be three extra queries for one
// column. pgx.ErrNoRows when the row is missing or the user is not a member.
func (s *Store) WorkspaceProviderVendor(ctx context.Context, userID, workspaceID, providerID string) (string, error) {
	var vendor string
	err := s.pg.QueryRow(ctx, `
SELECT p.vendor FROM workspace_providers p
WHERE p.id = $1 AND p.workspace_id = $2
  AND EXISTS (SELECT 1 FROM workspace_members m WHERE m.user_id = $3 AND m.workspace_id = $2)`,
		providerID, workspaceID, userID).Scan(&vendor)
	return vendor, err
}
