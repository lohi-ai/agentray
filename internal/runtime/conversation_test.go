package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

func msgEntry(id, role, text string) storage.AgentConversationEntry {
	p, _ := json.Marshal(convMessagePayload{Text: text})
	return storage.AgentConversationEntry{ID: id, Kind: ConvKindMessage, Role: role, PayloadJSON: string(p)}
}

func TestMessageEntryText(t *testing.T) {
	// A message entry yields its text (regenerate resends this verbatim).
	if got := MessageEntryText(msgEntry("1", "user", "resend me")); got != "resend me" {
		t.Fatalf("want %q, got %q", "resend me", got)
	}
	// Non-message kinds and unparsable payloads yield "".
	if got := MessageEntryText(storage.AgentConversationEntry{Kind: ConvKindToolTrace, PayloadJSON: `{"tool":"q"}`}); got != "" {
		t.Fatalf("non-message should yield empty, got %q", got)
	}
	if got := MessageEntryText(storage.AgentConversationEntry{Kind: ConvKindMessage, PayloadJSON: "not-json"}); got != "" {
		t.Fatalf("unparsable should yield empty, got %q", got)
	}
}

func TestEstimateTokensCharsOverFour(t *testing.T) {
	if got := estimateTokens("abcd"); got != 1 {
		t.Fatalf("want 1 token for 4 chars, got %d", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("want 0 tokens for empty, got %d", got)
	}
}

// --- /clear seam + forced compaction -------------------------------------

// The plan renderer is what /plan and the chat surface both read, so its three
// states have to be distinguishable at a glance.
func TestRenderPlanMarksTheStates(t *testing.T) {
	out := RenderPlan([]PlanItem{
		{Content: "read the schema", Status: "completed"},
		{Content: "write the query", Status: "in_progress"},
		{Content: "verify the numbers", Status: "pending"},
	})
	for _, want := range []string{"[x]", "~~read the schema~~", "**write the query**", "- [ ] verify the numbers"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered plan missing %q:\n%s", want, out)
		}
	}
	if RenderPlan(nil) != "" {
		t.Fatal("an empty plan must render as nothing")
	}
}
