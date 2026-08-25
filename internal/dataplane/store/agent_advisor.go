package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// agent_advisor.go — the per-agent switch for the reviewer that checks an
// agent's answer before its run is allowed to finish (agentcore/plugins/advisor).
//
// Kept separate from AgentTaskTiers on purpose. That map answers "which tier
// does this kind of work draw from"; this row answers "does this work happen at
// all", plus the review priorities. Folding the switch into the map would mean
// spelling "off" as a tier value, and an operator who wants the advisor on but
// cheaper would then have nothing to say.

// MaxAdvisorInstructionsBytes bounds the operator's review priorities. They are
// concatenated into the reviewer's system prompt on every review, so an
// unbounded field is an unbounded per-run cost.
const MaxAdvisorInstructionsBytes = 8000

// AgentAdvisor is one agent's advisor settings.
type AgentAdvisor struct {
	ScopeID string `json:"scope_id"`
	// Enabled turns the reviewer on for this agent's runs. Default false: the
	// advisor spends an extra pro-tier call per run, so it is opted into.
	Enabled bool `json:"enabled"`
	// Instructions are the review priorities — the risks this project wants
	// watched, the traps, the quality bar. It is the analog of oh-my-pi's
	// WATCHDOG.md: it reaches the REVIEWER's prompt only, never the agent's.
	// That separation is the point. Guidance useful to someone checking the
	// work ("distrust any revenue figure that isn't grouped by currency") is
	// usually too noisy to put in front of the agent doing it, which is why it
	// does not simply go in AGENTS.md.
	Instructions string `json:"instructions"`
}

// GetAgentAdvisor returns an agent's advisor settings for any project member.
// An absent row reads as disabled, which is the default for every agent that
// predates the feature.
func (s *Store) GetAgentAdvisor(ctx context.Context, userID, projectID, agentID string) (AgentAdvisor, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentAdvisor{}, err
	}
	return s.readAgentAdvisor(ctx, scopeID)
}

// readAgentAdvisor loads the stored row, or the zero (disabled) value.
func (s *Store) readAgentAdvisor(ctx context.Context, scopeID string) (AgentAdvisor, error) {
	out := AgentAdvisor{ScopeID: scopeID}
	err := s.pg.QueryRow(ctx,
		`SELECT enabled, instructions FROM agent_advisor WHERE scope_id = $1`, scopeID).
		Scan(&out.Enabled, &out.Instructions)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentAdvisor{}, err
	}
	return out, nil
}

// UpsertAgentAdvisor writes an agent's advisor settings; workspace owner/admin
// only, the same bar as the task→tier map it sits beside.
func (s *Store) UpsertAgentAdvisor(ctx context.Context, userID, projectID, agentID string, in AgentAdvisor) (AgentAdvisor, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentAdvisor{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentAdvisor{}, err
	}
	if !canManage {
		return AgentAdvisor{}, errAgentForbidden
	}

	out := AgentAdvisor{
		ScopeID:      scopeID,
		Enabled:      in.Enabled,
		Instructions: clampBytes(strings.TrimSpace(in.Instructions), MaxAdvisorInstructionsBytes),
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_advisor (scope_id, enabled, instructions) VALUES ($1, $2, $3)
ON CONFLICT (scope_id) DO UPDATE SET enabled = EXCLUDED.enabled, instructions = EXCLUDED.instructions, updated_at = now()`,
		scopeID, out.Enabled, out.Instructions)
	if err != nil {
		return AgentAdvisor{}, err
	}

	// Audit the switch, not the prose: turning a reviewer on or off changes what
	// the agent is allowed to hand over, which is the part worth a trail.
	detail, _ := json.Marshal(map[string]any{"enabled": out.Enabled, "instructions_bytes": len(out.Instructions)})
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.advisor.update", "agent", scopeID, "", string(detail))
	return out, nil
}

// AdvisorForRun resolves an agent's advisor settings on the system path (a run,
// no requesting user).
func (s *Store) AdvisorForRun(ctx context.Context, scopeID string) (AgentAdvisor, error) {
	return s.readAgentAdvisor(ctx, scopeID)
}

// clampBytes truncates to at most max bytes without splitting a multi-byte
// rune — the instructions are written in Vietnamese as often as English.
func clampBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b begins a UTF-8 sequence (i.e. is not a
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
