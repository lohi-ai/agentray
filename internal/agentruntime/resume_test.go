package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// TestCloseDanglingCallsSynthesizesResults verifies a transcript with an
// assistant tool call that never got a result is closed with a synthesized tool
// message, so a provider will accept the replayed history.
func TestCloseDanglingCallsSynthesizesResults(t *testing.T) {
	in := []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "how many signups?"},
		{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{ID: "c1", Name: "query"}}},
		// no tool result for c1 — the run crashed mid-flight
	}
	out := closeDanglingCalls(in)
	if len(out) != 3 {
		t.Fatalf("want 3 messages (orig 2 + 1 synthesized), got %d: %+v", len(out), out)
	}
	last := out[2]
	if last.Role != agentcore.RoleTool || last.ToolCallID != "c1" || last.Name != "query" {
		t.Fatalf("synthesized result malformed: %+v", last)
	}
	if last.Content == "" {
		t.Fatal("synthesized tool result must carry an interrupted note")
	}
}

// TestCloseDanglingCallsLeavesSatisfiedCalls verifies an already-answered call is
// untouched and no spurious result is added.
func TestCloseDanglingCallsLeavesSatisfiedCalls(t *testing.T) {
	in := []agentcore.Message{
		{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{ID: "c1", Name: "query"}}},
		{Role: agentcore.RoleTool, ToolCallID: "c1", Name: "query", Content: "42"},
	}
	out := closeDanglingCalls(in)
	if len(out) != 2 {
		t.Fatalf("a satisfied call must not be re-closed, got %d messages: %+v", len(out), out)
	}
}

// TestLastUserPrompt verifies the most recent user turn is returned for recall.
func TestLastUserPrompt(t *testing.T) {
	msgs := []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "first"},
		{Role: agentcore.RoleAssistant, Content: "ok"},
		{Role: agentcore.RoleUser, Content: "second"},
		{Role: agentcore.RoleAssistant, Content: "done"},
	}
	if got := lastUserPrompt(msgs); got != "second" {
		t.Fatalf("lastUserPrompt = %q, want \"second\"", got)
	}
	if got := lastUserPrompt(nil); got != "" {
		t.Fatalf("lastUserPrompt(nil) = %q, want empty", got)
	}
}

// TestNormalizeProvider verifies the empty label folds to openai and matching is
// case-insensitive, so a key refresh matches the provider an agentcore provider
// reports.
func TestNormalizeProvider(t *testing.T) {
	cases := map[string]string{
		"":          "openai",
		"  ":        "openai",
		"OpenAI":    "openai",
		"Anthropic": "anthropic",
		"myrouter":  "myrouter",
	}
	for in, want := range cases {
		if got := normalizeProvider(in); got != want {
			t.Fatalf("normalizeProvider(%q) = %q, want %q", in, got, want)
		}
	}
}
