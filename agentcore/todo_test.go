package agentcore

import (
	"context"
	"strings"
	"testing"
)

func TestTodoToolSetsAndRenders(t *testing.T) {
	store := NewTodoStore()
	tool := NewTodoTool(store)

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
	tool := NewTodoTool(NewTodoStore())
	_, err := tool.Run(context.Background(), `{"items":[
		{"content":"a","status":"in_progress"},
		{"content":"b","status":"in_progress"}]}`)
	if err == nil || !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("expected rejection of two in_progress items, got %v", err)
	}
}

func TestTodoToolRejectsBadStatusAndEmpty(t *testing.T) {
	tool := NewTodoTool(NewTodoStore())
	if _, err := tool.Run(context.Background(), `{"items":[{"content":"a","status":"doing"}]}`); err == nil {
		t.Fatal("expected rejection of invalid status")
	}
	if _, err := tool.Run(context.Background(), `{"items":[{"content":"   ","status":"pending"}]}`); err == nil {
		t.Fatal("expected rejection of empty content")
	}
}

// TestTodoContextHookInjectsLivePlan is the goal-stability property for the todo
// list: the hook appends the CURRENT plan to the outgoing request as a trailing
// system reminder, so even a compacted transcript (which no longer holds the
// plan) still shows the model its checklist.
func TestTodoContextHookInjectsLivePlan(t *testing.T) {
	store := NewTodoStore()
	hook := TodoContextHook(store)

	// No plan yet -> hook injects nothing.
	base := []Message{{Role: RoleSystem, Content: "persona"}, {Role: RoleUser, Content: "go"}}
	if got := hook(context.Background(), base); len(got) != len(base) {
		t.Fatalf("empty plan must not inject; len=%d", len(got))
	}

	store.Set([]TodoItem{{Content: "ship it", Status: TodoInProgress}})
	out := hook(context.Background(), base)
	if len(out) != len(base)+1 {
		t.Fatalf("expected one injected reminder, got %d extra", len(out)-len(base))
	}
	last := out[len(out)-1]
	if last.Role != RoleSystem || !strings.HasPrefix(last.Content, todoContextPrefix) || !strings.Contains(last.Content, "ship it") {
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
	store := NewTodoStore()
	store.Set([]TodoItem{
		{Content: "phase 1", Status: TodoCompleted},
		{Content: "phase 2", Status: TodoInProgress},
	})
	hook := TodoContextHook(store)

	prov := &scriptedSummaryProvider{summary: "## Goal\nx\n## Next Steps\n1. y"}
	compacted := compactWithSummary(context.Background(), prov, "m", longTranscript(), CompactionSettings{KeepRecentTokens: 3000})

	out := hook(context.Background(), compacted)
	last := out[len(out)-1]
	if !strings.Contains(last.Content, "phase 2") || !strings.Contains(last.Content, "[~]") {
		t.Fatalf("plan not pinned post-compaction: %q", last.Content)
	}
}
