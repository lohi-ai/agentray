package agentruntime

// Real-provider test for the auto-improvement (reflection) pass — the capability
// that lets the agent learn from a working session. It exercises the exact
// prompt/parse path reflect() uses (reflectSystemPrompt + reflectUserPrompt →
// model → extractJSON → reflectOutput) against the operator's model, without a
// database: it proves the model, shown a real run history, returns a parseable
// improvement proposal that reflects what happened in the run.
//
// The DB write path (memory auto-apply with PII redaction, skill proposals for
// human approval) is mechanical and covered without a model elsewhere; the model
// half — does it actually produce useful, well-formed reflections — is what this
// confirms. Gated on the same env vars as the agentcore real tests; skips
// without them.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestReal_Reflection_ProposesImprovementFromRun(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("set AGENTRAY_TEST_OPENAI_BASE_URL, AGENTRAY_TEST_OPENAI_API_KEY, AGENTRAY_TEST_OPENAI_MODEL to run real-provider tests")
	}
	provider := agentcore.NewOpenAIProvider(key, base, agentcore.DefaultCompat())

	// A realistic working session: one tool blocked by policy, one query that
	// succeeded after the agent adapted. A good reflection should distill a
	// learning from this.
	res := agentcore.RunResult{
		Final: "Completed: weekly active users rose 12% after I switched from the blocked raw export " +
			"to the allowed run_sql aggregation query.",
		Tools: []agentcore.ToolTrace{
			{Tool: "export_raw_events", Allowed: false, Reason: "not in allow-list", Args: `{"table":"events"}`},
			{Tool: "run_sql", Allowed: true, Args: `{"sql":"select count(distinct user_id) ..."}`},
			{Tool: "create_chart", Allowed: true, Args: `{"type":"line","metric":"wau"}`},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := provider.Chat(ctx, agentcore.ChatRequest{
		Model:     model,
		MaxTokens: reflectMaxTokens,
		Messages: []agentcore.Message{
			{Role: agentcore.RoleSystem, Content: reflectSystemPrompt},
			{Role: agentcore.RoleUser, Content: reflectUserPrompt(res)},
		},
	})
	if err != nil {
		t.Fatalf("reflect Chat: %v", err)
	}
	t.Logf("raw reflection: %s", resp.Message.Content)

	// The reply must parse through the same path reflect() uses.
	var out reflectOutput
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
		t.Fatalf("reflection proposal did not parse: %v\nraw: %s", err, resp.Message.Content)
	}
	if len(out.Memories) == 0 && len(out.Skills) == 0 {
		t.Fatal("reflection proposed neither a memory nor a skill from a substantive run")
	}

	// The proposal should be about THIS run — not generic boilerplate. Look for a
	// reference to the actual events of the session in any proposed content.
	all := strings.ToLower(allProposalText(out))
	hit := strings.Contains(all, "run_sql") ||
		strings.Contains(all, "allow") || strings.Contains(all, "block") ||
		strings.Contains(all, "export") || strings.Contains(all, "wau") ||
		strings.Contains(all, "active user")
	if !hit {
		t.Fatalf("reflection did not reference the run's content; proposal=%s", all)
	}
	// Memory kinds, if any, must be ones reflect() accepts.
	for _, m := range out.Memories {
		k := agentcore.MemoryKind(m.Kind)
		if k != agentcore.MemoryFact && k != agentcore.MemoryLearning && k != agentcore.MemoryOutcome {
			t.Logf("note: proposed memory kind %q will be normalized to learning", m.Kind)
		}
	}
}

func allProposalText(out reflectOutput) string {
	var b strings.Builder
	for _, m := range out.Memories {
		b.WriteString(m.Content)
		b.WriteString(" ")
		b.WriteString(strings.Join(m.Tags, " "))
		b.WriteString(" ")
	}
	for _, s := range out.Skills {
		b.WriteString(s.Name + " " + s.Description + " " + s.Body + " ")
	}
	return b.String()
}
