package storage

import (
	"context"
	"encoding/json"
	"fmt"
)

// The marketplace is AgentRay's "start here" catalog. Packs are defined in
// internal/workloads (config only, versioned with the binary). This file is
// the persistence adapter: list/install/seed write into the existing
// per-agent tables through the same RBAC-checked setters a human editor uses,
// so a pack can never grant a capability the UI could not.

// AgentPresetSkill is one starter skill shipped inside an agent preset. It maps
// directly onto an active, enabled AgentSkill at install time.
type AgentPresetSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// AgentPreset is the JSON/persistence view of a workloads.Pack. Category
// groups presets in the UI (e.g. "growth", "marketing").
type AgentPreset struct {
	Slug        string             `json:"slug"`
	Name        string             `json:"name"`
	Tagline     string             `json:"tagline"`
	Description string             `json:"description"`
	Category    string             `json:"category"`
	Icon        string             `json:"icon"`
	SoulMD      string             `json:"-"`
	AgentsMD    string             `json:"-"`
	Scopes      map[string]bool    `json:"scopes"`
	Skills      []AgentPresetSkill `json:"skills"`
	// Tools are non-scope tool names (web_fetch, …) enabled at install. See
	// workloads.Pack.Tools for why a pack gets to ask for them at all.
	Tools []string `json:"tools,omitempty"`
}

// Pack catalog is owned by internal/workloads. The composition root injects it
// so this package never imports a layer above dataplane.
var (
	packList   func() []AgentPreset
	packLookup func(string) (AgentPreset, bool)
)

// SetPackCatalog wires the workloads registry into install/seed. Call once
// from app.New (and from tests that exercise marketplace install).
func SetPackCatalog(list func() []AgentPreset, lookup func(string) (AgentPreset, bool)) {
	packList, packLookup = list, lookup
}

// AgentPresets returns the system marketplace catalog (stable order).
func AgentPresets() []AgentPreset {
	if packList == nil {
		return nil
	}
	return packList()
}

// AgentPresetBySlug looks up a single preset.
func AgentPresetBySlug(slug string) (AgentPreset, bool) {
	if packLookup == nil {
		return AgentPreset{}, false
	}
	return packLookup(slug)
}

// InstallAgentPreset materializes a marketplace pack as a real agent in the
// project: it creates the agent (with a collision-free slug), writes its
// persona, grants its capability scopes, and installs its starter skills — each
// through the RBAC-checked setter, so an install carries exactly the permissions
// of the calling owner/admin. A failure after the agent row is created leaves a
// partially-configured agent rather than rolling back; that is recoverable in
// the UI and preferable to a half-deleted agent, but callers should surface the
// error.
func (s *Store) InstallAgentPreset(ctx context.Context, userID, projectID, slug string) (Agent, error) {
	preset, ok := AgentPresetBySlug(slug)
	if !ok {
		return Agent{}, fmt.Errorf("unknown agent preset %q", slug)
	}

	// Idempotent install: hiring a preset the project already has returns the
	// teammate already hired from it instead of minting a second one. This is
	// what stops the marketplace's "Install agent" button from silently
	// duplicating a hire on a double click or a repeat visit — without it, two
	// installs of the same preset produce two agents named identically, which
	// is exactly the "two Product Scouts" bug this link exists to prevent. A
	// project member can read this; only the write paths below require
	// owner/admin.
	if _, err := s.ProjectByIDForUser(ctx, userID, projectID); err != nil {
		return Agent{}, err
	}
	if existing, found, err := s.AgentByPresetSlug(ctx, projectID, preset.Slug); err != nil {
		return Agent{}, err
	} else if found {
		return existing, nil
	}

	agentSlug, err := s.freeAgentSlug(ctx, userID, projectID, preset.Slug)
	if err != nil {
		return Agent{}, err
	}
	agent, err := s.createAgent(ctx, userID, projectID, preset.Name, agentSlug, preset.Slug)
	if err != nil {
		return Agent{}, err
	}
	if _, err := s.UpsertAgentDefinition(ctx, userID, projectID, agent.ID, preset.SoulMD, preset.AgentsMD); err != nil {
		return agent, fmt.Errorf("install %s: definition: %w", slug, err)
	}
	if _, err := s.UpsertAgentCapabilities(ctx, userID, projectID, agent.ID, preset.Scopes); err != nil {
		return agent, fmt.Errorf("install %s: capabilities: %w", slug, err)
	}
	// Grant the new agent into its home project with the preset scopes. The agent
	// is owned by the workspace and can later be granted into sibling projects
	// without re-installing.
	if err := s.upsertAgentGrant(ctx, agent.ID, projectID, preset.Scopes); err != nil {
		return agent, fmt.Errorf("install %s: grant: %w", slug, err)
	}
	for _, sk := range preset.Skills {
		if _, err := s.UpsertAgentSkill(ctx, userID, projectID, agent.ID, AgentSkill{
			Name: sk.Name, Description: sk.Description, Body: sk.Body, Enabled: true,
		}); err != nil {
			return agent, fmt.Errorf("install %s: skill %q: %w", slug, sk.Name, err)
		}
	}
	// Non-scope tools last: the agent is already usable without them, so a
	// failure here degrades the hire rather than voiding it. Empty config — a
	// pack may only ask for tools that need none.
	for _, name := range preset.Tools {
		if err := s.UpsertAgentTool(ctx, userID, projectID, agent.ID, name, true, ""); err != nil {
			return agent, fmt.Errorf("install %s: tool %q: %w", slug, name, err)
		}
	}
	return agent, nil
}

// SeedDefaultFoundationAgent gives a brand-new project a capable default agent
// instead of a blank one: it seeds the Growth Lead pack as the project's
// default agent (scope_id == project_id) — persona, capability scopes, and
// starter skills — via direct, RBAC-free inserts (this runs at signup, before
// any session exists). It is idempotent: ON CONFLICT DO NOTHING means a project
// that already has a configured default agent is left untouched, so a returning
// owner never has their edits overwritten. The agent is seeded *disabled* (no
// model key yet); the user enables it once a model is configured.
func (s *Store) SeedDefaultFoundationAgent(ctx context.Context, projectID string) error {
	preset, ok := AgentPresetBySlug("growth-lead")
	if !ok {
		// Catalog not wired (unit tests that open storage without app.New).
		return nil
	}
	scopes := normalizeScopes(preset.Scopes)

	// The default agent's id equals the project id (isDefaultAgent). Create it
	// only if the project has no default agent yet.
	if _, err := s.pg.Exec(ctx, `
INSERT INTO agents (id, project_id, workspace_id, name, slug, is_default, enabled, autonomy)
SELECT $1, $1, p.workspace_id, $2, 'default', true, true, 'suggest'
FROM projects p WHERE p.id = $1
ON CONFLICT (id) DO NOTHING`, projectID, preset.Name); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO agent_configs (
	project_id, enabled, redact_pii,
	scope_monitor, scope_data_quality, scope_analyze_build, scope_growth_suggest,
	autonomy, schedule_cron
) VALUES ($1, true, true, $2, $3, $4, $5, 'suggest', '')
ON CONFLICT (project_id) DO NOTHING`, projectID,
		scopes["monitor"], scopes["data_quality"], scopes["analyze_build"], scopes["growth_suggest"]); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO agent_definitions (scope_id, soul_md, agents_md) VALUES ($1, $2, $3)
ON CONFLICT (scope_id) DO NOTHING`, projectID, preset.SoulMD, preset.AgentsMD); err != nil {
		return err
	}
	payload, err := json.Marshal(scopes)
	if err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO agent_capabilities (scope_id, scopes) VALUES ($1, $2)
ON CONFLICT (scope_id) DO NOTHING`, projectID, payload); err != nil {
		return err
	}
	for _, sk := range preset.Skills {
		if _, err := s.pg.Exec(ctx, `
INSERT INTO agent_skills (scope_id, name, description, body, enabled, status, origin)
SELECT $1, $2::text, $3, $4, true, 'active', 'user'
WHERE NOT EXISTS (SELECT 1 FROM agent_skills WHERE scope_id = $1 AND name = $2::text)`,
			projectID, sk.Name, sk.Description, sk.Body); err != nil {
			return err
		}
	}
	return nil
}

// freeAgentSlug returns the preset's preferred slug, or the first numbered
// variant ("growth-lead-2", "-3", …) that is not already taken in the
// project, so a preset can be installed more than once.
func (s *Store) freeAgentSlug(ctx context.Context, userID, projectID, base string) (string, error) {
	existing, err := s.ListAgents(ctx, userID, projectID)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(existing))
	for _, a := range existing {
		taken[a.Slug] = true
	}
	if !taken[base] {
		return base, nil
	}
	for n := 2; n < 100; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !taken[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find a free slug for %q", base)
}
