package agentcore_test

// The run plan at scale.
//
// The plan is the one piece of run state that is immune to compaction *by
// construction*: it lives in a Store, not the transcript, and a context hook
// re-injects a fresh rendering into every request. That is the whole reason the
// capability exists — after a thousand turns the original task has been
// summarized away, but the checklist is still right there.
//
// It is also why the plan is dangerous. Anything that survives compaction and is
// pinned into every request subtracts from the same window compaction is fighting
// to protect, and it does so on every single turn for the rest of the run. So it
// needs a ceiling for exactly the reason the compaction checkpoint needed one.
//
// The existing scale test writes a fixed five-item plan and only re-statuses it.
// That is the friendly case. A real long run does not work that way: an agent
// decomposes as it discovers, so the checklist GROWS — new subtasks appended,
// finished ones left behind as a record of progress. These tests drive that
// shape.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

// planScaleTask is a task whose shape invites decomposition, because that is the
// shape that grows a checklist.
const planScaleTask = "Audit every shard in the ledger corpus and file one report per region."

const planScaleGoal = "Every shard audited and one report filed per region"

// --- a model that discovers work as it goes -----------------------------------

// growingPlanProvider plays an agent that decomposes. Every planEvery turns it
// closes out the step it was on and appends the subtask it just discovered, then
// starts on that. Nothing is ever deleted — a finished item is the record that
// the work happened, which is exactly why a model keeps it.
type growingPlanProvider struct {
	mu sync.Mutex

	workTurns int
	planEvery int

	parentTurns int
	summaries   int
	planUpdates int
	finishes    int

	// discovered is the checklist as the model has built it so far.
	discovered []todo.Item

	// lastRequest is what the model was shown on its most recent turn. Every
	// assertion here is made against this, because the question is what the run
	// COSTS to keep the plan in front of the model, not what the store holds.
	lastRequest []agentcore.Message
}

func (*growingPlanProvider) Name() string        { return "growing-plan" }
func (*growingPlanProvider) SupportsTools() bool { return true }

func (p *growingPlanProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		return usageFor(req, agentcore.AssistantText(fmt.Sprintf(
			"## Goal\nContinue the audit\n## Progress\n### Done\n- [x] batch %d processed\n## Next Steps\n1. keep auditing",
			p.summaries))), nil
	}

	p.parentTurns++
	n := p.parentTurns
	p.lastRequest = req.Messages

	switch {
	case n >= p.workTurns:
		p.finishes++
		if p.finishes == 1 {
			return usageFor(req, agentcore.AssistantText("Audit looks complete.")), nil
		}
		return usageFor(req, agentcore.AssistantText("All shards audited, reports filed.\n"+goal.Done)), nil

	case n == 1 || n%p.planEvery == 0:
		p.planUpdates++
		p.discover()
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("plan%d", p.planUpdates), todo.ToolName, planItemArgs(p.discovered))), nil

	default:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n))), nil
	}
}

// discover closes the current step and appends the one it uncovered. Item text
// is the length a real agent writes — a short imperative sentence, not a word.
func (p *growingPlanProvider) discover() {
	for i := range p.discovered {
		if p.discovered[i].Status == todo.StatusInProgress {
			p.discovered[i].Status = todo.StatusCompleted
		}
	}
	p.discovered = append(p.discovered, todo.Item{
		Content: fmt.Sprintf("Reconcile shard %d against the regional clearing file and note the discrepancy",
			len(p.discovered)+1),
		Status: todo.StatusInProgress,
	})
}

func (p *growingPlanProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.ChatDelta, 4)
	go func() {
		defer close(ch)
		if resp.Message.Content != "" {
			ch <- agentcore.ChatDelta{ContentDelta: resp.Message.Content}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- agentcore.ChatDelta{ToolCall: &tc}
		}
		ch <- agentcore.ChatDelta{Done: true}
	}()
	return ch, nil
}

func planItemArgs(items []todo.Item) string {
	b, err := json.Marshal(struct {
		Items []todo.Item `json:"items"`
	}{items})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// injectedPlanBytes sizes the pinned checklist as the provider bills it: the
// trailing system reminder the context hook adds to the request.
func injectedPlanBytes(msgs []agentcore.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agentcore.RoleSystem && strings.HasPrefix(msgs[i].Content, todo.ContextPrefix) {
			return len(msgs[i].Content)
		}
	}
	return 0
}

// --- the test -----------------------------------------------------------------

// planCeilingBytes is the share of the window the pinned plan may hold.
//
// The run below is capped at 4000 context tokens, and the plan is charged
// against that on every turn forever — it is not a cost the run pays once. A
// checklist is a navigation aid, not a record, so it gets a smaller share than
// the compaction checkpoint's budget/4: one eighth of the window, ~500 tokens at
// the ~4-bytes-per-token the loop estimates with.
const planCeilingBytes = (4000 / 8) * 4

// TestVeryLongRunKeepsItsPlanInsideTheBudget is the round's headline. An agent
// that decomposes for 900 turns builds a long checklist, and every item of it is
// pinned into every request from then on. Unbounded, that is a slow-motion
// context leak that compaction cannot touch and that gets worse the longer the
// run goes — the exact opposite of what the plan is for.
func TestVeryLongRunKeepsItsPlanInsideTheBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("long run")
	}

	const workTurns = 900

	prov := &growingPlanProvider{workTurns: workTurns, planEvery: 3}
	store := newE2EStore()
	plan := todo.NewStore()
	work := &e2eWorkTool{size: 600}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * workTurns
	limits.MaxToolCalls = 4 * workTurns
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "plan-scale",
			Tools:      agentcore.NewToolSet(work),
			Policy:     agentcore.NewAllowList("work", todo.ToolName),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  "plan-scale",
		}},
		goal.Until(planScaleGoal),
		todo.With(plan),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), planScaleTask); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	last := prov.lastRequest
	updates := prov.planUpdates
	items := len(prov.discovered)
	prov.mu.Unlock()

	pinned := injectedPlanBytes(last)
	window := transcriptBytes(last)

	t.Logf("plan updates=%d items=%d pinned=%dB window=%dB (plan is %d%% of the window)",
		updates, items, pinned, window, pinned*100/max(window, 1))

	if pinned == 0 {
		t.Fatal("no plan was pinned into the final request — the checklist the whole capability exists to keep is not there")
	}
	if pinned > planCeilingBytes {
		t.Fatalf("the pinned plan is %d bytes against a %d-byte share of the window: it grows with the run, "+
			"is immune to compaction by construction, and is charged on every turn — so a long run pays it "+
			"forever and it crowds out the work it was meant to keep on track", pinned, planCeilingBytes)
	}

	// Bounding the plan must not mean losing the model's place in it. What the
	// agent is doing RIGHT NOW is the one line it cannot navigate without.
	current := fmt.Sprintf("Reconcile shard %d against the regional clearing file", items)
	if !strings.Contains(planText(last), current) {
		t.Fatalf("the plan was bounded by dropping the in_progress step, which is the one item that "+
			"decides the next action:\n%s", planText(last))
	}

	// And progress must still be legible: an agent that cannot tell how far it
	// has come will redo work, which costs far more than the bytes saved.
	if !strings.Contains(planText(last), "completed") {
		t.Fatalf("the bounded plan does not account for the finished steps at all, so the run cannot "+
			"tell what it has already done:\n%s", planText(last))
	}
}

func planText(msgs []agentcore.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agentcore.RoleSystem && strings.HasPrefix(msgs[i].Content, todo.ContextPrefix) {
			return msgs[i].Content
		}
	}
	return ""
}
