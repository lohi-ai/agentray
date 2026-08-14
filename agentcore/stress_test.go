package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// blobTool returns a payload of size bytes so each work turn grows the
// context. Sized large it drives the transcript past the compaction budget;
// it, the payloads dodge the editor and force compaction instead.
type blobTool struct {
	calls int
	size  int
}

func (b *blobTool) Name() string { return "blob" }
func (b *blobTool) Schema() ToolSchema {
	return ToolSchema{Name: "blob", Description: "returns a large blob", Parameters: map[string]any{"type": "object"}}
}
func (b *blobTool) Run(_ context.Context, _ string) (string, error) {
	b.calls++
	return "blob#" + fmt.Sprint(b.calls) + " " + bigText(b.size), nil
}

// stressProvider is a content-aware scripted seam: it drives a long run (work +
// periodic plan updates, then a final answer) AND doubles as the compaction
// summarizer — when handed the summarization system prompt it returns a valid
// structured checkpoint, so the LLM-summary path (and thus the goal pin) engages
// live across many compactions.
type stressProvider struct {
	target    int
	calls     int
	Summaries int
	// uniqueArgs varies each blob call's arguments so a redundancy-aware
	// redundancy-aware rule could not treat them as duplicates.
	uniqueArgs bool
}

func (p *stressProvider) Name() string        { return "stress" }
func (p *stressProvider) SupportsTools() bool { return true }

func (p *stressProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.Summaries++
		return AssistantText("## Goal\nMaintain the long-running invariant\n## Next Steps\n1. keep working"), nil
	}
	p.calls++
	switch {
	case p.calls >= p.target:
		return AssistantText("DONE long-run complete"), nil
	case p.calls%5 == 0:
		// Bookkeeping turn: update the plan (refunded against MaxTurns).
		items := `{"items":[{"content":"phase A","status":"completed"},{"content":"phase B","status":"in_progress"}]}`
		return AssistantToolCall(fmt.Sprintf("p%d", p.calls), planToolName, items), nil
	default:
		args := `{}`
		if p.uniqueArgs {
			args = fmt.Sprintf(`{"n":%d}`, p.calls)
		}
		return AssistantToolCall(fmt.Sprintf("w%d", p.calls), "blob", args), nil
	}
}

func (p *stressProvider) Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error) {
	ch := make(chan ChatDelta, 8)
	go func() {
		defer close(ch)
		resp, _ := p.Chat(ctx, req)
		if resp.Message.Content != "" {
			ch <- ChatDelta{ContentDelta: resp.Message.Content}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- ChatDelta{ToolCall: &tc}
		}
		ch <- ChatDelta{Done: true, StopReason: resp.StopReason}
	}()
	return ch, nil
}

// TestLongRunStaysStableAcrossManyCompactions is the headline long-running
// guarantee: a 120-turn run under a tight context budget compacts dozens of
// times, yet (a) reaches its final answer without dying, (b) keeps the original
// goal pinned exactly once and verbatim the whole way, (c) keeps the live plan,
// and (d) stays bounded — compaction prevents the message list from growing with
// the turn count.
func TestLongRunStaysStableAcrossManyCompactions(t *testing.T) {
	const goal = "Hold the long-running invariant: stay on task for the entire run"
	store := newPlanStore()
	// Unique args per call mean no redundancy pass could ever collapse these
	// results, so the run stresses the compaction path itself.
	prov := &stressProvider{target: 120, uniqueArgs: true}

	limits := DefaultLimits()
	limits.MaxTurns = 400
	limits.MaxToolCalls = 500
	limits.MaxContextTokens = 4000 // tight budget -> frequent compaction
	cs := DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := New(Config{
		Provider:   prov,
		Model:      "stress",
		Tools:      NewToolSet(&blobTool{size: 900}, newPlanTool(store)),
		Policy:     NewAllowList("blob", planToolName),
		Limits:     &limits,
		Compaction: &cs,
		Hooks:      Hooks{Context: []ContextHook{planContextHook(store)}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), goal)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if res.Final != "DONE long-run complete" {
		t.Fatalf("run did not reach its end (stop=%q final=%q turns=%d)", res.StopReason, res.Final, res.Turns)
	}
	if res.Turns < 100 {
		t.Fatalf("expected a long run (>=100 turns), got %d", res.Turns)
	}
	if prov.Summaries < 3 {
		t.Fatalf("expected many compactions over a long run, got %d summary calls", prov.Summaries)
	}

	pins := 0
	for _, m := range res.Messages {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, goalMarker) {
			pins++
			if got := strings.TrimSpace(strings.TrimPrefix(m.Content, goalMarker)); got != goal {
				t.Fatalf("goal drifted across %d compactions: %q", prov.Summaries, got)
			}
		}
	}
	if pins != 1 {
		t.Fatalf("expected exactly one goal pin after %d compactions, got %d", prov.Summaries, pins)
	}

	// Compaction must keep the transcript bounded: 120 turns would be ~240+
	// messages uncompacted; a healthy run stays far smaller.
	if len(res.Messages) > 60 {
		t.Fatalf("compaction did not bound the context: %d messages after %d turns", len(res.Messages), res.Turns)
	}

	// The live plan is still queryable at run end (it backs the context-hook
	// reminder that keeps the model on its checklist across compaction).
	if len(store.items) == 0 {
		t.Fatal("run plan was lost over the long run")
	}
}

// TestLongRunWithBulkyResultsStaysBounded is the token-pressure counterpart to
// the compaction stress above: with bulky results from identical calls, a long
// run must still terminate with its context bounded. Compaction is the only
// mechanism doing that bounding — the deterministic in-place context editor was
// removed, so this run pays for the bounding in summarization calls.
func TestLongRunWithBulkyResultsStaysBounded(t *testing.T) {
	prov := &stressProvider{target: 120}

	limits := DefaultLimits()
	limits.MaxTurns = 400
	limits.MaxToolCalls = 500
	limits.MaxContextTokens = 4000
	cs := DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := New(Config{
		Provider:   prov,
		Model:      "stress",
		Tools:      NewToolSet(&blobTool{size: 3000}, newPlanTool(newPlanStore())),
		Policy:     NewAllowList("blob", planToolName),
		Limits:     &limits,
		Compaction: &cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "long run with redundant bulky results")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "DONE long-run complete" {
		t.Fatalf("run did not reach its end (stop=%q final=%q turns=%d)", res.StopReason, res.Final, res.Turns)
	}
	// Compaction has to engage — a run this bulky cannot fit its own budget.
	if prov.Summaries == 0 {
		t.Fatal("compaction never engaged on a bulky long run")
	}
	// And the estimated context must stay bounded, not grow with turn count.
	if est := estimateContextTokens(res.Messages); est > 2*limits.MaxContextTokens {
		t.Fatalf("context not bounded: estimated %d tokens after %d turns", est, res.Turns)
	}
}

// --- local plan stand-in --------------------------------------------------
//
// These stress tests are about COMPACTION under load: that a long run stays
// stable, that the goal pin survives, that growth stays bounded. A
// pinned-context capability is one of the participants, so the run needs one —
// but core's tests must not import a plugin, or "the loop names no plugin"
// would be false in the test build even while true in the shipped one.
//
// So this is a minimal local stand-in with the same shape as the todo plugin: a
// store, a bookkeeping tool that writes it, and a context hook that pins it.

const planToolName = "update_plan"

type planItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

type planStore struct{ items []planItem }

func newPlanStore() *planStore { return &planStore{} }

func (s *planStore) render() string {
	if len(s.items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Current plan:\n")
	for _, it := range s.items {
		fmt.Fprintf(&b, "- [%s] %s\n", it.Status, it.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// planContextHook pins the live plan into every request, which is what makes it
// survive compaction: it is applied to the request view, not to history.
func planContextHook(s *planStore) ContextHook {
	return func(_ context.Context, msgs []Message) []Message {
		rendered := s.render()
		if rendered == "" {
			return msgs
		}
		return append(append([]Message{}, msgs...), Message{Role: RoleSystem, Content: "[run plan]\n" + rendered})
	}
}

type planTool struct{ store *planStore }

func newPlanTool(s *planStore) Tool { return &planTool{store: s} }

func (*planTool) Name() string { return planToolName }

// Bookkeeping earns the turn refund, which these tests depend on: without it a
// run that interleaves plan updates would hit MaxTurns before compacting enough
// times to prove stability.
func (*planTool) Bookkeeping() bool { return true }

func (*planTool) Schema() ToolSchema {
	return ToolSchema{Name: planToolName, Description: "record the plan", Parameters: map[string]any{"type": "object"}}
}

func (t *planTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Items []planItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	t.store.items = in.Items
	return "Plan updated.", nil
}
