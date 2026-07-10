package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// maxAlwaysLoadedBytes caps each always-loaded markdown part so the system
// prompt stays bounded (§14.8).
const maxAlwaysLoadedBytes = 8 * 1024

// Skill is a named, on-demand playbook authored as a SKILL.md (frontmatter +
// body). Selected by Description match (progressive disclosure) so only
// relevant skills enter context.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// SkillLoader fills selected skills with their full content only when needed.
// The runtime may pass nil when skills are already fully materialized.
type SkillLoader func(ctx context.Context, ids []string) (map[string]string, error)

// AgentDefinition is the complete user-authored description of one agent: the
// stable identity (Soul), the changeable mission/context (Agents), and the
// on-demand skills. Tools, hooks, permission, and memory are bound at the Agent
// level by the consumer, not authored here.
type AgentDefinition struct {
	ScopeID     string
	Soul        string      // SOUL.md — who the agent is (always loaded)
	Agents      string      // AGENTS.md — what it works on (always loaded)
	Skills      []Skill     // headers selected by description match; body loads on demand
	SkillLoader SkillLoader // optional deferred content loader for selected skills
}

// Severity ranks a load Diagnostic.
type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Diagnostic is a non-fatal problem found while loading a definition. A bad
// SKILL.md yields a warning, not a crashed run (pi load-diagnostics pattern).
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Part     string   `json:"part"`
	Message  string   `json:"message"`
}

// Validate returns diagnostics for an over-budget or malformed definition. It
// never fails the run; the caller surfaces warnings (e.g. in Settings) and
// proceeds with what loaded.
func (d AgentDefinition) Validate() []Diagnostic {
	var diags []Diagnostic
	if len(d.Soul) > maxAlwaysLoadedBytes {
		diags = append(diags, Diagnostic{SeverityWarning, "SOUL.md",
			fmt.Sprintf("exceeds %d-byte budget (%d); it will be truncated", maxAlwaysLoadedBytes, len(d.Soul))})
	}
	if len(d.Agents) > maxAlwaysLoadedBytes {
		diags = append(diags, Diagnostic{SeverityWarning, "AGENTS.md",
			fmt.Sprintf("exceeds %d-byte budget (%d); it will be truncated", maxAlwaysLoadedBytes, len(d.Agents))})
	}
	seen := map[string]bool{}
	for i, s := range d.Skills {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			diags = append(diags, Diagnostic{SeverityWarning, "skill",
				fmt.Sprintf("skill #%d has no name; skipped", i)})
			continue
		}
		if seen[name] {
			diags = append(diags, Diagnostic{SeverityWarning, "skill",
				fmt.Sprintf("duplicate skill name %q; later wins", name)})
		}
		seen[name] = true
	}
	return diags
}

// enabledSkills returns every enabled skill in definition order. Selection is no
// longer a keyword pre-filter: all enabled skill headers are advertised to the
// model and it loads the relevant body on demand via read_skill (progressive
// disclosure), so the runtime never has to guess relevance from the task string.
func (d AgentDefinition) enabledSkills() []Skill {
	var out []Skill
	for _, s := range d.Skills {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// DefinitionDraft is the structured result of an authoring-generation pass: a
// bounded pair of markdown documents, optional warnings, and nothing persisted.
// The HTTP authoring helper uses this shape so the UI can review/edit before save.
type DefinitionDraft struct {
	SoulMD   string   `json:"soul_md"`
	AgentsMD string   `json:"agents_md"`
	Warnings []string `json:"warnings,omitempty"`
}

const definitionDraftSystem = `You write starter agent definitions for non-technical operators.
Return JSON only, with keys "soul_md", "agents_md", and optional "warnings".

Rules:
- soul_md = stable identity, tone, boundaries, and non-negotiables.
- agents_md = mission, workflow, operating steps, escalation rules, and critical context.
- Keep both concise, clear, and practical.
- Do not mention JSON, schemas, or that you are an AI.
- Do not wrap output in markdown fences.
- warnings must be a short array only when important assumptions or missing details should be flagged.`

// DraftDefinition turns a free-text agent description into structured SOUL.md and
// AGENTS.md content. The provider must return strict JSON; malformed output fails
// closed so the caller never guesses how to split prose into the two files.
func DraftDefinition(ctx context.Context, provider LLMProvider, model, prompt string) (DefinitionDraft, error) {
	prompt = strings.TrimSpace(prompt)
	if provider == nil {
		return DefinitionDraft{}, fmt.Errorf("agentcore: provider is required")
	}
	if strings.TrimSpace(model) == "" {
		return DefinitionDraft{}, fmt.Errorf("agentcore: model is required")
	}
	if prompt == "" {
		return DefinitionDraft{}, fmt.Errorf("agentcore: prompt is required")
	}
	resp, err := provider.Chat(ctx, ChatRequest{
		Model: model,
		Messages: []Message{
			{Role: RoleSystem, Content: definitionDraftSystem},
			{Role: RoleUser, Content: prompt},
		},
		Temperature: 0.2,
		MaxTokens:   1200,
	})
	if err != nil {
		return DefinitionDraft{}, err
	}
	var out DefinitionDraft
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Message.Content)), &out); err != nil {
		return DefinitionDraft{}, fmt.Errorf("agentcore: invalid definition draft response")
	}
	out.SoulMD = strings.TrimSpace(out.SoulMD)
	out.AgentsMD = strings.TrimSpace(out.AgentsMD)
	if out.SoulMD == "" || out.AgentsMD == "" {
		return DefinitionDraft{}, fmt.Errorf("agentcore: definition draft missing soul_md or agents_md")
	}
	return out, nil
}
