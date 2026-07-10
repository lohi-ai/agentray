package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ToolUpdatePlan is the stable name of the built-in run todo list tool.
const ToolUpdatePlan = "update_plan"

// Todo status values. A well-formed plan has at most one in_progress item — the
// single thing the agent is doing right now — mirroring a focused worklist.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// TodoItem is one step in the run's plan: a short imperative description and its
// current status.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// TodoStore holds a single run's live plan. It is the out-of-band state that
// makes a long run goal-stable: the plan is owned here, not in the transcript,
// so compaction can never trim it. The loop re-injects a rendering of it before
// every provider request (TodoContextHook), so the model always sees its own
// up-to-date checklist regardless of how much history was summarized away.
//
// A TodoStore is safe for concurrent use; the tool writes it while a context
// hook reads it.
type TodoStore struct {
	mu    sync.RWMutex
	items []TodoItem
}

// NewTodoStore returns an empty plan store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Set replaces the whole plan. The model always sends the full list (not a
// delta), so a replace is the correct semantics and keeps the store trivially
// consistent. Items are copied so a later caller mutation cannot alias the store.
func (s *TodoStore) Set(items []TodoItem) {
	cp := make([]TodoItem, len(items))
	copy(cp, items)
	s.mu.Lock()
	s.items = cp
	s.mu.Unlock()
}

// List returns a copy of the current plan.
func (s *TodoStore) List() []TodoItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TodoItem, len(s.items))
	copy(out, s.items)
	return out
}

// Render formats the plan as a compact checklist for the model. It returns the
// empty string when there is no plan yet, so the context hook injects nothing
// until the agent has actually written one.
func (s *TodoStore) Render() string {
	items := s.List()
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Current plan (your live todo list — keep it updated with update_plan):\n")
	for _, it := range items {
		b.WriteString(todoBox(it.Status))
		b.WriteByte(' ')
		b.WriteString(strings.TrimSpace(it.Content))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func todoBox(status string) string {
	switch status {
	case TodoCompleted:
		return "[x]"
	case TodoInProgress:
		return "[~]"
	default:
		return "[ ]"
	}
}

// todoContextPrefix marks the injected plan reminder so it is recognizable in a
// transcript and never confused with model-authored content.
const todoContextPrefix = "[run plan]"

// TodoContextHook returns a ContextHook that appends the current plan as a
// trailing system reminder to every outgoing request. Because it is applied to
// the request view (not persisted history) on every turn, the plan survives
// compaction by construction: even after the original task and all early turns
// are summarized away, the freshly rendered checklist is right there in front of
// the model. An empty plan injects nothing.
func TodoContextHook(store *TodoStore) ContextHook {
	return func(_ context.Context, msgs []Message) []Message {
		rendered := store.Render()
		if rendered == "" {
			return msgs
		}
		out := make([]Message, 0, len(msgs)+1)
		out = append(out, msgs...)
		out = append(out, Message{Role: RoleSystem, Content: todoContextPrefix + "\n" + rendered})
		return out
	}
}

// todoTool is the model-facing tool that writes the run plan.
type todoTool struct {
	store *TodoStore
}

// NewTodoTool returns the built-in update_plan tool bound to a run's plan store.
// The model calls it to record and revise its checklist for a multi-step task;
// the stored plan is then pinned into every later turn by TodoContextHook.
func NewTodoTool(store *TodoStore) Tool { return &todoTool{store: store} }

func (t *todoTool) Name() string { return ToolUpdatePlan }

func (t *todoTool) Schema() ToolSchema {
	return ToolSchema{
		Name: ToolUpdatePlan,
		Description: "Record or update your plan as a todo list for a multi-step task. " +
			"Send the FULL list every time (it replaces the previous plan). Mark exactly one " +
			"item in_progress (the step you are doing now), completed for finished steps, and " +
			"pending for the rest. The plan is pinned into your context and survives summarization, " +
			"so use it to stay on the original goal across a long run.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"items": map[string]any{
					"type":        "array",
					"description": "The full ordered todo list.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string", "description": "Short imperative description of the step."},
							"status": map[string]any{
								"type":        "string",
								"enum":        []string{TodoPending, TodoInProgress, TodoCompleted},
								"description": "Step status.",
							},
						},
						"required": []string{"content", "status"},
					},
				},
			},
			"required": []string{"items"},
		},
	}
}

func (t *todoTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Items []TodoItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("update_plan: invalid arguments: %w", err)
	}
	inProgress := 0
	for i, it := range in.Items {
		if strings.TrimSpace(it.Content) == "" {
			return "", fmt.Errorf("update_plan: item %d has empty content", i+1)
		}
		switch it.Status {
		case TodoPending, TodoInProgress, TodoCompleted:
		case "":
			in.Items[i].Status = TodoPending
		default:
			return "", fmt.Errorf("update_plan: item %d has invalid status %q", i+1, it.Status)
		}
		if in.Items[i].Status == TodoInProgress {
			inProgress++
		}
	}
	if inProgress > 1 {
		return "", fmt.Errorf("update_plan: at most one item may be in_progress (got %d)", inProgress)
	}
	t.store.Set(in.Items)
	return "Plan updated.\n" + t.store.Render(), nil
}
