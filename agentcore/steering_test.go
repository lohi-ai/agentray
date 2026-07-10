package agentcore

import (
	"context"
	"strings"
	"testing"
)

// TestSteeringInjectedBeforeNextTurn verifies a steering message queued during
// turn 1 appears in the turn-2 request, ahead of the model's reasoning.
func TestSteeringInjectedBeforeNextTurn(t *testing.T) {
	// Turn 1: model calls a (permitted) no-op tool so the loop continues; turn 2:
	// final answer. Steering is queued once, drained on turn 2.
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("ok"),
	)
	var delivered bool
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
		GetSteeringMessages: func(context.Context) []Message {
			if delivered {
				return nil
			}
			delivered = true
			return []Message{{Role: RoleUser, Content: "STEER: prefer option B"}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// The second recorded request must contain the steering message.
	if len(faux.Recorded) < 2 {
		t.Fatalf("expected at least 2 turns, got %d", len(faux.Recorded))
	}
	var seen bool
	for _, m := range faux.Recorded[1].Messages {
		if strings.Contains(m.Content, "STEER: prefer option B") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("steering message not present in turn-2 request: %+v", faux.Recorded[1].Messages)
	}
}

// TestFollowUpRestartsLoop verifies a follow-up queued after the final answer
// restarts the loop instead of ending the run.
func TestFollowUpRestartsLoop(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("first answer"),
		AssistantText("second answer"),
	)
	var sent bool
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		GetFollowUpMessages: func(context.Context) []Message {
			if sent {
				return nil
			}
			sent = true
			return []Message{{Role: RoleUser, Content: "now do the next thing"}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "second answer" {
		t.Fatalf("loop did not restart on follow-up: final=%q turns=%d", res.Final, res.Turns)
	}
	if res.Turns != 2 {
		t.Fatalf("expected 2 turns after one follow-up, got %d", res.Turns)
	}
	// The follow-up must have entered the second request.
	var seen bool
	for _, m := range faux.Recorded[1].Messages {
		if strings.Contains(m.Content, "now do the next thing") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("follow-up not present in restarted turn: %+v", faux.Recorded[1].Messages)
	}
}

// TestFollowUpRespectsMaxTurns verifies an always-on follow-up queue cannot loop
// past the turn budget.
func TestFollowUpRespectsMaxTurns(t *testing.T) {
	faux := NewFauxProvider() // always returns "(end)" / stop
	limits := DefaultLimits()
	limits.MaxTurns = 3
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Limits:   &limits,
		GetFollowUpMessages: func(context.Context) []Message {
			return []Message{{Role: RoleUser, Content: "again"}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Turns != 3 || res.StopReason != "max_turns" {
		t.Fatalf("follow-up loop ignored budget: turns=%d stop=%q", res.Turns, res.StopReason)
	}
}

// noopTool is a permitted do-nothing tool used to keep the loop alive for a turn.
type noopTool struct{}

func (noopTool) Name() string { return "noop" }
func (noopTool) Schema() ToolSchema {
	return ToolSchema{Name: "noop", Description: "does nothing", Parameters: map[string]any{"type": "object"}}
}
func (noopTool) Run(context.Context, string) (string, error) { return "ok", nil }
