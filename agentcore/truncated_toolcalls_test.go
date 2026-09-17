package agentcore

import (
	"context"
	"strings"
	"testing"
)

// TestTruncatedResponseNeverExecutesTools pins the truncated-output guard (pi's
// failToolCallsFromTruncatedMessage): a response cut off by the output-token
// limit can carry tool calls whose arguments are silently incomplete — the
// stream ended mid-JSON and the truncated tail may still parse. None of them
// run; each is answered with an error telling the model to re-issue, and the
// refusal is traced as not-allowed without spending tool-call budget.
func TestTruncatedResponseNeverExecutesTools(t *testing.T) {
	tool := &echoTool{name: "write_file"}
	faux := NewFauxProvider(
		ChatResponse{
			Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "c1", Name: "write_file", Arguments: `{"path":"a.go","content":"package main`},
			}},
			StopReason: "length",
		},
		AssistantText("re-issued with complete arguments"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("write_file"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "write the file")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if tool.called != 0 {
		t.Fatalf("a tool call from a truncated response must never execute, got %d calls", tool.called)
	}
	if res.Final != "re-issued with complete arguments" {
		t.Fatalf("final = %q", res.Final)
	}
	if len(res.Tools) != 1 || res.Tools[0].Allowed {
		t.Fatalf("expected 1 refused trace, got %+v", res.Tools)
	}
	// The refusal reaches the model as a tool result, so the transcript stays
	// provider-valid and the model can re-issue the call.
	var sawRefusal bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.ToolCallID == "c1" && strings.Contains(m.Content, "truncated") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatal("truncated call was not answered with a re-issue refusal")
	}
}

// TestTruncatedStopReasons covers the vendor-verbatim spellings the guard must
// catch: OpenAI-wire "length", Anthropic's "max_tokens", Codex's "incomplete".
func TestTruncatedStopReasons(t *testing.T) {
	for _, reason := range []string{"length", "max_tokens", "incomplete"} {
		if !isTruncatedStop(reason) {
			t.Fatalf("stop reason %q must be recognized as truncated", reason)
		}
	}
	for _, reason := range []string{"stop", "tool_calls", "error", "aborted", ""} {
		if isTruncatedStop(reason) {
			t.Fatalf("stop reason %q is not a truncation", reason)
		}
	}
}

// TestTruncatedFinalAnswerStillReturned verifies the guard only intercepts
// tool calls: a length-stopped response with no calls is still the run's
// answer (truncated text is better than none, and nothing unsafe can run).
func TestTruncatedFinalAnswerStillReturned(t *testing.T) {
	faux := NewFauxProvider(ChatResponse{
		Message:    Message{Role: RoleAssistant, Content: "partial ans"},
		StopReason: "length",
	})
	agent, err := New(Config{Provider: faux, Model: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "say something")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "partial ans" {
		t.Fatalf("final = %q, want the truncated text", res.Final)
	}
}
