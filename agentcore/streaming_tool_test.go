package agentcore

import (
	"context"
	"testing"
)

// streamingProbe is a StreamingTool that emits a fixed set of partials before
// returning its final result.
type streamingProbe struct{ partials []string }

func (streamingProbe) Name() string { return "probe" }
func (streamingProbe) Schema() ToolSchema {
	return ToolSchema{Name: "probe", Description: "streams partials", Parameters: map[string]any{"type": "object"}}
}
func (streamingProbe) Run(context.Context, string) (string, error) { return "final result", nil }
func (p streamingProbe) RunStreaming(_ context.Context, _ string, emit func(string)) (string, error) {
	for _, s := range p.partials {
		emit(s)
	}
	return "final result", nil
}

// TestStreamingToolEmitsPartials verifies a streaming tool's partials reach the
// sink as tool_execution_update events before the final tool_execution_end, and
// that the final result is unchanged.
func TestStreamingToolEmitsPartials(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "probe", `{}`),
		AssistantText("ok"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(streamingProbe{partials: []string{"25%", "50%", "75%"}}),
		Policy:   NewAllowList("probe"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var updates []string
	var sawEndAfterUpdates bool
	var updateCount, endCount int
	sink := func(ev StreamEvent) {
		switch ev.Type {
		case StreamToolExecUpdate:
			updates = append(updates, ev.Note)
			updateCount++
		case StreamToolExecEnd:
			endCount++
			if updateCount >= 2 {
				sawEndAfterUpdates = true
			}
		}
	}
	res, err := agent.PromptStream(context.Background(), "go", sink)
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	if len(updates) < 2 {
		t.Fatalf("expected >=2 partials, got %v", updates)
	}
	if !sawEndAfterUpdates {
		t.Fatalf("tool_execution_end did not follow the partials")
	}
	// Final tool result fed to the model is the authoritative value, not a partial.
	var sawFinal bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.Content == "final result" {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Fatalf("final tool result missing or overwritten by a partial: %+v", res.Messages)
	}
}

// TestNonStreamingToolNoUpdates verifies a plain tool produces no
// tool_execution_update events (the partial path is opt-in).
func TestNonStreamingToolNoUpdates(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("ok"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var updates int
	sink := func(ev StreamEvent) {
		if ev.Type == StreamToolExecUpdate {
			updates++
		}
	}
	if _, err := agent.PromptStream(context.Background(), "go", sink); err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	if updates != 0 {
		t.Fatalf("non-streaming tool emitted %d updates, want 0", updates)
	}
}
