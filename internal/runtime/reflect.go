package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// reflectInput carries everything the reflect pass needs.
type reflectInput struct {
	ProjectID string
	// ScopeID is the agent's own scope — where recall reads from and where
	// loadSkills reads from. Memories and skill proposals are filed here, not
	// under ProjectID; see applyMemories and applySkills.
	ScopeID  string
	RunID    string
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	// Memory is the store the pass writes through — the interface, not *PgMemory,
	// because the pass only ever calls Remember, and the scope it writes under is
	// the thing worth testing without a database behind it.
	Memory agentcore.MemoryStore
	Result agentcore.RunResult
}

// reflectMaxTokens caps the offline pass; reflection is token-bounded and never
// gains tool access (§14.9).
const reflectMaxTokens = 1024

// reflectMemory is one durable memory the reflection pass proposes.
type reflectMemory struct {
	Kind    string   `json:"kind"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

// reflectOutput is the structured proposal the model returns.
type reflectOutput struct {
	Memories []reflectMemory `json:"memories"`
	Skills   []reflectSkill  `json:"skills"`
}

// reflectSkill is one playbook the reflection pass proposes. It is recorded as
// a proposal, not auto-applied — skill writes are capability-adjacent.
type reflectSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// reflect runs the self-improvement pass (§14.9): the model reviews the run's
// own working history and proposes memory + skill writes. Memory writes
// auto-apply (low-risk, PII-redacted, reversible); skill writes are recorded as
// proposals for owner/admin approval. The pass is given no tools.
func (r *Runner) reflect(ctx context.Context, in reflectInput) error {
	if in.Memory == nil {
		return nil
	}
	provider, err := buildTracedProvider(in.Provider, in.BaseURL, in.APIKey, r.Tracer)
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
	in.applyMemories(ctx, out.Memories)

	// Skill writes are capability-adjacent → human-approved (recorded as proposals).
	in.applySkills(ctx, r.Store, out.Skills)
	return nil
}

// skillProposer is the write seam applySkills needs — ProposeAgentSkill on
// *storage.Store, narrowed so the pass can be tested without a database.
type skillProposer interface {
	ProposeAgentSkill(ctx context.Context, scopeID string, sk storage.AgentSkill) error
}

// applySkills records the pass's proposed skills under the AGENT's scope.
// Skills are read from that same scope (loadSkills → ActiveSkillHeadersForScope),
// so filing them under the project made every self-authored skill by a
// non-default agent invisible to the agent that proposed it — even after the
// owner approved it, because ApproveAgentSkill / ListAgentSkills already key
// on the agent scope. For the default agent the two ids are the same, so its
// behaviour is unchanged.
//
// Best-effort by design: a proposal that fails to persist must not fail the
// run that produced it, exactly as before.
func (in reflectInput) applySkills(ctx context.Context, store skillProposer, proposed []reflectSkill) {
	if store == nil {
		return
	}
	for _, s := range proposed {
		name := strings.TrimSpace(s.Name)
		if name == "" || strings.TrimSpace(s.Body) == "" {
			continue
		}
		_ = store.ProposeAgentSkill(ctx, in.ScopeID, storageSkill(name, s.Description, s.Body))
	}
}

// applyMemories persists the pass's proposed memories under the AGENT's
// scope. That scope — not the project's — is the one recall reads
// (agentcore/loop.go hands def.ScopeID to Recall), so filing them under the
// project made every reflection by a non-default agent write-only: it wrote to
// the project and read from itself, and nothing it learned ever came back. For
// the default agent the two ids are the same, so its behaviour is unchanged.
//
// Best-effort by design: a memory that fails to persist must not fail the run
// that produced it, exactly as before.
func (in reflectInput) applyMemories(ctx context.Context, proposed []reflectMemory) {
	if in.Memory == nil {
		return
	}
	for _, m := range proposed {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		kind := agentcore.MemoryKind(m.Kind)
		if kind != agentcore.MemoryFact && kind != agentcore.MemoryLearning && kind != agentcore.MemoryOutcome {
			kind = agentcore.MemoryLearning
		}
		_ = in.Memory.Remember(ctx, agentcore.MemoryEntry{
			ScopeID: in.ScopeID, Kind: kind, Content: content, Tags: m.Tags,
			Confidence: 0.6, SourceRun: in.RunID,
		})
	}
}

const reflectSystemPrompt = `You are the reflection pass of an analytics agent. You do NOT act or call tools.
Review the run history and label what worked (reinforce) and what failed or wasted effort (avoid).
Then propose: (1) durable memories — distilled facts/learnings/outcomes worth recalling in future runs;
(2) at most one skill — a reusable playbook for a repeated successful sequence, or a guardrail for a repeated failure.
Respond with ONLY a JSON object of this exact shape, no prose:
{"memories":[{"kind":"fact|learning|outcome","content":"...","tags":["..."]}],"skills":[{"name":"kebab-case","description":"when to use","body":"numbered steps"}]}
Keep memories specific and free of personal data. If nothing is worth saving, return empty arrays.`

// How much of a run's tool trace the reflection prompt is allowed to render.
//
// The trace is bounded only by Limits.MaxToolCalls, which a long autonomous run
// raises into the thousands, so "render every call" is not a prompt size — it is
// a function of how long the agent worked. A 4200-turn run produces ~4200 lines
// of up to ~230 characters: a megabyte of input to a pass whose entire output is
// capped at 1024 tokens, and past any model's window long before that, so the
// reflection pass fails outright on exactly the long runs with the most to learn
// from.
//
// The fix is not a smaller slice of the same list. Reflection is looking for
// REPEATED successful sequences and REPEATED failures, and a tally answers that
// question better than any window of individual calls does — a tool that failed
// 300 times is one line, not 300, and its 300 lines would have crowded out
// everything else. So the tally is always rendered, and the individual calls
// below it are the evidence: failures first, because a guardrail needs the
// specific error text, then the most recent calls, because that is where a run's
// final state was decided.
const (
	reflectMaxFailureLines = 40
	reflectMaxRecentLines  = 60
)

// toolTally is one row of the per-tool aggregate.
type toolTally struct {
	tool    string
	calls   int
	errors  int
	blocked int
}

// reflectUserPrompt renders a compact, size-bounded view of the run for the
// model: the final answer, a per-tool tally of the whole trace, and a bounded
// sample of the individual calls worth reading verbatim.
func reflectUserPrompt(res agentcore.RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run summary: %s\n\nTool calls (%d):\n", truncate(res.Final, 1500), len(res.Tools))

	// Tally in first-seen order, so the summary reads in the order the run
	// actually used its tools rather than in map order (which would make the
	// same run render differently every time).
	order := make([]string, 0, 8)
	byTool := make(map[string]*toolTally, 8)
	var failures []agentcore.ToolTrace
	for _, t := range res.Tools {
		row, ok := byTool[t.Tool]
		if !ok {
			row = &toolTally{tool: t.Tool}
			byTool[t.Tool] = row
			order = append(order, t.Tool)
		}
		row.calls++
		switch {
		case !t.Allowed:
			row.blocked++
			failures = append(failures, t)
		case t.Error != "":
			row.errors++
			failures = append(failures, t)
		}
	}
	for _, name := range order {
		row := byTool[name]
		fmt.Fprintf(&b, "- %s: %d calls", row.tool, row.calls)
		if row.errors > 0 {
			fmt.Fprintf(&b, ", %d errored", row.errors)
		}
		if row.blocked > 0 {
			fmt.Fprintf(&b, ", %d blocked", row.blocked)
		}
		b.WriteString("\n")
	}

	// Failures, oldest first: the first time a tool broke usually explains the
	// ones after it, and a guardrail written from the first failure covers the
	// repeats.
	if len(failures) > 0 {
		shown := failures
		if len(shown) > reflectMaxFailureLines {
			shown = shown[:reflectMaxFailureLines]
		}
		fmt.Fprintf(&b, "\nFailed or blocked calls (%d of %d):\n", len(shown), len(failures))
		for _, t := range shown {
			fmt.Fprintf(&b, "- %s\n", reflectTraceLine(t))
		}
	}

	// The tail of the run. Skipped when the whole trace is already shorter than
	// the window, since every one of those calls is in the tally above and
	// listing them again is noise.
	if len(res.Tools) > 0 {
		recent := res.Tools
		if len(recent) > reflectMaxRecentLines {
			recent = recent[len(recent)-reflectMaxRecentLines:]
			fmt.Fprintf(&b, "\nMost recent %d calls:\n", len(recent))
		} else {
			b.WriteString("\nCalls in order:\n")
		}
		for _, t := range recent {
			fmt.Fprintf(&b, "- %s\n", reflectTraceLine(t))
		}
	}
	return b.String()
}

// reflectTraceLine renders one tool call the way the prompt has always shown
// them: name, truncated args, outcome.
func reflectTraceLine(t agentcore.ToolTrace) string {
	status := "ok"
	if !t.Allowed {
		status = "blocked:" + t.Reason
	} else if t.Error != "" {
		status = "error:" + t.Error
	}
	return fmt.Sprintf("%s(%s) -> %s", t.Tool, truncate(t.Args, 200), status)
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
