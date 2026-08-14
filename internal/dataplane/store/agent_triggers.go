package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Per-agent triggers (AgentGarden §7): what starts a run. A schedule trigger
// fires on a cron; a webhook trigger exposes an unguessable token as its global
// ingress address and optionally authenticates the body with an HMAC secret (an
// agent_secrets entry). Both reuse the existing NATS run path — a webhook is just
// a second producer of run messages, not a new run engine. Triggers are keyed by
// scope_id = agent id, like the other per-agent tables; the legacy
// agent_configs.schedule_cron remains the default agent's schedule, so this table
// is purely additive.

const (
	// TriggerSchedule fires a run on a cron expression.
	TriggerSchedule = "schedule"
	// TriggerWebhook fires a run from an authenticated inbound HTTP request.
	TriggerWebhook = "webhook"
)

var errInvalidTriggerKind = errors.New("invalid trigger kind (must be 'schedule' or 'webhook')")
var errScheduleNeedsCron = errors.New("a schedule trigger needs a cron expression")

// AgentTrigger is one configured trigger for an agent.
type AgentTrigger struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Enabled        bool      `json:"enabled"`
	Cron           string    `json:"cron"`
	WebhookToken   string    `json:"webhook_token"`
	PromptTemplate string    `json:"prompt_template"`
	HMACSecretName string    `json:"hmac_secret_name"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// WebhookDispatch is the run-path resolution of an inbound webhook token: which
// agent to run, how to authenticate the body, and how to shape the prompt.
type WebhookDispatch struct {
	ProjectID      string
	AgentID        string
	PromptTemplate string
	HMACSecretName string
}

// ScheduledTrigger is one due-check candidate the scheduler evaluates each tick.
type ScheduledTrigger struct {
	ProjectID      string
	AgentID        string
	Cron           string
	PromptTemplate string
}

// newWebhookToken returns a 32-byte unguessable hex token used as a webhook's
// global ingress address (and, being unguessable, its bearer credential). Pure
// aside from the system CSPRNG.
func newWebhookToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// renderPrompt maps an inbound webhook body into the run prompt by substituting
// the {{body}} placeholder in the template. An empty template falls back to a
// default that hands the raw payload to the agent. Pure and unit-testable.
func renderPrompt(template, body string) string {
	if strings.TrimSpace(template) == "" {
		return "An inbound webhook fired. Handle this payload:\n\n" + body
	}
	return strings.ReplaceAll(template, "{{body}}", body)
}

// verifyWebhookHMAC reports whether signature is a valid hex HMAC-SHA256 of body
// under secret, compared in constant time. An empty secret means the trigger is
// unauthenticated beyond its unguessable token, so it accepts. An empty (but
// required) signature fails. Pure and unit-testable.
func verifyWebhookHMAC(secret, body, signature string) bool {
	if secret == "" {
		return true
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.TrimSpace(signature)))
}

// ListAgentTriggers returns an agent's triggers for any project member — the
// read surface the UI shows. The webhook token is returned (it is the address,
// not a stored secret); the HMAC secret is referenced by name only.
func (s *Store) ListAgentTriggers(ctx context.Context, userID, projectID, agentID string) ([]AgentTrigger, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, kind, enabled, cron, webhook_token, prompt_template, hmac_secret_name, created_at, updated_at
FROM agent_triggers WHERE scope_id = $1 ORDER BY kind ASC, created_at ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentTrigger, 0)
	for rows.Next() {
		var t AgentTrigger
		if err := rows.Scan(&t.ID, &t.Kind, &t.Enabled, &t.Cron, &t.WebhookToken, &t.PromptTemplate, &t.HMACSecretName, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateAgentTrigger adds a trigger to an agent (owner/admin only). A webhook
// trigger is assigned a fresh unguessable token (its global ingress address); a
// schedule trigger requires a cron. The returned row carries the token so the UI
// can show the webhook URL.
func (s *Store) CreateAgentTrigger(ctx context.Context, userID, projectID, agentID string, in AgentTrigger) (AgentTrigger, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentTrigger{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentTrigger{}, err
	}
	if !canManage {
		return AgentTrigger{}, errAgentForbidden
	}
	switch in.Kind {
	case TriggerSchedule:
		if strings.TrimSpace(in.Cron) == "" {
			return AgentTrigger{}, errScheduleNeedsCron
		}
		in.WebhookToken = ""
	case TriggerWebhook:
		token, gErr := newWebhookToken()
		if gErr != nil {
			return AgentTrigger{}, gErr
		}
		in.WebhookToken = token
		in.Cron = ""
	default:
		return AgentTrigger{}, errInvalidTriggerKind
	}
	var t AgentTrigger
	err = s.pg.QueryRow(ctx, `
INSERT INTO agent_triggers (scope_id, kind, enabled, cron, webhook_token, prompt_template, hmac_secret_name)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id::text, kind, enabled, cron, webhook_token, prompt_template, hmac_secret_name, created_at, updated_at`,
		scopeID, in.Kind, in.Enabled, in.Cron, in.WebhookToken, in.PromptTemplate, in.HMACSecretName).
		Scan(&t.ID, &t.Kind, &t.Enabled, &t.Cron, &t.WebhookToken, &t.PromptTemplate, &t.HMACSecretName, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return AgentTrigger{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.trigger.create", "project", project.ID, project.Name, "{}")
	return t, nil
}

// UpdateAgentTrigger edits a trigger's enabled flag, cron, prompt template, and
// HMAC secret name (owner/admin only). The kind and the webhook token are
// immutable: changing a token would silently break the ingress URL already
// handed out, so a new address means a new trigger.
func (s *Store) UpdateAgentTrigger(ctx context.Context, userID, projectID, agentID, triggerID string, in AgentTrigger) (AgentTrigger, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentTrigger{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentTrigger{}, err
	}
	if !canManage {
		return AgentTrigger{}, errAgentForbidden
	}
	var t AgentTrigger
	err = s.pg.QueryRow(ctx, `
UPDATE agent_triggers SET enabled = $3, cron = $4, prompt_template = $5, hmac_secret_name = $6, updated_at = now()
WHERE scope_id = $1 AND id = $2
RETURNING id::text, kind, enabled, cron, webhook_token, prompt_template, hmac_secret_name, created_at, updated_at`,
		scopeID, triggerID, in.Enabled, in.Cron, in.PromptTemplate, in.HMACSecretName).
		Scan(&t.ID, &t.Kind, &t.Enabled, &t.Cron, &t.WebhookToken, &t.PromptTemplate, &t.HMACSecretName, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return AgentTrigger{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.trigger.update", "project", project.ID, project.Name, "{}")
	return t, nil
}

// DeleteAgentTrigger removes a trigger (owner/admin only). For a webhook this
// permanently retires its ingress URL.
func (s *Store) DeleteAgentTrigger(ctx context.Context, userID, projectID, agentID, triggerID string) error {
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
	if _, err := s.pg.Exec(ctx, `DELETE FROM agent_triggers WHERE scope_id = $1 AND id = $2`, scopeID, triggerID); err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.trigger.delete", "project", project.ID, project.Name, "{}")
	return nil
}

// WebhookDispatchByToken resolves an inbound webhook token to the agent it runs,
// for the ingress path (no requesting user — the unguessable token is the
// credential). It returns ErrNoRows-equivalent (ok=false) for an unknown,
// disabled, or non-webhook token, so a bad token can never start a run.
func (s *Store) WebhookDispatchByToken(ctx context.Context, token string) (WebhookDispatch, bool, error) {
	if strings.TrimSpace(token) == "" {
		return WebhookDispatch{}, false, nil
	}
	var d WebhookDispatch
	err := s.pg.QueryRow(ctx, `
SELECT a.project_id::text, t.scope_id::text, t.prompt_template, t.hmac_secret_name
FROM agent_triggers t
JOIN agents a ON a.id = t.scope_id
WHERE t.webhook_token = $1 AND t.enabled = true AND t.kind = 'webhook' AND a.enabled = true`, token).
		Scan(&d.ProjectID, &d.AgentID, &d.PromptTemplate, &d.HMACSecretName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WebhookDispatch{}, false, nil
		}
		return WebhookDispatch{}, false, err
	}
	return d, true, nil
}

// WebhookRun is an authenticated, ready-to-dispatch webhook run.
type WebhookRun struct {
	ProjectID string
	AgentID   string
	Prompt    string
}

// ResolveWebhook turns a raw inbound webhook request into a dispatchable run: it
// resolves the token to an agent, authenticates the body against the trigger's
// HMAC secret (decrypted from the agent's own vault, never leaving storage), and
// renders the prompt. ok=false means reject (unknown/disabled token, or a bad
// signature) — the caller returns 401/404 and starts no run, so a forged request
// can never reach the agent. This is the one trusted entry point for the ingress
// handler, keeping the signature secret and crypto out of the HTTP layer.
func (s *Store) ResolveWebhook(ctx context.Context, token, body, signature string) (WebhookRun, bool, error) {
	d, ok, err := s.WebhookDispatchByToken(ctx, token)
	if err != nil || !ok {
		return WebhookRun{}, false, err
	}
	secret := ""
	if d.HMACSecretName != "" {
		secrets, sErr := s.AgentSecretsForRun(ctx, d.AgentID)
		if sErr != nil {
			return WebhookRun{}, false, sErr
		}
		// A configured-but-missing secret must fail closed: never downgrade to
		// unauthenticated because the named secret was deleted.
		v, present := secrets[d.HMACSecretName]
		if !present {
			return WebhookRun{}, false, nil
		}
		secret = v
	}
	if !verifyWebhookHMAC(secret, body, signature) {
		return WebhookRun{}, false, nil
	}
	return WebhookRun{ProjectID: d.ProjectID, AgentID: d.AgentID, Prompt: renderPrompt(d.PromptTemplate, body)}, true, nil
}

// ScheduledAgentTriggers returns the enabled schedule triggers across all agents
// — the per-agent candidate set the scheduler evaluates each tick, complementing
// the legacy project-level ScheduledAgentProjects (which still fires the default
// agent). Only enabled triggers on enabled agents are returned.
func (s *Store) ScheduledAgentTriggers(ctx context.Context) ([]ScheduledTrigger, error) {
	rows, err := s.pg.Query(ctx, `
SELECT a.project_id::text, t.scope_id::text, t.cron, t.prompt_template
FROM agent_triggers t
JOIN agents a ON a.id = t.scope_id
WHERE t.enabled = true AND t.kind = 'schedule' AND t.cron <> '' AND a.enabled = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ScheduledTrigger, 0)
	for rows.Next() {
		var st ScheduledTrigger
		if err := rows.Scan(&st.ProjectID, &st.AgentID, &st.Cron, &st.PromptTemplate); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
