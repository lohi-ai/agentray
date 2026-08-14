// Package todo contributes a live run plan: a checklist the model writes for
// itself and that the loop pins into every request.
//
// The plan is deliberately NOT part of the transcript. It lives in a Store the
// tool writes and a context hook reads, so compaction can never trim it — even
// after the original task and all early turns are summarized away, the freshly
// rendered checklist is right there in front of the model. That is the whole
// point of the capability: it is what keeps a long autonomous run on its
// original objective.
//
// It is a CONTRIBUTION plugin, the cheapest kind: a tool plus a hook, resolved
// once at compose time, with no per-run state of its own (the Store belongs to
// the caller, one per run). Nothing here needs BeginRun.
package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/lohi-ai/agentray/agentcore"
)

// ToolName is the stable name the model calls, and the name a policy must
// permit.
const ToolName = "update_plan"

// Todo status values. A well-formed plan has at most one in_progress item — the
// single thing the agent is doing right now — mirroring a focused worklist.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// Item is one step in the run's plan: a short imperative description and its
// current status.
type Item struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// Store holds a single run's live plan. It is the out-of-band state that
// makes a long run goal-stable: the plan is owned here, not in the transcript,
// so compaction can never trim it. The loop re-injects a rendering of it before
// every provider request (agentcore.ContextHook), so the model always sees its own
// up-to-date checklist regardless of how much history was summarized away.
//
// A Store is safe for concurrent use; the tool writes it while a context
// hook reads it.
type Store struct {
	mu    sync.RWMutex
	items []Item
}

// NewStore returns an empty plan store.
func NewStore() *Store { return &Store{} }

// Set replaces the whole plan. The model always sends the full list (not a
// delta), so a replace is the correct semantics and keeps the store trivially
// consistent. Items are copied so a later caller mutation cannot alias the store.
func (s *Store) Set(items []Item) {
	cp := make([]Item, len(items))
	copy(cp, items)
	s.mu.Lock()
	s.items = cp
	s.mu.Unlock()
}

// List returns a copy of the current plan.
func (s *Store) List() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, len(s.items))
	copy(out, s.items)
	return out
}

// Render formats the plan as a compact checklist for the model. It returns the
// empty string when there is no plan yet, so the context hook injects nothing
// until the agent has actually written one.
func (s *Store) Render() string {
	items := s.List()
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Current plan (your live todo list — keep it updated with update_plan):\n")
	for _, it := range items {
		b.WriteString(statusBox(it.Status))
		b.WriteByte(' ')
		b.WriteString(strings.TrimSpace(it.Content))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func statusBox(status string) string {
	switch status {
	case StatusCompleted:
		return "[x]"
	case StatusInProgress:
		return "[~]"
	default:
		return "[ ]"
	}
}

// ContextPrefix marks the injected plan reminder so it is recognizable in a
// transcript and never confused with model-authored content.
const ContextPrefix = "[run plan]"

// ContextHook returns a agentcore.ContextHook that appends the current plan as a
// trailing system reminder to every outgoing request. Because it is applied to
// the request view (not persisted history) on every turn, the plan survives
// compaction by construction: even after the original task and all early turns
// are summarized away, the freshly rendered checklist is right there in front of
// the model. An empty plan injects nothing.
func ContextHook(store *Store) agentcore.ContextHook {
	return func(_ context.Context, msgs []agentcore.Message) []agentcore.Message {
		rendered := store.Render()
		if rendered == "" {
			return msgs
		}
		out := make([]agentcore.Message, 0, len(msgs)+1)
		out = append(out, msgs...)
		out = append(out, agentcore.Message{Role: agentcore.RoleSystem, Content: ContextPrefix + "\n" + rendered})
		return out
	}
}

// planTool is the model-facing tool that writes the run plan.
type planTool struct {
	store *Store
}

// NewTool returns the built-in update_plan tool bound to a run's plan store.
// The model calls it to record and revise its checklist for a multi-step task;
// the stored plan is then pinned into every later turn by agentcore.ContextHook.
func NewTool(store *Store) agentcore.Tool { return &planTool{store: store} }

func (t *planTool) Name() string { return ToolName }

func (t *planTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolName,
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
								"enum":        []string{StatusPending, StatusInProgress, StatusCompleted},
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

func (t *planTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Items []Item `json:"items"`
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
		case StatusPending, StatusInProgress, StatusCompleted:
		case "":
			in.Items[i].Status = StatusPending
		default:
			return "", fmt.Errorf("update_plan: item %d has invalid status %q", i+1, it.Status)
		}
		if in.Items[i].Status == StatusInProgress {
			inProgress++
		}
	}
	if inProgress > 1 {
		return "", fmt.Errorf("update_plan: at most one item may be in_progress (got %d)", inProgress)
	}
	t.store.Set(in.Items)
	return "Plan updated.\n" + t.store.Render(), nil
}

// Bookkeeping marks update_plan calls as administrative rather than progress.
//
// A turn spent only on plan updates is refunded against MaxTurns, so an agent
// that keeps its checklist honest is not punished for it — without this, a
// careful planner finishes fewer steps than a careless one on the same budget.
// The MaxToolCalls budget still bounds a runaway planning loop.
func (*planTool) Bookkeeping() bool { return true }

// Plugin contributes the run plan: the update_plan tool and the context hook
// that pins the plan into every request.
type Plugin struct {
	// Store holds this run's plan. Required — a plan with nowhere to live is a
	// tool that silently forgets, which is worse for the model than no tool at
	// all. One Store per run; sharing one across concurrent runs would let two
	// agents overwrite each other's checklist.
	Store *Store
}

// With builds the plugin around a store.
func With(s *Store) Plugin { return Plugin{Store: s} }

// Name identifies the plugin.
func (Plugin) Name() string { return "todo" }

// Register contributes the tool and the hook together. They are one call
// because either alone is broken: the tool without the hook writes a plan the
// model never sees again, and the hook without the tool pins a plan nothing can
// write.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Store == nil {
		return errNoStore
	}
	r.AddTools(NewTool(p.Store))
	r.AddHooks(agentcore.PriorityDefault, agentcore.Hooks{
		Context: []agentcore.ContextHook{ContextHook(p.Store)},
	})
	return nil
}

// errNoStore refuses a composition that asks for a plan with nowhere to keep it.
var errNoStore = fmt.Errorf("todo: Plugin requires a Store")
