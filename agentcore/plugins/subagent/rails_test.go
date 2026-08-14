package subagent_test

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
)

// spawnTool reaches the model-facing tool the way the loop does: begin the
// plugin for a run, then take the tool it contributes.
func spawnTool(t *testing.T) interface {
	agentcore.Tool
	RetrySafeCall(agentcore.ToolCall) bool
} {
	t.Helper()
	ext, err := subagent.SelfOnly().BeginRun(context.Background(), agentcore.RunInfo{})
	if err != nil || ext == nil {
		t.Fatalf("BeginRun = %v, %v", ext, err)
	}
	tools := ext.(interface{ Tools() []agentcore.Tool }).Tools()
	if len(tools) != 1 {
		t.Fatalf("expected exactly one contributed tool, got %d", len(tools))
	}
	tool, ok := tools[0].(interface {
		agentcore.Tool
		RetrySafeCall(agentcore.ToolCall) bool
	})
	if !ok {
		t.Fatalf("%T must declare per-call retry safety", tools[0])
	}
	return tool
}

// TestSpawnRetrySafetyIsCallConditional pins the spawn_subagent retry contract:
// only self-forks are retry-safe (their child session ID is deterministic in
// the call, so a replay reattaches), while delegate-routed spawns — which have
// no reattach wiring — and malformed calls are not.
func TestSpawnRetrySafetyIsCallConditional(t *testing.T) {
	tool := spawnTool(t)
	for args, want := range map[string]bool{
		`{"task":"t"}`:                   true,
		`{"task":"t","agent":"self"}`:    true,
		`{"task":"t","agent":"Self"}`:    true,
		`{"task":"t","agent":"analyst"}`: false,
		`not json`:                       false,
	} {
		if got := tool.RetrySafeCall(agentcore.ToolCall{Name: subagent.ToolSpawnSubagent, Arguments: args}); got != want {
			t.Errorf("RetrySafeCall(%s) = %v, want %v", args, got, want)
		}
	}

	// Recovery classification honors the per-call verdict: a dangling self-fork
	// is replayed, a dangling delegate spawn is dropped (closed with a note).
	log := []agentcore.SessionEntry{
		{Kind: agentcore.EntryMessage, Message: &agentcore.Message{Role: agentcore.RoleUser, Content: "go"}},
		{Kind: agentcore.EntryMessage, Message: &agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{
			{ID: "c1", Name: subagent.ToolSpawnSubagent, Arguments: `{"task":"t"}`},
			{ID: "c2", Name: subagent.ToolSpawnSubagent, Arguments: `{"task":"t","agent":"analyst"}`},
		}}},
	}
	plan := agentcore.RecoverSession(log, agentcore.NewToolSet(tool), agentcore.RecoveryMarkInterrupted)
	if len(plan.RetryCalls) != 1 || plan.RetryCalls[0].ID != "c1" {
		t.Fatalf("self-fork must be retried: %+v", plan.RetryCalls)
	}
	if len(plan.DroppedCalls) != 1 || plan.DroppedCalls[0].ID != "c2" {
		t.Fatalf("delegate spawn must be dropped, not replayed: %+v", plan.DroppedCalls)
	}
}
