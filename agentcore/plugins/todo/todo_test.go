package todo_test

import (
	"context"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"strings"
	"testing"
)

func TestTodoToolSetsAndRenders(t *testing.T) {
	store := todo.NewStore()
	tool := todo.NewTool(store)

	out, err := tool.Run(context.Background(), `{"items":[
		{"content":"Read the schema","status":"completed"},
		{"content":"Write the migration","status":"in_progress"},
		{"content":"Run the tests","status":"pending"}]}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"[x] Read the schema", "[~] Write the migration", "[ ] Run the tests"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered plan missing %q in:\n%s", want, out)
		}
	}
	if got := store.List(); len(got) != 3 {
		t.Fatalf("store should hold 3 items, got %d", len(got))
	}
}

func TestTodoToolRejectsMultipleInProgress(t *testing.T) {
	tool := todo.NewTool(todo.NewStore())
	_, err := tool.Run(context.Background(), `{"items":[
		{"content":"a","status":"in_progress"},
		{"content":"b","status":"in_progress"}]}`)
	if err == nil || !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("expected rejection of two in_progress items, got %v", err)
	}
}

func TestTodoToolRejectsBadStatusAndEmpty(t *testing.T) {
	tool := todo.NewTool(todo.NewStore())
	if _, err := tool.Run(context.Background(), `{"items":[{"content":"a","status":"doing"}]}`); err == nil {
		t.Fatal("expected rejection of invalid status")
	}
	if _, err := tool.Run(context.Background(), `{"items":[{"content":"   ","status":"pending"}]}`); err == nil {
		t.Fatal("expected rejection of empty content")
	}
}

// TestContextHookInjectsLivePlan is the goal-stability property for the todo
// list: the hook appends the CURRENT plan to the outgoing request as a trailing
// system reminder, so even a compacted transcript (which no longer holds the
// plan) still shows the model its checklist.
func TestContextHookInjectsLivePlan(t *testing.T) {
	store := todo.NewStore()
	hook := todo.ContextHook(store)

	// No plan yet -> hook injects nothing.
	base := []agentcore.Message{{Role: agentcore.RoleSystem, Content: "persona"}, {Role: agentcore.RoleUser, Content: "go"}}
	if got := hook(context.Background(), base); len(got) != len(base) {
		t.Fatalf("empty plan must not inject; len=%d", len(got))
	}

	store.Set([]todo.Item{{Content: "ship it", Status: todo.StatusInProgress}})
	out := hook(context.Background(), base)
	if len(out) != len(base)+1 {
		t.Fatalf("expected one injected reminder, got %d extra", len(out)-len(base))
	}
	last := out[len(out)-1]
	if last.Role != agentcore.RoleSystem || !strings.HasPrefix(last.Content, todo.ContextPrefix) || !strings.Contains(last.Content, "ship it") {
		t.Fatalf("injected reminder wrong: %+v", last)
	}
	// The hook must not mutate the caller's history.
	if len(base) != 2 {
		t.Fatalf("hook mutated input history (len=%d)", len(base))
	}
}

// TestTodoSurvivesCompaction proves the end-to-end goal-stability claim: after a
// transcript is compacted (plan not in history), the context hook still presents
// the live plan to the model.
func TestTodoSurvivesCompaction(t *testing.T) {
	store := todo.NewStore()
	store.Set([]todo.Item{
		{Content: "phase 1", Status: todo.StatusCompleted},
		{Content: "phase 2", Status: todo.StatusInProgress},
	})
	hook := todo.ContextHook(store)

	// Compact for real, through the same seam the loop uses, so this proves the
	// property against the shipping strategy rather than a stand-in.
	prov := agentcore.NewFauxProvider(agentcore.AssistantText("## Goal\nx\n## Next Steps\n1. y"))
	res, err := agentcore.DefaultCompactor().Compact(context.Background(), agentcore.CompactionRequest{
		Messages: longTranscript(),
		Budget:   agentcore.DefaultLimits().MaxContextTokens,
		Settings: agentcore.CompactionSettings{KeepRecentTokens: 3000},
		Provider: prov,
		Model:    "m",
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	compacted := res.Messages

	out := hook(context.Background(), compacted)
	last := out[len(out)-1]
	if !strings.Contains(last.Content, "phase 2") || !strings.Contains(last.Content, "[~]") {
		t.Fatalf("plan not pinned post-compaction: %q", last.Content)
	}
}

// longTranscript builds a transcript big enough that the compactor finds a span
// worth summarizing.
func longTranscript() []agentcore.Message {
	out := []agentcore.Message{{Role: agentcore.RoleSystem, Content: "persona"}}
	for i := 0; i < 60; i++ {
		out = append(out,
			agentcore.Message{Role: agentcore.RoleUser, Content: strings.Repeat("question ", 64)},
			agentcore.Message{Role: agentcore.RoleAssistant, Content: strings.Repeat("answer ", 64)},
		)
	}
	return out
}
