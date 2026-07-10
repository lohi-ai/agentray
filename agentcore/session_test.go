package agentcore

import (
	"context"
	"sync"
	"testing"
)

// memSessionStore is an in-memory append-only SessionStore for tests.
type memSessionStore struct {
	mu  sync.Mutex
	log map[string][]SessionEntry
}

func newMemSessionStore() *memSessionStore {
	return &memSessionStore{log: map[string][]SessionEntry{}}
}

func (m *memSessionStore) Append(_ context.Context, id string, e SessionEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.Seq = len(m.log[id])
	m.log[id] = append(m.log[id], e)
	return nil
}

func (m *memSessionStore) Log(_ context.Context, id string) ([]SessionEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionEntry, len(m.log[id]))
	copy(out, m.log[id])
	return out, nil
}

// retrySafeProbe is a retry-safe (idempotent) tool.
type retrySafeProbe struct{}

func (retrySafeProbe) Name() string { return "safe_read" }
func (retrySafeProbe) Schema() ToolSchema {
	return ToolSchema{Name: "safe_read", Description: "idempotent read", Parameters: map[string]any{"type": "object"}}
}
func (retrySafeProbe) Run(context.Context, string) (string, error) { return "ok", nil }
func (retrySafeProbe) RetrySafe() bool                             { return true }

// TestDurableRunProducesReducibleLog runs a real agent against an in-memory
// store and verifies the resulting log reduces back to the same message history
// and is marked completed (a leaf was written).
func TestDurableRunProducesReducibleLog(t *testing.T) {
	store := newMemSessionStore()
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("final answer"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(noopTool{}),
		Policy:    NewAllowList("noop"),
		Session:   store,
		SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	log, _ := store.Log(context.Background(), "s1")
	rs := ReduceSession(log)
	if !rs.Completed {
		t.Fatalf("reduced state should be completed (leaf written): %+v", rs)
	}
	if rs.PendingCompaction {
		t.Fatalf("no compaction happened; PendingCompaction must be false")
	}
	// History: user, assistant(toolcall), tool result, assistant(final).
	if len(rs.Messages) != 4 {
		t.Fatalf("reduced messages = %d, want 4: %+v", len(rs.Messages), rs.Messages)
	}
	if rs.Messages[len(rs.Messages)-1].Content != "final answer" {
		t.Fatalf("last reduced message = %q", rs.Messages[len(rs.Messages)-1].Content)
	}

	// A completed run recovers to "nothing to do".
	plan := RecoverSession(log, agent.tools, RecoveryMarkInterrupted)
	if !plan.Completed || plan.Interrupted {
		t.Fatalf("completed run should not be interrupted: %+v", plan)
	}
}

// TestRecoverInterruptedAfterToolResult simulates a crash: the log ends at a
// tool result with no following assistant turn and no leaf. Recovery resumes to
// the same history and marks the turn interrupted.
func TestRecoverInterruptedAfterToolResult(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "do it"}},
		{Kind: EntryMessage, Turn: 1, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "safe_read"}}}},
		{Kind: EntryMessage, Turn: 1, Message: &Message{Role: RoleTool, ToolCallID: "c1", Name: "safe_read", Content: "ok"}},
		// crash here: no next assistant turn, no leaf.
	}
	tools := NewToolSet(retrySafeProbe{})
	plan := RecoverSession(log, tools, RecoveryMarkInterrupted)
	if plan.Completed {
		t.Fatalf("run did not complete; Completed must be false")
	}
	if !plan.Interrupted {
		t.Fatalf("interrupted run must be flagged: %+v", plan)
	}
	if len(plan.Messages) != 3 {
		t.Fatalf("should resume to the same 3-message leaf, got %d", len(plan.Messages))
	}
	// The tool call was satisfied (result present), so nothing dangling to retry.
	if len(plan.RetryCalls) != 0 || len(plan.DroppedCalls) != 0 {
		t.Fatalf("satisfied call should not be re-run: retry=%v dropped=%v", plan.RetryCalls, plan.DroppedCalls)
	}
}

// TestRecoverDanglingCallRetrySafety verifies a crash between an assistant tool
// call and its result re-runs only retry-safe tools; non-idempotent tools are
// dropped (left for the model), never silently re-executed.
func TestRecoverDanglingCallRetrySafety(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "go"}},
		{Kind: EntryMessage, Turn: 1, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "safe_read"}, // retry-safe
			{ID: "c2", Name: "noop"},      // not retry-safe (noopTool has no RetrySafe)
		}}},
		// crash: neither tool produced a result.
	}
	tools := NewToolSet(retrySafeProbe{}, noopTool{})
	plan := RecoverSession(log, tools, RecoveryMarkInterrupted)
	if !plan.Interrupted {
		t.Fatalf("dangling calls must mark the run interrupted")
	}
	if len(plan.RetryCalls) != 1 || plan.RetryCalls[0].Name != "safe_read" {
		t.Fatalf("retry-safe call should be queued: %+v", plan.RetryCalls)
	}
	if len(plan.DroppedCalls) != 1 || plan.DroppedCalls[0].Name != "noop" {
		t.Fatalf("non-idempotent call must be dropped, not retried: %+v", plan.DroppedCalls)
	}
}

// TestRecoverUnfinishedCompactionReRuns verifies a compaction start with no
// completion entry sets RerunCompaction.
func TestRecoverUnfinishedCompactionReRuns(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "x"}},
		{Kind: EntryCompaction, Turn: 2}, // started, never completed
	}
	plan := RecoverSession(log, nil, RecoveryMarkInterrupted)
	if !plan.RerunCompaction {
		t.Fatalf("unfinished compaction must set RerunCompaction: %+v", plan)
	}

	// With a completion entry it is considered done.
	log = append(log, SessionEntry{Kind: EntryCompaction, Turn: 2, Final: true})
	if RecoverSession(log, nil, RecoveryMarkInterrupted).RerunCompaction {
		t.Fatalf("completed compaction must not re-run")
	}
}

// TestReduceSessionReconstructsDisabledTools verifies the circuit breaker's
// disable records reduce into a deduplicated DisabledTools set on both the
// reduced state and the resume plan.
func TestReduceSessionReconstructsDisabledTools(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "go"}},
		{Kind: EntryToolDisabled, Turn: 2, Tool: "flaky"},
		{Kind: EntryToolDisabled, Turn: 2, Tool: "flaky"}, // duplicate: must fold to one
		{Kind: EntryToolDisabled, Turn: 3, Tool: "other"},
	}
	rs := ReduceSession(log)
	if len(rs.DisabledTools) != 2 || rs.DisabledTools[0] != "flaky" || rs.DisabledTools[1] != "other" {
		t.Fatalf("reduced disabled set wrong: %v", rs.DisabledTools)
	}
	plan := RecoverSession(log, nil, RecoveryMarkInterrupted)
	if len(plan.DisabledTools) != 2 {
		t.Fatalf("resume plan should carry the disabled set: %v", plan.DisabledTools)
	}
}

// TestCircuitBreakerDisableSurvivesResume is the end-to-end durability proof:
// a tool that fails repeatedly is disabled and that verdict is written to the
// durable log; recovering the log surfaces the disabled tool; and a fresh run
// seeded with it (as a resume does) refuses the tool without executing it —
// the broken tool is not retried from scratch after a crash.
func TestCircuitBreakerDisableSurvivesResume(t *testing.T) {
	// Run 1: the breaker trips and logs the disable.
	store := newMemSessionStore()
	tool1 := &flakyTool{name: "flaky"}
	faux1 := NewFauxProvider(
		AssistantToolCall("c1", "flaky", `{}`),
		AssistantToolCall("c2", "flaky", `{}`),
		AssistantToolCall("c3", "flaky", `{}`),
		AssistantText("done"),
	)
	agent1, err := New(Config{
		Provider:  faux1,
		Model:     "test",
		Tools:     NewToolSet(tool1),
		Policy:    NewAllowList("flaky"),
		Session:   store,
		SessionID: "r1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent1.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// The disable is durable: the log carries an EntryToolDisabled for the tool.
	log, _ := store.Log(context.Background(), "r1")
	var logged bool
	for _, e := range log {
		if e.Kind == EntryToolDisabled && e.Tool == "flaky" {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("disable was not written to the durable log: %+v", log)
	}

	// Recovery surfaces it, exactly as ResumeRun seeds a resumed run.
	plan := RecoverSession(log, agent1.tools, RecoveryMarkInterrupted)
	if len(plan.DisabledTools) != 1 || plan.DisabledTools[0] != "flaky" {
		t.Fatalf("recovery did not surface the disabled tool: %v", plan.DisabledTools)
	}

	// Run 2 (the resume): seeded with the disabled tool, it must not be advertised
	// or executed — the model can't burn the resumed run on the same broken tool.
	tool2 := &flakyTool{name: "flaky"}
	faux2 := NewFauxProvider(
		AssistantToolCall("c1", "flaky", `{}`),
		AssistantText("finished without it"),
	)
	agent2, err := New(Config{
		Provider:          faux2,
		Model:             "test",
		Tools:             NewToolSet(tool2),
		Policy:            NewAllowList("flaky"),
		SeedDisabledTools: plan.DisabledTools,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res2, err := agent2.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if tool2.called != 0 {
		t.Fatalf("seeded-disabled tool must not execute on resume, ran %d times", tool2.called)
	}
	for _, s := range faux2.Recorded[0].Tools {
		if s.Name == "flaky" {
			t.Fatal("seeded-disabled tool must not be advertised on resume")
		}
	}
	var blocked bool
	for _, tr := range res2.Tools {
		if tr.Tool == "flaky" && !tr.Allowed {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("the model's call to the seeded-disabled tool should be refused")
	}
}
