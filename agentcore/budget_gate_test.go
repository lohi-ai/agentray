package agentcore

import (
	"context"
	"strings"
	"testing"
)

// toolCallWithCost scripts a tool-calling turn that also reports model cost, so a
// test can drive accumulated Usage across turns.
func toolCallWithCost(id, name, args string, cost float64) ChatResponse {
	r := AssistantToolCall(id, name, args)
	r.Usage = Usage{CostUSD: cost}
	return r
}

// TestBudgetGateGracefulStop proves the mid-run ceiling (#4b): a run whose own
// turns push accumulated cost over the cap gets exactly one tool-free wrap-up
// turn and then stops with StopReason "budget_exhausted" — it does not keep
// spending, and the model is told to summarize (tools are stripped for that
// turn).
func TestBudgetGateGracefulStop(t *testing.T) {
	work := &echoTool{name: "do_work"}
	faux := NewFauxProvider(
		toolCallWithCost("w1", "do_work", `{"step":1}`, 0.30), // usage after: 0.30
		toolCallWithCost("w2", "do_work", `{"step":2}`, 0.30), // usage after: 0.60 (>= cap)
		// The finalizing turn: a well-behaved model, seeing no tools, writes a
		// wrap-up. (Faux replays blindly; the loop having stripped schemas is what a
		// real provider would honor.)
		AssistantText("Completed step 1 and 2; next I would do step 3."),
		// Guard turn: must never be reached — if the run kept going it would call
		// work a third time and the assertion on work.called would fail.
		toolCallWithCost("w3", "do_work", `{"step":3}`, 0.30),
	)
	limits := DefaultLimits()
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(work),
		Policy:   NewAllowList("do_work"),
		Limits:   &limits,
		BudgetGate: func(_ context.Context, u Usage) bool {
			return u.CostUSD >= 0.50
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "do the work")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "budget_exhausted" {
		t.Fatalf("stop reason = %q, want budget_exhausted (turns=%d final=%q)", res.StopReason, res.Turns, res.Final)
	}
	if work.called != 2 {
		t.Fatalf("work called %d times; run must stop after the cap, not run turn 3", work.called)
	}
	if !strings.Contains(res.Final, "Completed step") {
		t.Fatalf("final answer should be the wrap-up summary, got %q", res.Final)
	}
	// The steer message must have been injected before the wrap-up turn.
	foundSteer := false
	for _, m := range res.Messages {
		if m.Role == RoleUser && strings.Contains(m.Content, "run budget for this period has been exhausted") {
			foundSteer = true
		}
	}
	if !foundSteer {
		t.Fatal("budget-exhausted steer message was not injected into history")
	}
}

// TestBudgetGateInactiveWhenUnderCap confirms an uncapped-enough run finishes
// normally: the gate never trips, no steer is injected, and the model reaches its
// own final answer.
func TestBudgetGateInactiveWhenUnderCap(t *testing.T) {
	work := &echoTool{name: "do_work"}
	faux := NewFauxProvider(
		toolCallWithCost("w1", "do_work", `{"step":1}`, 0.10),
		AssistantText("done"),
	)
	limits := DefaultLimits()
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(work),
		Policy:   NewAllowList("do_work"),
		Limits:   &limits,
		BudgetGate: func(_ context.Context, u Usage) bool {
			return u.CostUSD >= 100.0
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "do the work")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "done" {
		t.Fatalf("final = %q want normal completion", res.Final)
	}
	if res.StopReason == "budget_exhausted" {
		t.Fatal("budget gate tripped under cap")
	}
}
