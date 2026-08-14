package compaction_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/compaction"
	"github.com/lohi-ai/agentray/agentcore/plugins/model"
	"github.com/lohi-ai/agentray/agentcore/plugins/preset"
)

// prunerCompactor is a second compaction strategy written entirely outside
// agentcore: it drops old tool results instead of summarizing them, and makes no
// model call at all. It is deepseek-harness's compaction-tool-result-pruner in
// miniature, and it exists here to prove the claim the seam was extracted for —
// that a different strategy is a sibling package, not an edit to the loop.
type prunerCompactor struct{ compacted *int }

func (prunerCompactor) Name() string { return "pruner" }

// ShouldCompact deliberately measures pressure differently from the built-in
// (message count, not estimated tokens), because a strategy that could not
// choose its own trigger would only be half replaceable.
func (prunerCompactor) ShouldCompact(messages []agentcore.Message, _ int) bool {
	return len(messages) > 4
}

func (p prunerCompactor) Compact(_ context.Context, req agentcore.CompactionRequest) (agentcore.CompactionResult, error) {
	*p.compacted++
	out := make([]agentcore.Message, 0, len(req.Messages))
	for i, m := range req.Messages {
		// Keep the leading system prompt and the last two messages verbatim;
		// blank out older tool results, preserving call linkage.
		if m.Role == agentcore.RoleTool && i < len(req.Messages)-2 {
			out = append(out, agentcore.Message{
				Role: agentcore.RoleTool, ToolCallID: m.ToolCallID, Name: m.Name,
				Content: "[pruned]",
			})
			continue
		}
		out = append(out, m)
	}
	return agentcore.CompactionResult{Messages: out}, nil
}

// TestCustomStrategyReplacesTheBuiltIn is the seam's whole point: a compactor
// defined outside agentcore runs instead of the built-in summarizer, with no
// change to the loop and no model call.
func TestCustomStrategyReplacesTheBuiltIn(t *testing.T) {
	calls := 0
	faux := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c1", "echo", `{"v":"1"}`),
		agentcore.AssistantToolCall("c2", "echo", `{"v":"2"}`),
		agentcore.AssistantText("done"),
	)
	agent, err := preset.New(agentcore.Config{
		Provider:  faux,
		Model:     "test",
		Tools:     agentcore.NewToolSet(&echoTool{}),
		Policy:    agentcore.NewAllowList("echo"),
		Compactor: prunerCompactor{compacted: &calls},
	})
	if err != nil {
		t.Fatalf("preset.New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if calls == 0 {
		t.Fatal("the custom compactor never ran — the loop is still calling its own strategy")
	}
	// The built-in would have made a summarization call; this one must not have.
	for _, req := range faux.Recorded {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "structured context checkpoint") {
				t.Fatal("the built-in summarizer ran despite a replacement being installed")
			}
		}
	}
	if desc := agent.Describe(); !strings.Contains(desc, "compactor:") || !strings.Contains(desc, "pruner") {
		t.Fatalf("Describe() does not report the installed strategy:\n%s", desc)
	}
}

// TestDefaultStrategyStaysInstalled pins the other half: a composition that says
// nothing about compaction still gets the built-in, so extracting the seam did
// not quietly turn compaction off.
func TestDefaultStrategyStaysInstalled(t *testing.T) {
	agent, err := preset.New(agentcore.Config{
		Provider: agentcore.NewFauxProvider(agentcore.AssistantText("ok")),
		Model:    "test",
		Policy:   agentcore.DenyAll{},
	})
	if err != nil {
		t.Fatalf("preset.New: %v", err)
	}
	if desc := agent.Describe(); !strings.Contains(desc, "summary") {
		t.Fatalf("default compactor missing:\n%s", desc)
	}
}

// TestTwoStrategiesIsABuildError proves the seam is keyed: compaction has one
// provider, and a second claim names both plugins rather than silently winning.
func TestTwoStrategiesIsABuildError(t *testing.T) {
	calls := 0
	_, err := agentcore.Build(
		model.Plugin{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("ok")), Model: "m"},
		compaction.Using(prunerCompactor{compacted: &calls}),
		compaction.Using(prunerCompactor{compacted: &calls}),
	)
	if err == nil {
		t.Fatal("two compaction strategies composed without complaint")
	}
	if !strings.Contains(err.Error(), "compact") {
		t.Fatalf("error should name the contested seam, got: %v", err)
	}
}

// echoTool returns bulk, so the transcript grows enough to compact.
type echoTool struct{}

func (*echoTool) Name() string { return "echo" }

func (*echoTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: "echo", Description: "echo", Parameters: map[string]any{"type": "object"}}
}

func (*echoTool) Run(context.Context, string) (string, error) {
	return strings.Repeat("payload ", 64), nil
}
