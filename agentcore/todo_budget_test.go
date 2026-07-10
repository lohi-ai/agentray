package agentcore

import (
	"context"
	"testing"
)

// TestPlanUpdatesDoNotStarveTurnBudget is the long-running fix proven in NEBULA:
// a turn spent only on update_plan is bookkeeping, not productive work, so it
// must not consume the MaxTurns budget. With MaxTurns=3 the model interleaves
// three plan updates with three real tool calls (6 turns) and still reaches its
// final answer — without the refund it would stop at "max_turns" mid-task.
func TestPlanUpdatesDoNotStarveTurnBudget(t *testing.T) {
	store := NewTodoStore()
	work := &echoTool{name: "do_work"}
	faux := NewFauxProvider(
		AssistantToolCall("p1", ToolUpdatePlan, `{"items":[{"content":"a","status":"in_progress"}]}`),
		AssistantToolCall("w1", "do_work", `{"step":1}`),
		AssistantToolCall("p2", ToolUpdatePlan, `{"items":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"}]}`),
		AssistantToolCall("w2", "do_work", `{"step":2}`),
		AssistantToolCall("p3", ToolUpdatePlan, `{"items":[{"content":"b","status":"completed"}]}`),
		AssistantText("all three steps complete"),
	)
	limits := DefaultLimits()
	limits.MaxTurns = 3
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(work, NewTodoTool(store)),
		Policy:   NewAllowList("do_work", ToolUpdatePlan),
		Limits:   &limits,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "do the work in steps")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "all three steps complete" {
		t.Fatalf("run stopped early (%q, stop=%q); plan turns starved the budget", res.Final, res.StopReason)
	}
	if work.called != 2 {
		t.Fatalf("expected 2 real work calls, got %d", work.called)
	}
}

// TestPlanOnlyLoopStillBounded guards the refund: a model that ONLY ever updates
// the plan must not loop forever. The MaxToolCalls budget is the backstop, so the
// run halts cleanly at max_tool_calls rather than spinning.
func TestPlanOnlyLoopStillBounded(t *testing.T) {
	store := NewTodoStore()
	resp := make([]ChatResponse, 0, 50)
	for i := 0; i < 50; i++ {
		resp = append(resp, AssistantToolCall("p", ToolUpdatePlan, `{"items":[{"content":"x","status":"in_progress"}]}`))
	}
	faux := NewFauxProvider(resp...)
	limits := DefaultLimits()
	limits.MaxTurns = 5
	limits.MaxToolCalls = 6
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(NewTodoTool(store)),
		Policy:   NewAllowList(ToolUpdatePlan),
		Limits:   &limits,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "just keep planning")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "max_tool_calls" {
		t.Fatalf("plan-only loop must be bounded by MaxToolCalls, got stop=%q turns=%d", res.StopReason, res.Turns)
	}
}
