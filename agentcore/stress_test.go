package agentcore

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// blobTool returns a bulky payload so each work turn grows the context, forcing
// the loop to compact repeatedly over a long run.
type blobTool struct{ calls int }

func (b *blobTool) Name() string { return "blob" }
func (b *blobTool) Schema() ToolSchema {
	return ToolSchema{Name: "blob", Description: "returns a large blob", Parameters: map[string]any{"type": "object"}}
}
func (b *blobTool) Run(_ context.Context, _ string) (string, error) {
	b.calls++
	return "blob#" + fmt.Sprint(b.calls) + " " + bigText(3000), nil
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
		return AssistantToolCall(fmt.Sprintf("p%d", p.calls), ToolUpdatePlan, items), nil
	default:
		return AssistantToolCall(fmt.Sprintf("w%d", p.calls), "blob", `{}`), nil
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
	store := NewTodoStore()
	prov := &stressProvider{target: 120}

	limits := DefaultLimits()
	limits.MaxTurns = 400
	limits.MaxToolCalls = 500
	limits.MaxContextTokens = 4000 // tight budget -> frequent compaction
	cs := DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := New(Config{
		Provider:    prov,
		Model:       "stress",
		Tools:       NewToolSet(&blobTool{}, NewTodoTool(store)),
		Policy:      NewAllowList("blob", ToolUpdatePlan),
		Limits:     &limits,
		Compaction: &cs,
		Hooks:      Hooks{Context: []ContextHook{TodoContextHook(store)}},
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
	if len(store.List()) == 0 {
		t.Fatal("run plan was lost over the long run")
	}
}
