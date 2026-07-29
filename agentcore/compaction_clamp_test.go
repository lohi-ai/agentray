package agentcore

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// These tests pin the keep-recent clamp (effectiveCompaction). The wedge it
// fixes, found by the live collision-sim benchmark (bench/collision_test.go):
// with Limits.MaxContextTokens set BELOW the default KeepRecentTokens (20k),
// shouldCompact fires every turn but findCutPoint sees the whole transcript
// inside the "recent" window (cut 0), and the deterministic-elide fallback only
// collapses bulky TOOL RESULTS — so a run whose bulk lives in assistant
// tool-call ARGUMENTS (the write-a-full-file shape) never shrinks: compaction
// runs forever and changes nothing.

func TestEffectiveCompactionClampsKeepRecentToHalfBudget(t *testing.T) {
	// Small budget, default settings: clamp to budget/2.
	got := effectiveCompaction(CompactionSettings{}, 6000)
	if got.KeepRecentTokens != 3000 {
		t.Fatalf("KeepRecentTokens = %d, want 3000 (budget/2)", got.KeepRecentTokens)
	}
	// Explicit setting below the clamp is honored untouched.
	got = effectiveCompaction(CompactionSettings{KeepRecentTokens: 1500}, 6000)
	if got.KeepRecentTokens != 1500 {
		t.Fatalf("KeepRecentTokens = %d, want 1500 (explicit, under clamp)", got.KeepRecentTokens)
	}
	// Default budget: default settings pass through unchanged.
	got = effectiveCompaction(CompactionSettings{}, 0)
	if got.KeepRecentTokens != defaultKeepRecentTokens {
		t.Fatalf("KeepRecentTokens = %d, want default %d", got.KeepRecentTokens, defaultKeepRecentTokens)
	}
}

// sinkTool accepts a large content argument and returns a tiny confirmation —
// the bulk stays in the CALL, mirroring a full-file write tool.
type sinkTool struct{ calls int }

func (s *sinkTool) Name() string { return "write_file" }
func (s *sinkTool) Schema() ToolSchema {
	return ToolSchema{Name: "write_file", Description: "write a file", Parameters: map[string]any{"type": "object"}}
}
func (s *sinkTool) Run(_ context.Context, _ string) (string, error) {
	s.calls++
	return fmt.Sprintf("wrote #%d", s.calls), nil
}

// bigArgsProvider emits tool calls whose ARGUMENTS carry the bulk (unique per
// call so no editing rule applies), and doubles as the compaction summarizer.
type bigArgsProvider struct {
	target    int
	calls     int
	Summaries int
	argSize   int
}

func (p *bigArgsProvider) Name() string        { return "bigargs" }
func (p *bigArgsProvider) SupportsTools() bool { return true }
func (p *bigArgsProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.Summaries++
		return AssistantText("## Goal\nKeep writing the file\n## Next Steps\n1. next milestone"), nil
	}
	p.calls++
	if p.calls >= p.target {
		return AssistantText("DONE"), nil
	}
	args := fmt.Sprintf(`{"path":"page.html","content":"%s-%d"}`, bigText(p.argSize), p.calls)
	return AssistantToolCall(fmt.Sprintf("w%d", p.calls), "write_file", args), nil
}
func (p *bigArgsProvider) Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error) {
	ch := make(chan ChatDelta, 4)
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

func TestCompactionFiresWhenBulkIsInToolCallArguments(t *testing.T) {
	prov := &bigArgsProvider{target: 20, argSize: 3000} // ~750 est. tokens per call

	limits := DefaultLimits()
	limits.MaxTurns = 60
	limits.MaxToolCalls = 80
	limits.MaxContextTokens = 4000 // deliberately below default KeepRecentTokens

	agent, err := New(Config{
		Provider: prov,
		Model:    "bigargs",
		Tools:    NewToolSet(&sinkTool{}),
		Policy:   NewAllowList("write_file"),
		Limits:   &limits,
		// No Compaction override: the default 20k keep-recent window must be
		// clamped to the 4k budget or this run wedges (the pre-fix behavior).
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "build the page milestone by milestone")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if res.Final != "DONE" {
		t.Fatalf("run did not finish (stop=%q final=%q turns=%d)", res.StopReason, res.Final, res.Turns)
	}
	if prov.Summaries == 0 {
		t.Fatalf("compaction never summarized: big tool-call arguments under a small budget must compact (turns=%d)", res.Turns)
	}
	var sawSummary bool
	for _, m := range res.Messages {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			sawSummary = true
			break
		}
	}
	if !sawSummary {
		t.Fatal("no summary checkpoint in the final transcript")
	}
	// Bounded: 20 turns of ~750-token calls is ~15k tokens uncompacted; the
	// transcript must have been folded down, not merely marked.
	if est := estimateContextTokens(res.Messages); est > 3*limits.MaxContextTokens {
		t.Fatalf("transcript not bounded: estimated %d tokens against a %d budget", est, limits.MaxContextTokens)
	}
}
