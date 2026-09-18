package agentcore

import (
	"context"
	"errors"
	"strings"
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

// flakySessionStore wraps memSessionStore, failing Append while broken (forever
// when failUntil is 0, else until failUntil failures have been served). It lets
// tests exercise the valid-prefix flush: entries buffered during an outage must
// be retried at the next save point, never dropped or written out of order.
type flakySessionStore struct {
	*memSessionStore
	broken    bool
	failUntil int
	failures  int
}

func (f *flakySessionStore) Append(ctx context.Context, id string, e SessionEntry) error {
	if f.broken {
		f.failures++
		if f.failUntil > 0 && f.failures >= f.failUntil {
			f.broken = false // outage over; this attempt still fails
		}
		return errors.New("store down")
	}
	return f.memSessionStore.Append(ctx, id, e)
}

// TestSteeringPersistedToSessionLog verifies a drained steering message is
// written to the durable log: a resume must rebuild the same conversation the
// model actually saw, corrections included.
func TestSteeringPersistedToSessionLog(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("ok"),
	)
	store := newMemSessionStore()
	var delivered bool
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(noopTool{}),
		Policy:    NewAllowList("noop"),
		Session:   store,
		SessionID: "s-steer",
		GetSteeringMessages: func(context.Context) []Message {
			if delivered {
				return nil
			}
			delivered = true
			return []Message{{Role: RoleUser, Content: "STEER: prefer option B"}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	log, _ := store.Log(context.Background(), "s-steer")
	var found bool
	for _, e := range log {
		if e.Kind == EntryMessage && e.Message != nil && strings.Contains(e.Message.Content, "STEER: prefer option B") {
			found = true
		}
	}
	if !found {
		t.Fatalf("drained steering message missing from durable log (%d entries)", len(log))
	}
	// And the reduced history must include it, in conversation order.
	rs := ReduceSession(log)
	var inHistory bool
	for _, m := range rs.Messages {
		if strings.Contains(m.Content, "STEER: prefer option B") {
			inHistory = true
		}
	}
	if !inHistory {
		t.Fatalf("steer missing from reduced resume history: %+v", rs.Messages)
	}
}

// TestFollowUpPersistedToSessionLog verifies a drained follow-up message is in
// the durable log for the same reason as a steer.
func TestFollowUpPersistedToSessionLog(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("first answer"),
		AssistantText("second answer"),
	)
	store := newMemSessionStore()
	var sent bool
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "s-follow",
		GetFollowUpMessages: func(context.Context) []Message {
			if sent {
				return nil
			}
			sent = true
			return []Message{{Role: RoleUser, Content: "now do the next thing"}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	log, _ := store.Log(context.Background(), "s-follow")
	var found bool
	for _, e := range log {
		if e.Kind == EntryMessage && e.Message != nil && strings.Contains(e.Message.Content, "now do the next thing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("drained follow-up missing from durable log (%d entries)", len(log))
	}
}

// TestCompactionUsageAccounted verifies the compaction summarization call's
// spend is folded into the run's usage and stamped on the durable completion
// entry — compaction is a real provider call, not free.
func TestCompactionUsageAccounted(t *testing.T) {
	// Turn 1 produces a bulky tool result; at the top of turn 2 the tiny context
	// budget forces compaction, whose Chat call consumes the scripted summary
	// response (with usage). The next response is the final answer.
	faux := NewFauxProvider(
		ChatResponse{
			Message:    Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "big", Arguments: "{}"}}},
			StopReason: "tool_calls",
			Usage:      Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.001},
		},
		ChatResponse{
			Message:    Message{Role: RoleAssistant, Content: "## Goal\nkeep going\n## Next Steps\n1. finish"},
			StopReason: "stop",
			Usage:      Usage{InputTokens: 100, OutputTokens: 20, CostUSD: 0.01},
		},
		ChatResponse{
			Message:    Message{Role: RoleAssistant, Content: "done"},
			StopReason: "stop",
			Usage:      Usage{InputTokens: 30, OutputTokens: 8, CostUSD: 0.003},
		},
	)
	store := newMemSessionStore()
	limits := DefaultLimits()
	limits.MaxContextTokens = 500
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Tools:      NewToolSet(bigResultTool{}),
		Policy:     NewAllowList("big"),
		Limits:     &limits,
		Compaction: &CompactionSettings{KeepRecentTokens: 1000},
		Session:    store,
		SessionID:  "s-compact",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start "+bigText(8000))
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) != 3 {
		t.Fatalf("expected 3 provider calls (turn, compaction, turn), got %d", len(faux.Recorded))
	}
	// Run usage must include the compaction call's spend.
	wantCost := 0.001 + 0.01 + 0.003
	if diff := res.Usage.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("run cost %.4f, want %.4f (compaction spend folded in)", res.Usage.CostUSD, wantCost)
	}
	if res.Usage.InputTokens != 140 {
		t.Fatalf("run input tokens %d, want 140", res.Usage.InputTokens)
	}
	// The durable completion entry carries the summarization usage.
	log, _ := store.Log(context.Background(), "s-compact")
	var stamped *Usage
	for _, e := range log {
		if e.Kind == EntryCompaction && e.Final {
			stamped = e.Usage
		}
	}
	if stamped == nil || stamped.InputTokens != 100 || stamped.OutputTokens != 20 {
		t.Fatalf("compaction completion entry missing usage stamp: %+v", stamped)
	}
}

// TestFlushRetriesTransientStoreFailure verifies a store outage during one save
// point self-heals: the unflushed entries are retried at the next save point,
// the final log is complete and ordered, and nothing is reported unpersisted.
func TestFlushRetriesTransientStoreFailure(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("ok"),
	)
	// The store serves exactly one failure: turn 1's flush loses its first
	// append attempt, and every later attempt (the retry at the next save
	// point included) succeeds.
	store := &flakySessionStore{memSessionStore: newMemSessionStore(), broken: true, failUntil: 1}
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(noopTool{}),
		Policy:    NewAllowList("noop"),
		Session:   store,
		SessionID: "s-flaky",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if store.failures == 0 {
		t.Fatal("test never exercised the outage")
	}
	if res.UnpersistedEntries != 0 {
		t.Fatalf("unpersisted=%d after store healed, want 0 (retry at next save point)", res.UnpersistedEntries)
	}
	log, _ := store.Log(context.Background(), "s-flaky")
	// The healed log must be the complete run: seed prompt, assistant turn, tool
	// result, final answer, leaf.
	var kinds []SessionEntryKind
	for _, e := range log {
		kinds = append(kinds, e.Kind)
	}
	if len(log) < 5 || log[0].Kind != EntryMessage || log[len(log)-1].Kind != EntryLeaf {
		t.Fatalf("healed log incomplete or out of order: %v", kinds)
	}
	rs := ReduceSession(log)
	if rs.Completed != true {
		t.Fatalf("healed log should reduce to a completed run: %+v", kinds)
	}
}

// TestFlushReportsPermanentStoreFailure verifies a store that never recovers
// degrades to a flagged result instead of silently dropping entries, and the
// written log (here: nothing) is a valid prefix rather than a log with holes.
func TestFlushReportsPermanentStoreFailure(t *testing.T) {
	faux := NewFauxProvider(AssistantText("ok"))
	store := &flakySessionStore{memSessionStore: newMemSessionStore(), broken: true}
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "s-dead",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.UnpersistedEntries == 0 {
		t.Fatal("permanent store failure must be reported via UnpersistedEntries")
	}
	log, _ := store.Log(context.Background(), "s-dead")
	if len(log) != 0 {
		t.Fatalf("dead store should hold no entries, got %d", len(log))
	}
}

// TestToolEffectsRequireDurableIntent verifies the pre-effect save point fails
// closed: if the assistant call cannot be recorded, the tool must not run and
// leave an external effect that recovery cannot attach to any durable intent.
func TestToolEffectsRequireDurableIntent(t *testing.T) {
	tool := &echoTool{name: "write"}
	store := &flakySessionStore{memSessionStore: newMemSessionStore(), broken: true}
	agent, err := New(Config{
		Provider:  NewFauxProvider(AssistantToolCall("c1", "write", `{}`)),
		Model:     "test",
		Tools:     NewToolSet(tool),
		Policy:    NewAllowList("write"),
		Session:   store,
		SessionID: "s-no-intent",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "write it"); err == nil {
		t.Fatal("run should fail when tool intent cannot be made durable")
	}
	if tool.called != 0 {
		t.Fatalf("tool ran %d times without durable intent", tool.called)
	}
}

// bigResultTool returns an oversized result so the context estimate crosses the
// compaction threshold within one turn.
type bigResultTool struct{}

func (bigResultTool) Name() string { return "big" }
func (bigResultTool) Schema() ToolSchema {
	return ToolSchema{Name: "big", Description: "returns a big result", Parameters: map[string]any{"type": "object"}}
}
func (bigResultTool) Run(context.Context, string) (string, error) { return bigText(8000), nil }

// TestPrepareNextTurnSwapsModel verifies the save-point hook can bump the model
// between turns: turn-1's request keeps the original model (the in-flight request
// is untouched), turn-2's request uses the new one.
func TestPrepareNextTurnSwapsModel(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`), // turn 1 -> loop continues
		AssistantText("done"),                 // turn 2 -> final
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "model-a",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
		PrepareNextTurn: func(_ context.Context, s TurnState) TurnState {
			s.Model = "model-b" // bump for the next turn
			return s
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) < 2 {
		t.Fatalf("expected 2 turns, got %d", len(faux.Recorded))
	}
	if got := faux.Recorded[0].Model; got != "model-a" {
		t.Fatalf("turn-1 request model = %q, want model-a (in-flight untouched)", got)
	}
	if got := faux.Recorded[1].Model; got != "model-b" {
		t.Fatalf("turn-2 request model = %q, want model-b (save-point applied)", got)
	}
}

// TestPrepareNextTurnEmptyKeepsCurrent verifies a hook returning zero-value
// fields does not blank the run's model or system prompt.
func TestPrepareNextTurnEmptyKeepsCurrent(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "model-a",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
		PrepareNextTurn: func(_ context.Context, _ TurnState) TurnState {
			return TurnState{} // careless hook: everything zero
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := faux.Recorded[1].Model; got != "model-a" {
		t.Fatalf("turn-2 model = %q, want model-a (empty hook must not blank it)", got)
	}
}

// TestGoalFromLogClearsAtLeaf pins the rule that keeps a chat continuation from
// inheriting a finished run's gate: the goal belongs to the run that recorded
// it, and EntryLeaf ends that run.
//
// Without this, a second prompt on the same session would resume gated by a
// goal the user already satisfied, and the model would be told to keep working
// on it forever.
func TestGoalFromLogClearsAtLeaf(t *testing.T) {
	cases := []struct {
		name    string
		entries []SessionEntry
		want    string
	}{
		{"no entries", nil, ""},
		{"goal only", []SessionEntry{{Kind: EntryGoal, Goal: "ship it"}}, "ship it"},
		{
			"last goal wins",
			[]SessionEntry{{Kind: EntryGoal, Goal: "first"}, {Kind: EntryGoal, Goal: "second"}},
			"second",
		},
		{
			"leaf clears the finished run's goal",
			[]SessionEntry{{Kind: EntryGoal, Goal: "ship it"}, {Kind: EntryLeaf}},
			"",
		},
		{
			"a run chained after a leaf carries its own goal",
			[]SessionEntry{
				{Kind: EntryGoal, Goal: "old"},
				{Kind: EntryLeaf},
				{Kind: EntryGoal, Goal: "new"},
			},
			"new",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := goalFromLog(tc.entries); got != tc.want {
				t.Fatalf("goalFromLog = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGoalFromLogMatchesRecoverSession keeps the two folds from drifting: the
// loop resolves the goal from goalFromLog before extensions begin, while
// RecoverSession derives it again for the recovery plan. If they ever disagree,
// a resumed run would be gated by one condition and reason about another.
func TestGoalFromLogMatchesRecoverSession(t *testing.T) {
	entries := []SessionEntry{
		{Kind: EntryGoal, Goal: "old"},
		{Kind: EntryLeaf},
		{Kind: EntryGoal, Goal: "current"},
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "go"}},
	}
	plan := RecoverSession(entries, NewToolSet(), RecoveryMarkInterrupted)
	if got := goalFromLog(entries); got != plan.Goal {
		t.Fatalf("goalFromLog = %q but RecoverSession = %q", got, plan.Goal)
	}
}

// TestSteerIsDurableAndDelivered verifies the durable inbox contract: Steer
// writes an EntryInbox side record immediately (intent), the loop delivers it
// as an EntryMessage on the next turn (effect), and settles it with an
// EntryInboxDone chain entry (settlement) so a later resume never re-delivers
// it.
func TestSteerIsDurableAndDelivered(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	faux := NewFauxProvider(
		AssistantToolCall("c1", "echo", `{}`),
		AssistantText("saw the steer"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(&echoTool{name: "echo"}),
		Policy:    NewAllowList("echo"),
		Session:   store,
		SessionID: "s-inbox",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Queue the steer before the run starts (same as queuing it mid-turn).
	if err := agent.Steer(ctx, Message{Role: RoleUser, Content: "focus on X"}); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	res, err := agent.Prompt(ctx, "hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "saw the steer" {
		t.Fatalf("final = %q", res.Final)
	}

	// The steer was delivered into the conversation before the second turn.
	var sawSteer bool
	for _, m := range res.Messages {
		if m.Role == RoleUser && m.Content == "focus on X" && m.Directive {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Fatal("steer was not delivered into the message history")
	}

	// The log carries all three steps of the op: intent -> message -> settlement.
	log, _ := store.Log(ctx, "s-inbox")
	var hasInbox, hasDone bool
	var inboxID string
	for _, e := range log {
		switch e.Kind {
		case EntryInbox:
			hasInbox = true
			inboxID = e.ID
		case EntryInboxDone:
			if e.Target == inboxID && inboxID != "" {
				hasDone = true
			}
		}
	}
	if !hasInbox {
		t.Fatal("EntryInbox was not written")
	}
	if !hasDone {
		t.Fatalf("EntryInboxDone was not written for inbox entry %q", inboxID)
	}

	// Re-reducing the log reports the inbox as settled (rs.Inbox is empty).
	rs := ReduceSession(log)
	if len(rs.Inbox) != 0 {
		t.Fatalf("reduced inbox must be empty after settlement, got %+v", rs.Inbox)
	}
}

// TestResumeDeliversPendingInbox verifies an inbox entry queued before a crash
// survives: the resumed run reads it from the log, delivers it on turn 1, and
// settles it.
func TestResumeDeliversPendingInbox(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	// Seed a crashed log: user prompt, an unfinished assistant turn, and an
	// EntryInbox that was queued before the crash but never settled.
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "start"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Arguments: "{}"}}}},
		{Kind: EntryInbox, ID: "inbox-1", Lane: InboxSteer, Message: &Message{Role: RoleUser, Content: "queued before crash"}},
	} {
		if err := store.Append(ctx, "r-inbox", e); err != nil {
			t.Fatal(err)
		}
	}

	// Recovery sees the pending inbox.
	log, _ := store.Log(ctx, "r-inbox")
	plan := RecoverSession(log, NewToolSet(&echoTool{name: "echo"}), RecoveryMarkInterrupted)
	if len(plan.Inbox) != 1 || plan.Inbox[0].Message.Content != "queued before crash" {
		t.Fatalf("recovered plan.Inbox = %+v, want the queued message", plan.Inbox)
	}

	// Resuming the agent delivers the queued message.
	faux := NewFauxProvider(AssistantText("resumed and handled inbox"))
	agent, err := New(Config{
		Provider:      faux,
		Model:         "test",
		Tools:         NewToolSet(&echoTool{name: "echo"}),
		Policy:        NewAllowList("echo"),
		Session:       store,
		SessionID:     "r-inbox",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "resumed and handled inbox" {
		t.Fatalf("final = %q", res.Final)
	}

	// Check settlement in the store.
	logAfter, _ := store.Log(ctx, "r-inbox")
	rs := ReduceSession(logAfter)
	if len(rs.Inbox) != 0 {
		t.Fatalf("inbox must be settled after resume, got %+v", rs.Inbox)
	}
}

// TestStreamingWritesPartialFrames verifies streamed turns write throttled
// EntryAssistantFrame side records, that buildChain skips them (they do not
// fork the chain), and that a trailing frame is recovered as a draft.
func TestStreamingWritesPartialFrames(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	// An assistant turn with enough text to cross the frame throttle, then a
	// crash (no leaf).
	faux := NewFauxProvider(
		AssistantText("first chunk of answer and then some more words to make it long enough"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "s-frames",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Run streamed so streamTurn is exercised.
	_, err = agent.PromptStream(ctx, "hi", func(StreamEvent) {})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}

	log, _ := store.Log(ctx, "s-frames")
	var frames int
	for _, e := range log {
		if e.Kind == EntryAssistantFrame {
			frames++
		}
	}
	if frames == 0 {
		t.Fatal("no EntryAssistantFrame records were written during streaming")
	}

	// Side records are NOT in the tree.
	nodes := SessionTree(log)
	for _, n := range nodes {
		if n.Entry.Kind == EntryAssistantFrame {
			t.Fatalf("SessionTree must not include side records, found frame %q", n.ID)
		}
	}

	// A settled turn's frames are superseded by its assistant message, so Draft is empty.
	rs := ReduceSession(log)
	if rs.Draft != "" {
		t.Fatalf("settled run must have no draft, got %q", rs.Draft)
	}

	// Simulate a crash mid-generation: append a trailing frame after a user prompt.
	crashedStore := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "write a poem"}},
		{Kind: EntryAssistantFrame, Turn: 1, Content: "the rose was red"},
	} {
		_ = crashedStore.Append(ctx, "crashed", e)
	}
	cLog, _ := crashedStore.Log(ctx, "crashed")
	cPlan := RecoverSession(cLog, nil, RecoveryMarkInterrupted)
	if cPlan.Draft != "the rose was red" {
		t.Fatalf("recovered draft = %q, want 'the rose was red'", cPlan.Draft)
	}
}

// progressTool is a StreamingTool that emits partial output before returning.
type progressTool struct {
	called int
}

func (p *progressTool) Name() string { return "progress_tool" }
func (p *progressTool) Schema() ToolSchema {
	return ToolSchema{Name: "progress_tool", Description: "streaming tool"}
}
func (p *progressTool) Run(context.Context, string) (string, error) {
	return "authoritative final", nil
}
func (p *progressTool) RunStreaming(_ context.Context, _ string, emit func(string)) (string, error) {
	p.called++
	emit("fetched 50/100 records")
	emit("fetched 100/100 records")
	return "authoritative final", nil
}

// TestToolProgressRecordedAndRecovered verifies streaming tool partials write
// EntryToolProgress side records, and that an interrupted call's note carries
// its last reported progress.
func TestToolProgressRecordedAndRecovered(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	pt := &progressTool{}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "progress_tool", `{}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(pt),
		Policy:    NewAllowList("progress_tool"),
		Session:   store,
		SessionID: "s-progress",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = agent.PromptStream(ctx, "fetch", func(StreamEvent) {})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}

	log, _ := store.Log(ctx, "s-progress")
	var progressRecords int
	for _, e := range log {
		if e.Kind == EntryToolProgress {
			progressRecords++
			if e.CallID != "c1" {
				t.Fatalf("progress CallID = %q, want c1", e.CallID)
			}
		}
	}
	if progressRecords == 0 {
		t.Fatal("no EntryToolProgress records were written")
	}

	// Simulate an interrupted run whose tool reported progress before the crash.
	crashedStore := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "fetch"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "progress_tool", Arguments: "{}"}}}},
		{Kind: EntryToolProgress, Turn: 1, CallID: "c1", Content: "fetched 50/100"},
	} {
		_ = crashedStore.Append(ctx, "crashed-tool", e)
	}
	// progressTool does not implement RetrySafeTool, so it is dropped and gets
	// an interrupted note.
	resumeFaux := NewFauxProvider(AssistantText("recovered"))
	resumedAgent, err := New(Config{
		Provider:      resumeFaux,
		Model:         "test",
		Tools:         NewToolSet(&echoTool{name: "progress_tool"}), // plain, not retry-safe
		Policy:        NewAllowList("progress_tool"),
		Session:       crashedStore,
		SessionID:     "crashed-tool",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := resumedAgent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// The interrupted note carries the last reported progress.
	var sawProgressInNote bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.ToolCallID == "c1" && strings.Contains(m.Content, "Last reported progress: fetched 50/100") {
			sawProgressInNote = true
		}
	}
	if !sawProgressInNote {
		t.Fatalf("interrupted note did not carry progress: %+v", res.Messages)
	}
}

// TestFollowUpIsDurableAndDelivered verifies FollowUp queues work that drains
// after the agent would stop, restarts the loop, and settles its inbox entry.
func TestFollowUpIsDurableAndDelivered(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	faux := NewFauxProvider(
		AssistantText("first answer"),
		AssistantText("second answer after follow-up"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "s-follow-durable",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Queue the follow-up before the run starts.
	if err := agent.FollowUp(ctx, Message{Role: RoleUser, Content: "also do Y"}); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}

	res, err := agent.Prompt(ctx, "do X")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "second answer after follow-up" {
		t.Fatalf("final = %q", res.Final)
	}
	if res.Turns != 2 {
		t.Fatalf("turns = %d, want 2", res.Turns)
	}

	// Check settlement.
	log, _ := store.Log(ctx, "s-follow-durable")
	rs := ReduceSession(log)
	if len(rs.Inbox) != 0 {
		t.Fatalf("follow-up inbox must be settled, got %+v", rs.Inbox)
	}
}

// TestSideRecordsConcurrentWrites ensures direct appends from tool goroutines
// and the stream frame writer do not race with the save-point flush.
func TestSideRecordsConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Append(ctx, "concurrent", SessionEntry{
				Kind:    EntryToolProgress,
				CallID:  "c",
				Content: "progress",
			})
		}(i)
	}
	wg.Wait()
	log, _ := store.Log(ctx, "concurrent")
	if len(log) != 20 {
		t.Fatalf("expected 20 entries, got %d", len(log))
	}
}

// TestFoldSeesFramesBehindAFlushedTurn covers the ordering a graceful
// interruption produces: mid-turn side records land first, then the deferred
// flush commits the turn's buffered chain entries AFTER them. A tail scan that
// stops at the first chain entry loses the draft; the fold must bound the
// in-flight window by the last SETTLING entry (assistant/tool message), not
// the last chain entry.
func TestFoldSeesFramesBehindAFlushedTurn(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	user := Message{Role: RoleUser, Content: "go"}
	steer := Message{Role: RoleUser, Content: "mid-turn steer"}
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Turn: 1, Message: &user},
		// Turn 2 streams a draft and tool progress, delivers a steer (buffered
		// chain entries), then is interrupted — the deferred flush commits the
		// buffered entries AFTER the side records.
		{Kind: EntryAssistantFrame, Turn: 2, Content: "half-written answer"},
		{Kind: EntryToolProgress, Turn: 2, CallID: "c1", Content: "tool got this far"},
		{Kind: EntryMessage, Turn: 2, Message: &steer},
		{Kind: EntryInboxDone, Turn: 2, Target: "inbox-9"},
	} {
		if err := store.Append(ctx, "s", e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	log, _ := store.Log(ctx, "s")
	rs := ReduceSession(log)
	if rs.Draft != "half-written answer" {
		t.Fatalf("draft behind flushed chain entries must be recovered, got %q", rs.Draft)
	}
	if rs.ToolProgress["c1"] != "tool got this far" {
		t.Fatalf("tool progress behind flushed chain entries must be recovered, got %v", rs.ToolProgress)
	}
}

// TestInboxDoesNotLeakAcrossRewind covers both directions of branch leakage:
// an intent settled on an abandoned branch is pending again after the rewind
// (its delivered message is gone), and an intent queued on the abandoned
// branch is not delivered into the new one.
func TestInboxDoesNotLeakAcrossRewind(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	q := Message{Role: RoleUser, Content: "question"}
	a1 := Message{Role: RoleAssistant, Content: "first answer"}
	steer := Message{Role: RoleUser, Content: "delivered steer"}
	a2 := Message{Role: RoleAssistant, Content: "steered answer"}
	stale := Message{Role: RoleUser, Content: "queued on the old branch"}
	entries := []SessionEntry{
		{Kind: EntryMessage, Turn: 1, Message: &q},                               // seq 0
		{Kind: EntryMessage, Turn: 1, Message: &a1},                              // seq 1
		{Kind: EntryInbox, ID: "i-delivered", Lane: InboxSteer, Message: &steer}, // seq 2, anchored to seq 1
		{Kind: EntryMessage, Turn: 2, Message: &steer},                           // seq 3 (delivered)
		{Kind: EntryInboxDone, Turn: 2, Target: "i-delivered"},                   // seq 4
		{Kind: EntryMessage, Turn: 2, Message: &a2},                              // seq 5
		{Kind: EntryInbox, ID: "i-stale", Lane: InboxSteer, Message: &stale},     // seq 6, anchored to seq 5
	}
	for _, e := range entries {
		if err := store.Append(ctx, "s", e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Rewind to just after the first answer: the delivered steer, its
	// settlement, the second answer, and the stale intent all sit on the
	// abandoned branch.
	if _, err := Rewind(ctx, store, "s", "#1", BranchOptions{}); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	log, _ := store.Log(ctx, "s")
	rs := ReduceSession(log)

	var ids []string
	for _, it := range rs.Inbox {
		ids = append(ids, it.ID)
	}
	if len(ids) != 1 || ids[0] != "i-delivered" {
		t.Fatalf("after rewind the rewound-away delivery must be pending again and the stale intent dropped, got %v", ids)
	}
}
