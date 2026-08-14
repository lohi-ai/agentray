package todo_test

import (
	"context"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"testing"
)

// TestPlanUpdatesDoNotStarveTurnBudget is the long-running fix proven in NEBULA:
// a turn spent only on update_plan is bookkeeping, not productive work, so it
// must not consume the MaxTurns budget. With MaxTurns=3 the model interleaves
// three plan updates with three real tool calls (6 turns) and still reaches its
// final answer — without the refund it would stop at "max_turns" mid-task.
func TestPlanUpdatesDoNotStarveTurnBudget(t *testing.T) {
	store := todo.NewStore()
	work := &echoTool{name: "do_work"}
	faux := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("p1", todo.ToolName, `{"items":[{"content":"a","status":"in_progress"}]}`),
		agentcore.AssistantToolCall("w1", "do_work", `{"step":1}`),
		agentcore.AssistantToolCall("p2", todo.ToolName, `{"items":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"}]}`),
		agentcore.AssistantToolCall("w2", "do_work", `{"step":2}`),
		agentcore.AssistantToolCall("p3", todo.ToolName, `{"items":[{"content":"b","status":"completed"}]}`),
		agentcore.AssistantText("all three steps complete"),
	)
	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 3
	agent, err := agentcore.New(agentcore.Config{
		Provider: faux,
		Model:    "test",
		Tools:    agentcore.NewToolSet(work, todo.NewTool(store)),
		Policy:   agentcore.NewAllowList("do_work", todo.ToolName),
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
	store := todo.NewStore()
	resp := make([]agentcore.ChatResponse, 0, 50)
	for i := 0; i < 50; i++ {
		resp = append(resp, agentcore.AssistantToolCall("p", todo.ToolName, `{"items":[{"content":"x","status":"in_progress"}]}`))
	}
	faux := agentcore.NewFauxProvider(resp...)
	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 5
	limits.MaxToolCalls = 6
	agent, err := agentcore.New(agentcore.Config{
		Provider: faux,
		Model:    "test",
		Tools:    agentcore.NewToolSet(todo.NewTool(store)),
		Policy:   agentcore.NewAllowList(todo.ToolName),
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

// echoTool is a minimal host tool standing in for real work, counting calls so
// a test can tell productive turns from bookkeeping ones.
type echoTool struct {
	name   string
	called int
}

func (t *echoTool) Name() string { return t.name }

func (t *echoTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: t.name, Description: "work", Parameters: map[string]any{"type": "object"}}
}

func (t *echoTool) Run(context.Context, string) (string, error) {
	t.called++
	return "done", nil
}
