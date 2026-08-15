// Package authoring generates starter agent definitions from a free-text
// description. It is authoring-time, not run-time: nothing here participates in
// executing an agent, and the loop never names any of it.
//
// That is why it sits beside agentcore rather than under it. The kernel tree is
// the loop and the capabilities it dispatches to — agentcore/ holds the
// vocabulary read every turn (AgentDefinition, Limits, Env) and agentcore/plugins/
// holds what extends it. This package is neither: it only produces the markdown
// a human reviews and saves before any run exists. It makes its own provider call
// and persists nothing.
//
// The direction of the dependency is the whole point. authoring imports
// agentcore to speak its vocabulary; agentcore must never learn that an
// authoring step exists, because a definition typed by hand and one drafted here
// have to be indistinguishable to the loop.
package authoring

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// Draft is the structured result of an authoring-generation pass: a bounded
// pair of markdown documents, optional warnings, and nothing persisted. The
// HTTP authoring helper uses this shape so the UI can review/edit before save.
type Draft struct {
	SoulMD   string   `json:"soul_md"`
	AgentsMD string   `json:"agents_md"`
	Warnings []string `json:"warnings,omitempty"`
}

const draftSystem = `You write starter agent definitions for non-technical operators.
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
func DraftDefinition(ctx context.Context, provider agentcore.LLMProvider, model, prompt string) (Draft, error) {
	prompt = strings.TrimSpace(prompt)
	if provider == nil {
		return Draft{}, fmt.Errorf("agentcore: provider is required")
	}
	if strings.TrimSpace(model) == "" {
		return Draft{}, fmt.Errorf("agentcore: model is required")
	}
	if prompt == "" {
		return Draft{}, fmt.Errorf("agentcore: prompt is required")
	}
	resp, err := provider.Chat(ctx, agentcore.ChatRequest{
		Model: model,
		Messages: []agentcore.Message{
			{Role: agentcore.RoleSystem, Content: draftSystem},
			{Role: agentcore.RoleUser, Content: prompt},
		},
		Temperature: 0.2,
		MaxTokens:   1200,
	})
	if err != nil {
		return Draft{}, err
	}
	var out Draft
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Message.Content)), &out); err != nil {
		return Draft{}, fmt.Errorf("agentcore: invalid definition draft response")
	}
	out.SoulMD = strings.TrimSpace(out.SoulMD)
	out.AgentsMD = strings.TrimSpace(out.AgentsMD)
	if out.SoulMD == "" || out.AgentsMD == "" {
		return Draft{}, fmt.Errorf("agentcore: definition draft missing soul_md or agents_md")
	}
	return out, nil
}
