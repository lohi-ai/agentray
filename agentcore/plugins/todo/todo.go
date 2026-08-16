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

// The plan's ceilings.
//
// Everything pinned into every request needs one, and the plan needs it more
// than most: it is immune to compaction BY CONSTRUCTION (that is the feature),
// so nothing else in the system will ever bring it back down. An agent that
// decomposes as it discovers — which is what a long run does — appends steps and
// leaves the finished ones behind as the record that the work happened. Three
// hundred turns of that is 25 KB in the prefix of every call for the rest of the
// run, crowding out the window the plan exists to keep the model oriented in.
//
// The numbers are chosen from what each part is FOR, not from a uniform budget:
//
//   - keepCompleted — finished steps are a progress signal, not a working set.
//     The most recent few say "here is where you are"; the two hundredth-most
//     recent says nothing the running count does not.
//   - maxItemBytes — a plan item is a short imperative. A model that writes a
//     paragraph into one is the other way this block bloats, and it is not
//     bounded by item count at all.
//   - maxRenderBytes — the last-resort ceiling, so the invariant "the pinned
//     plan is bounded" holds no matter what shape the plan takes. At the loop's
//     ~4-bytes-per-token estimate this is ~500 tokens.
const (
	keepCompleted  = 5
	maxItemBytes   = 160
	maxRenderBytes = 2000
)

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
	// retired counts completed steps folded away to keep the pinned plan bounded.
	// It is a floor, not an exact tally: the model sends the full list each time,
	// so a step it drops from its own list on its own initiative is never counted
	// here. That is the right direction to be wrong in — the number never claims
	// progress that did not happen.
	retired int
}

// NewStore returns an empty plan store.
func NewStore() *Store { return &Store{} }

// Set replaces the whole plan. The model always sends the full list (not a
// delta), so a replace is the correct semantics and keeps the store trivially
// consistent. Items are copied so a later caller mutation cannot alias the store.
//
// Set also folds: completed steps beyond the most recent few are dropped from
// the list and added to a running count. This is what keeps a long run's plan
// from becoming a transcript. It is done HERE rather than only in Render so that
// what the model reads and what the store holds are the same thing — a model
// shown a folded list sends the folded list back, and a store that quietly held
// more would re-expand the render on the next write.
func (s *Store) Set(items []Item) {
	cp := make([]Item, 0, len(items))
	completed := 0
	for _, it := range items {
		it.Content = clampItem(it.Content)
		if it.Status == StatusCompleted {
			completed++
		}
		cp = append(cp, it)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if drop := completed - keepCompleted; drop > 0 {
		kept := make([]Item, 0, len(cp)-drop)
		for _, it := range cp {
			// Oldest first: the list is ordered, so the ones at the front are the
			// ones furthest from what the agent is doing now.
			if it.Status == StatusCompleted && drop > 0 {
				drop--
				s.retired++
				continue
			}
			kept = append(kept, it)
		}
		cp = kept
	}
	s.items = cp
}

// Retired reports how many completed steps have been folded into the count.
func (s *Store) Retired() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retired
}

// clampItem bounds one step's text. A plan item is a short imperative; anything
// longer is prose that belongs in the answer, not in a block reprinted on every
// turn for the rest of the run.
func clampItem(content string) string {
	content = strings.TrimSpace(content)
	if len(content) <= maxItemBytes {
		return content
	}
	return strings.TrimSpace(content[:maxItemBytes]) + "…"
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
	s.mu.RLock()
	items := make([]Item, len(s.items))
	copy(items, s.items)
	retired := s.retired
	s.mu.RUnlock()

	if len(items) == 0 && retired == 0 {
		return ""
	}

	head := "Current plan (your live todo list — keep it updated with update_plan):\n"
	if retired > 0 {
		head += fmt.Sprintf("[x] (%d earlier steps completed)\n", retired)
	}

	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = statusBox(it.Status) + " " + it.Content
	}
	return strings.TrimRight(head+fitLines(lines, items, maxRenderBytes-len(head)), "\n")
}

// fitLines is the last-resort ceiling on the rendered plan, and it drops with a
// priority rather than a rule of thumb: whatever else goes, the step the agent
// is ON stays. That is the one line the model cannot choose its next action
// without, and it is not necessarily near the front of the list.
//
// Everything else is kept in the list's own order from the front, which favors
// the steps nearest the current work over a long pending tail, and the remainder
// is accounted for rather than silently vanished.
func fitLines(lines []string, items []Item, budget int) string {
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	if total <= budget {
		return strings.Join(lines, "\n") + "\n"
	}

	keep := make([]bool, len(lines))
	used := 0
	// Reserve the active step first, before anything can crowd it out.
	for i, it := range items {
		if it.Status == StatusInProgress {
			keep[i] = true
			used += len(lines[i]) + 1
		}
	}
	for i := range lines {
		if keep[i] {
			continue
		}
		if used+len(lines[i])+1 > budget {
			continue
		}
		keep[i] = true
		used += len(lines[i]) + 1
	}

	var b strings.Builder
	dropped := 0
	for i, l := range lines {
		if !keep[i] {
			dropped++
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "… (%d further steps in the plan, not shown here)\n", dropped)
	}
	return b.String()
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
			"so use it to stay on the original goal across a long run. Because it is pinned, keep it " +
			"a plan and not a log: one short line per step. Older completed steps are folded into a " +
			"running count automatically — that count is not an item, and you do not need to restate " +
			"the steps behind it.",
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
	items, err := parseItems(args)
	if err != nil {
		return "", err
	}
	t.store.Set(items)
	return "Plan updated.\n" + t.store.Render(), nil
}

// parseItems decodes and validates one update_plan payload. It is separate from
// Run because resume recovery replays the same arguments out of the log and must
// reach the same verdict — a call the original run rejected must not become a
// plan the recovered run holds.
func parseItems(args string) ([]Item, error) {
	var in struct {
		Items []Item `json:"items"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return nil, fmt.Errorf("update_plan: invalid arguments: %w", err)
	}
	inProgress := 0
	for i, it := range in.Items {
		if strings.TrimSpace(it.Content) == "" {
			return nil, fmt.Errorf("update_plan: item %d has empty content", i+1)
		}
		switch it.Status {
		case StatusPending, StatusInProgress, StatusCompleted:
		case "":
			in.Items[i].Status = StatusPending
		default:
			return nil, fmt.Errorf("update_plan: item %d has invalid status %q", i+1, it.Status)
		}
		if in.Items[i].Status == StatusInProgress {
			inProgress++
		}
	}
	if inProgress > 1 {
		return nil, fmt.Errorf("update_plan: at most one item may be in_progress (got %d)", inProgress)
	}
	return in.Items, nil
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
//
// The extension is the third piece, and it only does anything on a resume: it
// puts the plan back.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Store == nil {
		return errNoStore
	}
	r.AddTools(NewTool(p.Store))
	r.AddHooks(agentcore.PriorityDefault, agentcore.Hooks{
		Context: []agentcore.ContextHook{ContextHook(p.Store)},
	})
	r.AddExtension(p)
	return nil
}

// BeginRun restores the plan a crashed run had written.
//
// A long run's durable intent is three things: the user's requirement (pinned
// through compaction), the goal (EntryGoal, recovered by the loop), and this
// checklist. The first two came back after a crash and the third did not, which
// is worst in exactly the case it matters most — a run long enough to have been
// compacted has had its original task summarized away, so the plan is the
// agent's remaining record of what it decided to do. Resuming with an empty one
// means re-planning from a summary, which is how a recovered run quietly does
// different work than the run it is recovering.
//
// The recovery reads the run's OWN log through RunInfo.Session, which is handed
// over for precisely this. It writes nothing: the plan is reconstructed from the
// update_plan calls already in the record, so the loop stays the only writer and
// "model-visible means logged" still holds.
//
// It returns a nil Extension in every case — there is nothing to do for the rest
// of the run, and the plugin has no per-turn behavior. A fresh run, a run with a
// plan already in hand, and a log with no plan in it all take the same path.
func (p Plugin) BeginRun(ctx context.Context, info agentcore.RunInfo) (agentcore.Extension, error) {
	if p.Store == nil || info.Session == nil || len(p.Store.List()) > 0 {
		return nil, nil
	}
	entries, err := info.Session.Log(ctx, info.SessionID)
	if err != nil {
		// A resume whose log cannot be read is already failing louder elsewhere;
		// losing the plan is not the error worth aborting a run over.
		return nil, nil
	}
	if items, ok := planFromLog(entries); ok {
		p.Store.Set(items)
	}
	return nil, nil
}

// planFromLog folds the log down to the plan in force: the last update_plan call
// wins, and a leaf clears it, mirroring how the loop recovers the goal. A run
// chained onto a finished session starts with a clean checklist rather than
// inheriting the previous task's.
//
// Only a call that would be ACCEPTED counts. A rejected one (two steps in
// progress, empty content) left the original run's plan unchanged, so replaying
// it on resume would install a plan the crashed run never had. Reusing the
// tool's own validator is what keeps the two answers the same.
func planFromLog(entries []agentcore.SessionEntry) ([]Item, bool) {
	var found []Item
	ok := false
	for _, e := range entries {
		switch e.Kind {
		case agentcore.EntryLeaf:
			found, ok = nil, false
		case agentcore.EntryMessage:
			if e.Message == nil {
				continue
			}
			for _, tc := range e.Message.ToolCalls {
				if tc.Name != ToolName {
					continue
				}
				items, err := parseItems(tc.Arguments)
				if err != nil {
					continue
				}
				found, ok = items, true
			}
		}
	}
	return found, ok
}

// errNoStore refuses a composition that asks for a plan with nowhere to keep it.
var errNoStore = fmt.Errorf("todo: Plugin requires a Store")
