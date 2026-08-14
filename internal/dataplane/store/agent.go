package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

// AgentConfig is the per-project AI-agent configuration (§6.1). The API key is
// stored encrypted and is NEVER included in this struct's JSON — HasKey reports
// only its presence.
type AgentConfig struct {
	ProjectID    string          `json:"project_id"`
	Enabled      bool            `json:"enabled"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	BaseURL      string          `json:"base_url"`
	RedactPII    bool            `json:"redact_pii"`
	HasKey       bool            `json:"has_key"`
	Scopes       map[string]bool `json:"scopes"`
	Autonomy     string          `json:"autonomy"`
	ScheduleCron string          `json:"schedule_cron"`

	// Per-tier model overrides. The top-level Provider/Model/BaseURL/HasKey are
	// the flash (default) tier; lite/pro are additive and fall back to flash when
	// unconfigured. Keys are never returned — only the *HasKey presence flags.
	LiteProvider  string `json:"lite_provider"`
	LiteModel     string `json:"lite_model"`
	LiteBaseURL   string `json:"lite_base_url"`
	LiteHasKey    bool   `json:"lite_has_key"`
	ProProvider   string `json:"pro_provider"`
	ProModel      string `json:"pro_model"`
	ProBaseURL    string `json:"pro_base_url"`
	ProHasKey     bool   `json:"pro_has_key"`
	ModelFallback bool   `json:"model_fallback"`
}

// AgentConfigInput is the mutable subset accepted from an owner/admin. Model
// tiers are no longer part of this input — they live in the workspace model pool
// (WorkspaceModelTiers); this struct carries only the project-level run-eligibility
// fields. The model_* columns on agent_configs are left in place but no longer
// written or read.
type AgentConfigInput struct {
	Enabled      bool
	RedactPII    bool
	Scopes       map[string]bool
	Autonomy     string
	ScheduleCron string
}

// AgentCapabilityConfig is the per-agent usecase permission map. It controls the
// analytics operation tools exposed to this agent; project-level AgentConfig still
// owns the global enabled/redaction/autonomy gates for compatibility.
type AgentCapabilityConfig struct {
	ScopeID string          `json:"scope_id"`
	Scopes  map[string]bool `json:"scopes"`
}

var errAgentForbidden = errors.New("agent config permission denied")
var errInvalidSecretName = errors.New("invalid secret name (must match [A-Za-z0-9_.-]{1,128})")
var errEmptySecretValue = errors.New("secret value must not be empty")

// ErrInvalidAutonomy rejects an autonomy value outside the ladder, so the API
// layer can answer 400 instead of the generic permission 403.
var ErrInvalidAutonomy = errors.New("invalid autonomy: must be one of suggest, scheduled, auto")

// The autonomy ladder, lowest → highest trust. `suggest` files recommendations
// only; `scheduled` may run unattended (cron/webhook) but the runner strips
// external-write tools from those runs; `auto` is the explicit opt-in that
// keeps external-write tools in unattended runs (the hard publish rail opens).
const (
	AutonomySuggest   = "suggest"
	AutonomyScheduled = "scheduled"
	AutonomyAuto      = "auto"
)

// ValidAutonomy reports whether a is a rung of the autonomy ladder.
func ValidAutonomy(a string) bool {
	switch a {
	case AutonomySuggest, AutonomyScheduled, AutonomyAuto:
		return true
	default:
		return false
	}
}

// migrateAgent bootstraps the agent_* Postgres tables (§9, §14.10). Idempotent
// CREATE TABLE IF NOT EXISTS, matching the repo's inline-migrate convention.
// Config is the only table written in P0; the run/trace/recommendation/
// definition/skill/memory tables are created here so later phases (P1–P3) add
// only code, not schema.
func (s *Store) migrateAgent(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_configs (
	project_id UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
	enabled BOOLEAN NOT NULL DEFAULT false,
	provider VARCHAR(32) NOT NULL DEFAULT 'openai',
	model VARCHAR(128) NOT NULL DEFAULT '',
	base_url TEXT NOT NULL DEFAULT '',
	redact_pii BOOLEAN NOT NULL DEFAULT true,
	api_key_ciphertext TEXT NOT NULL DEFAULT '',
	scope_monitor BOOLEAN NOT NULL DEFAULT false,
	scope_data_quality BOOLEAN NOT NULL DEFAULT false,
	scope_analyze_build BOOLEAN NOT NULL DEFAULT false,
	scope_growth_suggest BOOLEAN NOT NULL DEFAULT false,
	autonomy VARCHAR(16) NOT NULL DEFAULT 'suggest',
	schedule_cron VARCHAR(64) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS agent_runs (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	trigger VARCHAR(16) NOT NULL,
	status VARCHAR(16) NOT NULL DEFAULT 'running',
	token_input INT NOT NULL DEFAULT 0,
	token_output INT NOT NULL DEFAULT 0,
	summary TEXT NOT NULL DEFAULT '',
	started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	finished_at TIMESTAMPTZ
)`,
		`CREATE INDEX IF NOT EXISTS agent_runs_project_started_idx ON agent_runs (project_id, started_at DESC)`,
		// Summed model cost (USD) per run, filled by the tracing/pricing layer.
		// Additive + nullable-with-default so existing runs read back 0.
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS agent_tool_calls (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
	tool VARCHAR(64) NOT NULL,
	args_json JSONB NOT NULL DEFAULT '{}'::jsonb,
	allowed BOOLEAN NOT NULL,
	result_meta TEXT NOT NULL DEFAULT '',
	duration_ms INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS agent_recommendations (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	run_id UUID REFERENCES agent_runs(id) ON DELETE SET NULL,
	category VARCHAR(16) NOT NULL,
	title TEXT NOT NULL,
	rationale TEXT NOT NULL DEFAULT '',
	evidence_json JSONB NOT NULL DEFAULT '{}'::jsonb,
	impact_score DOUBLE PRECISION NOT NULL DEFAULT 0,
	status VARCHAR(16) NOT NULL DEFAULT 'open',
	ack_note TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS agent_definitions (
	scope_id UUID PRIMARY KEY,
	soul_md TEXT NOT NULL DEFAULT '',
	agents_md TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS agent_skills (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	scope_id UUID NOT NULL,
	name VARCHAR(128) NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
	enabled BOOLEAN NOT NULL DEFAULT true,
	status VARCHAR(16) NOT NULL DEFAULT 'active',
	origin VARCHAR(16) NOT NULL DEFAULT 'user',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS agent_skills_scope_idx ON agent_skills (scope_id)`,
		`CREATE TABLE IF NOT EXISTS agent_memory (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	scope_id UUID NOT NULL,
	kind VARCHAR(16) NOT NULL,
	content TEXT NOT NULL,
	tags TEXT[] NOT NULL DEFAULT '{}',
	confidence DOUBLE PRECISION NOT NULL DEFAULT 0,
	source_run_id UUID,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		// Semantic-recall embedding (§14.7). Stored as JSONB float array so vector
		// recall needs no pgvector extension; ranking is computed in Go over the
		// small per-scope candidate set. Additive + nullable: rows without a vector
		// fall back to keyword recall.
		`ALTER TABLE agent_memory ADD COLUMN IF NOT EXISTS embedding JSONB`,
		`CREATE INDEX IF NOT EXISTS agent_memory_scope_idx ON agent_memory (scope_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS agent_sessions (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	scope_id UUID NOT NULL,
	parent_id UUID REFERENCES agent_sessions(id) ON DELETE SET NULL,
	messages JSONB NOT NULL DEFAULT '[]'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS agent_sessions_scope_idx ON agent_sessions (scope_id, updated_at DESC)`,
		// Per-tier model overrides (lite/pro). The existing provider/model/base_url/
		// api_key_ciphertext columns are the flash (default) tier; these are additive
		// and nullable so existing configs need no backfill. model_fallback toggles
		// upward escalation on a retryable provider error.
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS lite_provider VARCHAR(32) NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS lite_model VARCHAR(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS lite_base_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS lite_api_key_ciphertext TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS pro_provider VARCHAR(32) NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS pro_model VARCHAR(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS pro_base_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS pro_api_key_ciphertext TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_configs ADD COLUMN IF NOT EXISTS model_fallback BOOLEAN NOT NULL DEFAULT true`,
		// Per-agent named secrets (AgentGarden §5). Values are AES-encrypted with
		// the same agentEncKey path as the LLM API keys and are NEVER read back
		// over an API — only their names are listed. At run start they populate a
		// credential.Vault so {{cred:NAME}} placeholders in AGENTS.md/skills/tool
		// args resolve at the trust boundary. Keyed by scope_id = agent id (the
		// default agent's id equals its project_id, so existing rows are unchanged).
		`CREATE TABLE IF NOT EXISTS agent_secrets (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	scope_id UUID NOT NULL,
	name VARCHAR(128) NOT NULL,
	value_ciphertext TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (scope_id, name)
)`,
		// Per-agent selectable tools (AgentGarden §6): which registry tools this
		// agent may use and their per-agent config (e.g. http_request's host
		// allowlist). A row overrides the host-global default for that tool name;
		// no row falls back to it. Keyed by scope_id = agent id (the default
		// agent's id equals its project_id, so existing rows are unchanged).
		`CREATE TABLE IF NOT EXISTS agent_tools (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	scope_id UUID NOT NULL,
	tool_name VARCHAR(64) NOT NULL,
	enabled BOOLEAN NOT NULL DEFAULT true,
	config_json TEXT NOT NULL DEFAULT '{}',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (scope_id, tool_name)
)`,
		// First-class agent identity (AgentGarden §3): a project owns many agents.
		// The default agent's id equals its project_id, so every existing row keyed
		// by scope_id/project_id (definitions, skills, secrets, tools, memory, runs)
		// already belongs to the default agent — no data migration, and the
		// analytics analyst keeps running unchanged. Non-default agents get fresh
		// UUIDs; wiring them through the run path is a later increment.
		`CREATE TABLE IF NOT EXISTS agents (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(128) NOT NULL DEFAULT '',
	slug VARCHAR(64) NOT NULL DEFAULT '',
	is_default BOOLEAN NOT NULL DEFAULT false,
	enabled BOOLEAN NOT NULL DEFAULT true,
	autonomy VARCHAR(16) NOT NULL DEFAULT 'suggest',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (project_id, slug)
)`,
		`CREATE INDEX IF NOT EXISTS agents_project_idx ON agents (project_id)`,
		// Backfill one default agent per existing configured project, with
		// id = project_id so the scope key is preserved. Idempotent: ON CONFLICT
		// makes a re-run a no-op, so this is safe on every boot.
		`INSERT INTO agents (id, project_id, name, slug, is_default, enabled, autonomy)
	SELECT c.project_id, c.project_id, 'Default agent', 'default', true, c.enabled, c.autonomy
	FROM agent_configs c
	ON CONFLICT (id) DO NOTHING`,
		// Associate each run with the agent that produced it (data lineage). Additive
		// + backfilled to the default agent (id = project_id) so existing rows read
		// back coherently and the analytics run history is unchanged.
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS agent_id UUID`,
		`UPDATE agent_runs SET agent_id = project_id WHERE agent_id IS NULL`,
		// Per-agent triggers (AgentGarden §7): what starts a run. A schedule trigger
		// carries a cron; a webhook trigger carries an unguessable token (the global
		// ingress address) and an optional HMAC secret name (an agent_secrets entry)
		// for body authentication. prompt_template maps the inbound payload into the
		// run prompt. Keyed by scope_id = agent id, like the other per-agent tables.
		// The legacy agent_configs.schedule_cron remains the default agent's schedule
		// (still fired by ScheduledAgentProjects), so this table is purely additive.
		`CREATE TABLE IF NOT EXISTS agent_triggers (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	scope_id UUID NOT NULL,
	kind VARCHAR(16) NOT NULL,
	enabled BOOLEAN NOT NULL DEFAULT true,
	cron VARCHAR(128) NOT NULL DEFAULT '',
	webhook_token VARCHAR(64) NOT NULL DEFAULT '',
	prompt_template TEXT NOT NULL DEFAULT '',
	hmac_secret_name VARCHAR(128) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS agent_triggers_scope_idx ON agent_triggers (scope_id)`,
		// The webhook token is the global ingress address, so it must be unique —
		// but only across webhook rows that actually carry one (schedule rows leave
		// it empty). A partial unique index enforces that without colliding on ''.
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_triggers_webhook_token_idx ON agent_triggers (webhook_token) WHERE webhook_token <> ''`,
		// Workspace-shared model tier pool (AgentGarden model config). The 3 tiers
		// (lite/flash/pro: provider/model/base_url + encrypted key) are configured
		// once per workspace and shared by every project and agent in it. Mirrors the
		// model columns that used to live on agent_configs; the bare provider/model/
		// base_url/api_key are the flash (default) tier, lite/pro fall back to flash.
		// Keys are AES-encrypted and never returned over an API (only *_has_key).
		`CREATE TABLE IF NOT EXISTS workspace_model_tiers (
	workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
	provider VARCHAR(32) NOT NULL DEFAULT 'openai',
	model VARCHAR(128) NOT NULL DEFAULT '',
	base_url TEXT NOT NULL DEFAULT '',
	api_key_ciphertext TEXT NOT NULL DEFAULT '',
	lite_provider VARCHAR(32) NOT NULL DEFAULT '',
	lite_model VARCHAR(128) NOT NULL DEFAULT '',
	lite_base_url TEXT NOT NULL DEFAULT '',
	lite_api_key_ciphertext TEXT NOT NULL DEFAULT '',
	pro_provider VARCHAR(32) NOT NULL DEFAULT '',
	pro_model VARCHAR(128) NOT NULL DEFAULT '',
	pro_base_url TEXT NOT NULL DEFAULT '',
	pro_api_key_ciphertext TEXT NOT NULL DEFAULT '',
	model_fallback BOOLEAN NOT NULL DEFAULT true,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		// Backfill the workspace pool from existing per-project agent_configs: pick
		// one configured project per workspace (earliest-created with a key) and copy
		// its model tiers up. Lossy when a workspace has several configured projects —
		// one wins. Idempotent: ON CONFLICT makes a re-run a no-op.
		`INSERT INTO workspace_model_tiers (
	workspace_id, provider, model, base_url, api_key_ciphertext,
	lite_provider, lite_model, lite_base_url, lite_api_key_ciphertext,
	pro_provider, pro_model, pro_base_url, pro_api_key_ciphertext, model_fallback)
SELECT DISTINCT ON (p.workspace_id)
	p.workspace_id, c.provider, c.model, c.base_url, c.api_key_ciphertext,
	c.lite_provider, c.lite_model, c.lite_base_url, c.lite_api_key_ciphertext,
	c.pro_provider, c.pro_model, c.pro_base_url, c.pro_api_key_ciphertext, c.model_fallback
FROM agent_configs c
JOIN projects p ON p.id = c.project_id
WHERE c.api_key_ciphertext <> ''
ORDER BY p.workspace_id, p.created_at ASC
ON CONFLICT (workspace_id) DO NOTHING`,
		// Per-agent capability scopes (AgentGarden): which usecase/analytics tool
		// groups this agent can use. The default agent's scope row is backfilled from
		// agent_configs on read, preserving the original project-level behavior.
		`CREATE TABLE IF NOT EXISTS agent_capabilities (
	scope_id UUID PRIMARY KEY,
	scopes JSONB NOT NULL DEFAULT '{}'::jsonb,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		// Per-agent task→tier map (AgentGarden): which workspace tier each kind of
		// work runs on. JSON map over {triage,run,compaction,reflection} → {lite,
		// flash,pro}; an absent key (or no row) falls back to the default map in code.
		// Keyed by scope_id = agent id (default agent's id == project_id).
		`CREATE TABLE IF NOT EXISTS agent_task_tiers (
	scope_id UUID PRIMARY KEY,
	tiers JSONB NOT NULL DEFAULT '{}'::jsonb,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		// Retire the short-lived per-agent full-model override (this branch only):
		// per-agent model config is now a task→tier map over the workspace pool.
		`DROP TABLE IF EXISTS agent_model_overrides`,
		// Agent ownership moves up to the workspace (the "company hires the
		// analyst"). agents.project_id stays as the agent's *home* project (and
		// preserves the default-agent id==project_id trick); workspace_id is the
		// owner. Backfilled from the home project so every existing agent is owned
		// by the same workspace it already lived under — no behavior change.
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS workspace_id UUID`,
		`UPDATE agents a SET workspace_id = p.workspace_id
	FROM projects p WHERE a.project_id = p.id AND a.workspace_id IS NULL`,
		`CREATE INDEX IF NOT EXISTS agents_workspace_idx ON agents (workspace_id)`,
		// A workspace agent is *granted* into one or more projects (products). The
		// grant is the per-project access control: its scopes cap what the agent
		// may do in that project ("can read kiem-lai, can suggest, can't write").
		// An agent operates in a project iff it is that project's default agent OR a
		// grant row exists. Scopes JSONB mirrors agent_capabilities shape.
		`CREATE TABLE IF NOT EXISTS agent_project_grants (
	agent_id UUID NOT NULL,
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	scopes JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (agent_id, project_id)
)`,
		`CREATE INDEX IF NOT EXISTS agent_project_grants_project_idx ON agent_project_grants (project_id)`,
		// Backfill a grant for every existing agent into its home project, with the
		// agent's own capability scopes (or default-deny when it has no row yet), so
		// current access is preserved exactly. Idempotent via ON CONFLICT.
		`INSERT INTO agent_project_grants (agent_id, project_id, scopes)
	SELECT a.id, a.project_id, COALESCE(c.scopes, '{}'::jsonb)
	FROM agents a LEFT JOIN agent_capabilities c ON c.scope_id = a.id
	ON CONFLICT (agent_id, project_id) DO NOTHING`,
		// Conversation id a chat-triggered run belongs to, so the client can
		// reattach to (poll) the run after navigating away mid-stream — the run now
		// finishes on a detached context independent of the SSE connection. Additive
		// + defaulted, so scheduled/webhook runs (no session) read back ''.
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS session_id VARCHAR(64) NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS agent_runs_session_idx ON agent_runs (project_id, session_id, started_at DESC) WHERE session_id <> ''`,
		// Cross-agent delegation grants (ARCHITECT-AGENT-TEAM delegate, pulled
		// forward): which OTHER agents this agent may hand a task to through
		// spawn_subagent's agent parameter. Keyed by scope_id = the granting
		// agent's id (default agent id == project_id, same trick as agent_tools).
		// Self-delegation needs no row — spawn_subagent always allows a self-fork.
		// The delegate must operate in the same project at run time; the grant is
		// re-checked then, so a revoked/deleted/disabled delegate fails closed.
		`CREATE TABLE IF NOT EXISTS agent_delegates (
	scope_id UUID NOT NULL,
	delegate_agent_id UUID NOT NULL,
	enabled BOOLEAN NOT NULL DEFAULT true,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (scope_id, delegate_agent_id)
)`,
		// Per-agent budgets & quotas (#4). A ceiling is scoped to one agent and one
		// rolling period ('day' | 'month'); any of the three limits may be 0 = no
		// cap on that dimension. scope_id keys the agent (default agent id ==
		// project_id, same trick as agent_tools/agent_delegates). A workspace-level
		// default lives in a synthetic row with scope_id = the workspace id and
		// is_workspace_default = true, applied to any agent without its own row.
		`CREATE TABLE IF NOT EXISTS agent_budgets (
	scope_id UUID NOT NULL,
	period VARCHAR(8) NOT NULL DEFAULT 'day',
	max_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
	max_tokens BIGINT NOT NULL DEFAULT 0,
	max_runs INT NOT NULL DEFAULT 0,
	is_workspace_default BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (scope_id, period)
)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// defaultScopes is the default-deny scope set for a new project.
func defaultScopes() map[string]bool {
	return map[string]bool{"monitor": false, "data_quality": false, "analyze_build": false, "growth_suggest": false}
}

// normalizeScopes keeps only known usecase capability groups and fills missing
// keys with the default-deny value. This makes the JSONB row additive: adding a
// future scope needs one new key here and old rows still read safely.
func normalizeScopes(scopes map[string]bool) map[string]bool {
	out := defaultScopes()
	for k, v := range scopes {
		if _, ok := out[k]; ok {
			out[k] = v
		}
	}
	return out
}

// GetAgentConfig returns the project's config (key redacted) for any member.
func (s *Store) GetAgentConfig(ctx context.Context, userID, projectID string) (AgentConfig, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentConfig{}, err
	}
	return s.readAgentConfig(ctx, project.ID)
}

// readAgentConfig loads the row (or a default-deny config when absent) without
// the ciphertext.
func (s *Store) readAgentConfig(ctx context.Context, projectID string) (AgentConfig, error) {
	cfg := AgentConfig{ProjectID: projectID, Provider: "openai", RedactPII: true, Autonomy: AutonomySuggest, Scopes: defaultScopes(), ModelFallback: true}
	var (
		cipher, liteCipher, proCipher           *string
		scopeMonitor, scopeDQ, scopeAB, scopeGS bool
	)
	err := s.pg.QueryRow(ctx, `
SELECT enabled, provider, model, base_url, redact_pii, api_key_ciphertext,
       scope_monitor, scope_data_quality, scope_analyze_build, scope_growth_suggest,
       autonomy, schedule_cron,
       lite_provider, lite_model, lite_base_url, lite_api_key_ciphertext,
       pro_provider, pro_model, pro_base_url, pro_api_key_ciphertext, model_fallback
FROM agent_configs WHERE project_id = $1`, projectID).Scan(
		&cfg.Enabled, &cfg.Provider, &cfg.Model, &cfg.BaseURL, &cfg.RedactPII, &cipher,
		&scopeMonitor, &scopeDQ, &scopeAB, &scopeGS, &cfg.Autonomy, &cfg.ScheduleCron,
		&cfg.LiteProvider, &cfg.LiteModel, &cfg.LiteBaseURL, &liteCipher,
		&cfg.ProProvider, &cfg.ProModel, &cfg.ProBaseURL, &proCipher, &cfg.ModelFallback)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return cfg, nil // no row yet → default-deny config
		}
		return AgentConfig{}, err
	}
	cfg.HasKey = cipher != nil && *cipher != ""
	cfg.LiteHasKey = liteCipher != nil && *liteCipher != ""
	cfg.ProHasKey = proCipher != nil && *proCipher != ""
	cfg.Scopes = normalizeScopes(map[string]bool{
		"monitor": scopeMonitor, "data_quality": scopeDQ,
		"analyze_build": scopeAB, "growth_suggest": scopeGS,
	})
	return cfg, nil
}

// UpsertAgentConfig writes the config. Only workspace owner/admin may mutate it;
// the change is recorded in the workspace audit log.
func (s *Store) UpsertAgentConfig(ctx context.Context, userID, projectID string, in AgentConfigInput) (AgentConfig, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentConfig{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentConfig{}, err
	}
	if !canManage {
		return AgentConfig{}, errAgentForbidden
	}

	scopes := normalizeScopes(in.Scopes)
	if in.Scopes == nil {
		current, err := s.readAgentConfig(ctx, project.ID)
		if err != nil {
			return AgentConfig{}, err
		}
		scopes = current.Scopes
	}
	autonomy := strings.TrimSpace(in.Autonomy)
	if autonomy == "" {
		autonomy = AutonomySuggest
	}
	if !ValidAutonomy(autonomy) {
		return AgentConfig{}, ErrInvalidAutonomy
	}

	// Only the project-level run-eligibility fields are written; the model_*
	// columns are left at their stored values (defaults on first insert), since the
	// model pool now lives on the workspace.
	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_configs (
	project_id, enabled, redact_pii,
	scope_monitor, scope_data_quality, scope_analyze_build, scope_growth_suggest,
	autonomy, schedule_cron
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (project_id) DO UPDATE SET
	enabled = EXCLUDED.enabled,
	redact_pii = EXCLUDED.redact_pii,
	scope_monitor = EXCLUDED.scope_monitor,
	scope_data_quality = EXCLUDED.scope_data_quality,
	scope_analyze_build = EXCLUDED.scope_analyze_build,
	scope_growth_suggest = EXCLUDED.scope_growth_suggest,
	autonomy = EXCLUDED.autonomy,
	schedule_cron = EXCLUDED.schedule_cron,
	updated_at = now()`,
		project.ID, in.Enabled, in.RedactPII,
		scopes["monitor"], scopes["data_quality"], scopes["analyze_build"], scopes["growth_suggest"],
		autonomy, strings.TrimSpace(in.ScheduleCron))
	if err != nil {
		return AgentConfig{}, err
	}
	payload, err := json.Marshal(scopes)
	if err != nil {
		return AgentConfig{}, err
	}
	_, err = s.pg.Exec(ctx, `
	INSERT INTO agent_capabilities (scope_id, scopes) VALUES ($1, $2)
	ON CONFLICT (scope_id) DO UPDATE SET scopes = EXCLUDED.scopes, updated_at = now()`, project.ID, payload)
	if err != nil {
		return AgentConfig{}, err
	}

	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.config.update", "project", project.ID, project.Name, "{}")
	return s.readAgentConfig(ctx, project.ID)
}

func (s *Store) readAgentCapabilityScopes(ctx context.Context, scopeID string) (map[string]bool, bool, error) {
	var raw []byte
	err := s.pg.QueryRow(ctx, `SELECT scopes FROM agent_capabilities WHERE scope_id = $1`, scopeID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	out := map[string]bool{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false, err
		}
	}
	return normalizeScopes(out), true, nil
}

// GetAgentCapabilities returns the usecase/tool capability scopes for the
// addressed agent. Missing rows inherit the project/default-agent scopes so old
// configs keep working until an agent-specific row is saved.
func (s *Store) GetAgentCapabilities(ctx context.Context, userID, projectID, agentID string) (AgentCapabilityConfig, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentCapabilityConfig{}, err
	}
	scopes, found, err := s.readAgentCapabilityScopes(ctx, scopeID)
	if err != nil {
		return AgentCapabilityConfig{}, err
	}
	if !found {
		cfg, err := s.readAgentConfig(ctx, project.ID)
		if err != nil {
			return AgentCapabilityConfig{}, err
		}
		scopes = cfg.Scopes
	}
	return AgentCapabilityConfig{ScopeID: scopeID, Scopes: normalizeScopes(scopes)}, nil
}

// UpsertAgentCapabilities writes an agent's usecase/tool capability scopes;
// workspace owner/admin only.
func (s *Store) UpsertAgentCapabilities(ctx context.Context, userID, projectID, agentID string, scopes map[string]bool) (AgentCapabilityConfig, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentCapabilityConfig{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentCapabilityConfig{}, err
	}
	if !canManage {
		return AgentCapabilityConfig{}, errAgentForbidden
	}
	clean := normalizeScopes(scopes)
	payload, err := json.Marshal(clean)
	if err != nil {
		return AgentCapabilityConfig{}, err
	}
	_, err = s.pg.Exec(ctx, `
	INSERT INTO agent_capabilities (scope_id, scopes) VALUES ($1, $2)
	ON CONFLICT (scope_id) DO UPDATE SET scopes = EXCLUDED.scopes, updated_at = now()`, scopeID, payload)
	if err != nil {
		return AgentCapabilityConfig{}, err
	}
	if isDefaultAgent(project.ID, scopeID) {
		_, err = s.pg.Exec(ctx, `
		INSERT INTO agent_configs (
			project_id, enabled, redact_pii,
			scope_monitor, scope_data_quality, scope_analyze_build, scope_growth_suggest,
			autonomy, schedule_cron
		) VALUES ($1,false,true,$2,$3,$4,$5,'suggest','')
		ON CONFLICT (project_id) DO UPDATE SET
			scope_monitor = EXCLUDED.scope_monitor,
			scope_data_quality = EXCLUDED.scope_data_quality,
			scope_analyze_build = EXCLUDED.scope_analyze_build,
			scope_growth_suggest = EXCLUDED.scope_growth_suggest,
			updated_at = now()`, project.ID,
			clean["monitor"], clean["data_quality"], clean["analyze_build"], clean["growth_suggest"])
		if err != nil {
			return AgentCapabilityConfig{}, err
		}
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.capabilities.update", "agent", scopeID, "", string(payload))
	return AgentCapabilityConfig{ScopeID: scopeID, Scopes: clean}, nil
}

// AgentCapabilitiesForRun resolves the usecase/tool capability scopes for the
// running agent. Missing rows inherit project config for backward compatibility.
func (s *Store) AgentCapabilitiesForRun(ctx context.Context, projectID, scopeID string) (map[string]bool, error) {
	scopes, found, err := s.readAgentCapabilityScopes(ctx, scopeID)
	if err != nil {
		return nil, err
	}
	if !found {
		cfg, err := s.readAgentConfig(ctx, projectID)
		if err != nil {
			return nil, err
		}
		scopes = normalizeScopes(cfg.Scopes)
	}
	// A non-default (granted) agent's access in this project is capped by its
	// project grant: it can never exceed what the workspace owner allowed here.
	// The default agent (scopeID == projectID) is the project's own access and is
	// never capped. An all-false grant is "no cap" (see anyScopeTrue).
	if scopeID != projectID {
		cap, capped, err := s.grantScopes(ctx, projectID, scopeID)
		if err != nil {
			return nil, err
		}
		if capped && anyScopeTrue(cap) {
			scopes = intersectScopes(scopes, cap)
		}
	}
	return scopes, nil
}

// AgentDefinition holds the per-scope SOUL.md + AGENTS.md (§14.1–2).
type AgentDefinition struct {
	ScopeID  string `json:"scope_id"`
	SoulMD   string `json:"soul_md"`
	AgentsMD string `json:"agents_md"`
}

// GetAgentDefinition returns the project's SOUL/AGENTS (empty when unset) for
// any member.
func (s *Store) GetAgentDefinition(ctx context.Context, userID, projectID, agentID string) (AgentDefinition, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentDefinition{}, err
	}
	def := AgentDefinition{ScopeID: scopeID}
	err = s.pg.QueryRow(ctx, `SELECT soul_md, agents_md FROM agent_definitions WHERE scope_id = $1`, scopeID).
		Scan(&def.SoulMD, &def.AgentsMD)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentDefinition{}, err
	}
	return def, nil
}

// UpsertAgentDefinition writes SOUL/AGENTS for the addressed agent (the project's
// default when agentID is empty); owner/admin only.
func (s *Store) UpsertAgentDefinition(ctx context.Context, userID, projectID, agentID, soulMD, agentsMD string) (AgentDefinition, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentDefinition{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentDefinition{}, err
	}
	if !canManage {
		return AgentDefinition{}, errAgentForbidden
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_definitions (scope_id, soul_md, agents_md) VALUES ($1, $2, $3)
ON CONFLICT (scope_id) DO UPDATE SET soul_md = EXCLUDED.soul_md, agents_md = EXCLUDED.agents_md, updated_at = now()`,
		scopeID, soulMD, agentsMD)
	if err != nil {
		return AgentDefinition{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.definition.update", "project", project.ID, project.Name, "{}")
	return AgentDefinition{ScopeID: scopeID, SoulMD: soulMD, AgentsMD: agentsMD}, nil
}

// resolveCipherArg maps an input key to the upsert ciphertext argument: blank
// returns nil (COALESCE leaves the stored key unchanged), "-" returns "" (clear),
// any other value is encrypted at rest.
func resolveCipherArg(rawKey string) (any, error) {
	switch strings.TrimSpace(rawKey) {
	case "":
		return nil, nil
	case "-":
		return "", nil
	default:
		ct, err := encryptAgentKey(rawKey)
		if err != nil {
			return nil, err
		}
		return ct, nil
	}
}

// AgentKeyForRun returns the decrypted API key for a project, for in-memory use
// at call time only. Never expose this over an API.
func (s *Store) AgentKeyForRun(ctx context.Context, projectID string) (string, error) {
	var cipher *string
	err := s.pg.QueryRow(ctx, `SELECT api_key_ciphertext FROM agent_configs WHERE project_id = $1`, projectID).Scan(&cipher)
	if err != nil {
		return "", err
	}
	if cipher == nil || *cipher == "" {
		return "", errors.New("no agent API key configured")
	}
	return decryptAgentKey(*cipher)
}

// --- AES-GCM key encryption (§7) ---

// agentEncKey derives a 32-byte AES key from AGENT_KEY_ENC_SECRET.
func agentEncKey() ([]byte, error) {
	secret := os.Getenv("AGENT_KEY_ENC_SECRET")
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("AGENT_KEY_ENC_SECRET is not set")
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

func encryptAgentKey(plaintext string) (string, error) {
	key, err := agentEncKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func decryptAgentKey(ciphertext string) (string, error) {
	key, err := agentEncKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
