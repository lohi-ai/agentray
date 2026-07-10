package agentcore

import (
	"context"
	"testing"
)

// TestPrepareNextTurnSwapsModel verifies the save-point hook can bump the model
// between turns: turn-1's request keeps the original model (the in-flight request
// is untouched), turn-2's request uses the new one.
func TestPrepareNextTurnSwapsModel(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`), // turn 1 -> loop continues
		AssistantText("done"),                 // turn 2 -> final
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "model-a",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
		PrepareNextTurn: func(_ context.Context, s TurnState) TurnState {
			s.Model = "model-b" // bump for the next turn
			return s
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) < 2 {
		t.Fatalf("expected 2 turns, got %d", len(faux.Recorded))
	}
	if got := faux.Recorded[0].Model; got != "model-a" {
		t.Fatalf("turn-1 request model = %q, want model-a (in-flight untouched)", got)
	}
	if got := faux.Recorded[1].Model; got != "model-b" {
		t.Fatalf("turn-2 request model = %q, want model-b (save-point applied)", got)
	}
}

// TestPrepareNextTurnEmptyKeepsCurrent verifies a hook returning zero-value
// fields does not blank the run's model or system prompt.
func TestPrepareNextTurnEmptyKeepsCurrent(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "model-a",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
		PrepareNextTurn: func(_ context.Context, _ TurnState) TurnState {
			return TurnState{} // careless hook: everything zero
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := faux.Recorded[1].Model; got != "model-a" {
		t.Fatalf("turn-2 model = %q, want model-a (empty hook must not blank it)", got)
	}
}
