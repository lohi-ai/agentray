package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// reflectInput carries everything the reflect pass needs.
type reflectInput struct {
	ProjectID string
	RunID     string
	Provider  string
	Model     string
	BaseURL   string
	APIKey    string
	Memory    *PgMemory
	Result    agentcore.RunResult
}

// reflectMaxTokens caps the offline pass; reflection is token-bounded and never
// gains tool access (§14.9).
const reflectMaxTokens = 1024

// reflectOutput is the structured proposal the model returns.
type reflectOutput struct {
	Memories []struct {
		Kind    string   `json:"kind"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	} `json:"memories"`
	Skills []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	} `json:"skills"`
}

// reflect runs the self-improvement pass (§14.9): the model reviews the run's
// own working history and proposes memory + skill writes. Memory writes
// auto-apply (low-risk, PII-redacted, reversible); skill writes are recorded as
// proposals for owner/admin approval. The pass is given no tools.
func (r *Runner) reflect(ctx context.Context, in reflectInput) error {
	if in.Memory == nil {
		return nil
	}
	provider, err := buildProvider(in.Provider, in.BaseURL, in.APIKey, r.Tracer)
	if err != nil {
		return err
	}

	resp, err := provider.Chat(ctx, agentcore.ChatRequest{
		Model:     in.Model,
		MaxTokens: reflectMaxTokens,
		Messages: []agentcore.Message{
			{Role: agentcore.RoleSystem, Content: reflectSystemPrompt},
			{Role: agentcore.RoleUser, Content: reflectUserPrompt(in.Result)},
		},
	})
	if err != nil {
		return err
	}

	var out reflectOutput
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
		return fmt.Errorf("reflect: unparseable proposal: %w", err)
	}

	// Memory writes auto-apply (PgMemory redacts PII on the write path).
	for _, m := range out.Memories {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		kind := agentcore.MemoryKind(m.Kind)
		if kind != agentcore.MemoryFact && kind != agentcore.MemoryLearning && kind != agentcore.MemoryOutcome {
			kind = agentcore.MemoryLearning
		}
		_ = in.Memory.Remember(ctx, agentcore.MemoryEntry{
			ScopeID: in.ProjectID, Kind: kind, Content: content, Tags: m.Tags,
			Confidence: 0.6, SourceRun: in.RunID,
		})
	}

	// Skill writes are capability-adjacent → human-approved (recorded as proposals).
	for _, s := range out.Skills {
		name := strings.TrimSpace(s.Name)
		if name == "" || strings.TrimSpace(s.Body) == "" {
			continue
		}
		_ = r.Store.ProposeAgentSkill(ctx, in.ProjectID, storageSkill(name, s.Description, s.Body))
	}
	return nil
}

const reflectSystemPrompt = `You are the reflection pass of an analytics agent. You do NOT act or call tools.
Review the run history and label what worked (reinforce) and what failed or wasted effort (avoid).
Then propose: (1) durable memories — distilled facts/learnings/outcomes worth recalling in future runs;
(2) at most one skill — a reusable playbook for a repeated successful sequence, or a guardrail for a repeated failure.
Respond with ONLY a JSON object of this exact shape, no prose:
{"memories":[{"kind":"fact|learning|outcome","content":"...","tags":["..."]}],"skills":[{"name":"kebab-case","description":"when to use","body":"numbered steps"}]}
Keep memories specific and free of personal data. If nothing is worth saving, return empty arrays.`

// reflectUserPrompt renders a compact view of the run for the model.
func reflectUserPrompt(res agentcore.RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run summary: %s\n\nTool calls (%d):\n", truncate(res.Final, 1500), len(res.Tools))
	for _, t := range res.Tools {
		status := "ok"
		if !t.Allowed {
			status = "blocked:" + t.Reason
		} else if t.Error != "" {
			status = "error:" + t.Error
		}
		fmt.Fprintf(&b, "- %s(%s) -> %s\n", t.Tool, truncate(t.Args, 200), status)
	}
	return b.String()
}

// extractJSON pulls the first top-level JSON object out of a model reply,
// tolerating code fences or surrounding prose.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return "{}"
}
