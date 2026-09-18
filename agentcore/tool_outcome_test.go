package agentcore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type outcomeWatchStore struct {
	*MemorySessionStore
	outcomes chan SessionEntry
}

type transientOutcomeStore struct {
	*MemorySessionStore
	failed atomic.Bool
}

func (s *transientOutcomeStore) Append(ctx context.Context, sessionID string, entry SessionEntry) error {
	if entry.Kind == EntryToolOutcome && s.failed.CompareAndSwap(false, true) {
		return errors.New("transient outcome append failure")
	}
	return s.MemorySessionStore.Append(ctx, sessionID, entry)
}

func (s *transientOutcomeStore) AppendBatch(ctx context.Context, sessionID string, entries []SessionEntry) error {
	return s.MemorySessionStore.AppendBatch(ctx, sessionID, entries)
}

func (s *outcomeWatchStore) Append(ctx context.Context, sessionID string, entry SessionEntry) error {
	if err := s.MemorySessionStore.Append(ctx, sessionID, entry); err != nil {
		return err
	}
	if entry.Kind == EntryToolOutcome {
		s.outcomes <- entry
	}
	return nil
}

func (s *outcomeWatchStore) AppendBatch(ctx context.Context, sessionID string, entries []SessionEntry) error {
	return s.MemorySessionStore.AppendBatch(ctx, sessionID, entries)
}

type outcomeProbeTool struct {
	name    string
	started chan<- struct{}
	release <-chan struct{}
	calls   atomic.Int32
}

func (t *outcomeProbeTool) Name() string   { return t.name }
func (t *outcomeProbeTool) Parallel() bool { return true }
func (t *outcomeProbeTool) Schema() ToolSchema {
	return ToolSchema{Name: t.name, Description: "outcome probe", Parameters: map[string]any{"type": "object"}}
}
func (t *outcomeProbeTool) Run(ctx context.Context, _ string) (string, error) {
	t.calls.Add(1)
	if t.started != nil {
		t.started <- struct{}{}
	}
	if t.release == nil {
		return t.name + "-result", nil
	}
	select {
	case <-t.release:
		return t.name + "-result", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func twoOutcomeCalls() ChatResponse {
	return ChatResponse{
		Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "slow-call", Name: "slow", Arguments: "{}"},
			{ID: "fast-call", Name: "fast", Arguments: "{}"},
		}},
		StopReason: "tool_calls",
	}
}

func TestParallelToolOutcomeIsDurableBeforeSlowSiblingFinishes(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	slowStarted := make(chan struct{}, 1)
	store := &outcomeWatchStore{MemorySessionStore: NewMemorySessionStore(), outcomes: make(chan SessionEntry, 2)}
	slow := &outcomeProbeTool{name: "slow", started: slowStarted, release: release}
	fast := &outcomeProbeTool{name: "fast"}
	agent, err := New(Config{
		Provider: NewFauxProvider(twoOutcomeCalls(), AssistantText("done")),
		Model:    "test", Tools: NewToolSet(slow, fast), Policy: NewAllowList("slow", "fast"),
		Session: store, SessionID: "parallel-outcomes",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := agent.Prompt(ctx, "run both")
		done <- runErr
	}()
	select {
	case <-slowStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("slow tool did not start")
	}
	var fastEntry SessionEntry
	select {
	case fastEntry = <-store.outcomes:
		if fastEntry.CallID != "fast-call" {
			t.Fatalf("first completed outcome = %q, want fast-call", fastEntry.CallID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fast outcome was not persisted while slow sibling remained blocked")
	}

	log, err := store.Log(ctx, "parallel-outcomes")
	if err != nil {
		t.Fatal(err)
	}
	var intent bool
	for _, entry := range log {
		if entry.Kind == EntryMessage && entry.Message != nil && entry.Message.Role == RoleAssistant && len(entry.Message.ToolCalls) == 2 {
			intent = true
		}
		if entry.Kind == EntryMessage && entry.Message != nil && entry.Message.Role == RoleTool {
			t.Fatalf("canonical result became visible before the source-order group settled: %+v", entry.Message)
		}
	}
	if !intent {
		t.Fatal("assistant tool-call intent was not committed before execution")
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not finish after releasing slow tool")
	}
	log, _ = store.Log(ctx, "parallel-outcomes")
	var resultIDs []string
	for _, entry := range log {
		if entry.Kind == EntryMessage && entry.Message != nil && entry.Message.Role == RoleTool {
			resultIDs = append(resultIDs, entry.Message.ToolCallID)
		}
	}
	if len(resultIDs) != 2 || resultIDs[0] != "slow-call" || resultIDs[1] != "fast-call" {
		t.Fatalf("canonical result order = %v, want [slow-call fast-call]", resultIDs)
	}
}

func TestResumeMaterializesDurableOutcomesInSourceOrderWithoutReplay(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySessionStore()
	assistant := Message{Role: RoleAssistant, ToolCalls: []ToolCall{
		{ID: "slow-call", Name: "slow", Arguments: "{}"},
		{ID: "fast-call", Name: "fast", Arguments: "{}"},
	}}
	entries := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "run both"}},
		{Kind: EntryMessage, Message: &assistant},
		// Physical completion order is the reverse of transcript source order.
		{Kind: EntryToolOutcome, CallID: "fast-call", Outcome: &ToolOutcomeRecord{
			Message: Message{Role: RoleTool, ToolCallID: "fast-call", Name: "fast", Content: "fast-result"},
			Trace:   ToolTrace{CallID: "fast-call", Tool: "fast", Args: "{}", Allowed: true}, Executed: true,
		}},
		{Kind: EntryToolOutcome, CallID: "slow-call", Outcome: &ToolOutcomeRecord{
			Message: Message{Role: RoleTool, ToolCallID: "slow-call", Name: "slow", Content: "slow-result"},
			Trace:   ToolTrace{CallID: "slow-call", Tool: "slow", Args: "{}", Allowed: true}, Executed: true,
		}},
	}
	for _, entry := range entries {
		if err := store.Append(ctx, "recover-outcomes", entry); err != nil {
			t.Fatal(err)
		}
	}
	logged, _ := store.Log(ctx, "recover-outcomes")
	plan := RecoverSession(logged, nil, RecoveryMarkInterrupted)
	if len(plan.ToolOutcomes) != 2 || len(plan.RetryCalls) != 0 || len(plan.DroppedCalls) != 0 {
		t.Fatalf("unexpected recovery plan: %+v", plan)
	}

	provider := NewFauxProvider(AssistantText("continued"))
	agent, err := New(Config{
		Provider: provider, Model: "test", Session: store, SessionID: "recover-outcomes", ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("durable outcomes were not traced: %+v", res.Tools)
	}
	var got []string
	for _, message := range provider.Recorded[0].Messages {
		if message.Role == RoleTool {
			got = append(got, message.ToolCallID+":"+message.Content)
		}
	}
	if len(got) != 2 || got[0] != "slow-call:slow-result" || got[1] != "fast-call:fast-result" {
		t.Fatalf("recovered result order = %v", got)
	}
	log, _ := store.Log(ctx, "recover-outcomes")
	counts := map[string]int{}
	for _, entry := range log {
		if entry.Kind == EntryMessage && entry.Message != nil && entry.Message.Role == RoleTool {
			counts[entry.Message.ToolCallID]++
		}
	}
	if counts["slow-call"] != 1 || counts["fast-call"] != 1 {
		t.Fatalf("canonical outcomes should materialize once: %v", counts)
	}
}

func TestCancelledToolDoesNotPersistCompletedOutcome(t *testing.T) {
	started := make(chan struct{}, 1)
	tool := &outcomeProbeTool{name: "slow", started: started, release: make(chan struct{})}
	store := NewMemorySessionStore()
	agent, err := New(Config{
		Provider: NewFauxProvider(AssistantToolCall("c1", "slow", "{}")),
		Model:    "test", Tools: NewToolSet(tool), Policy: NewAllowList("slow"),
		Session: store, SessionID: "cancelled-outcome",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := agent.Prompt(ctx, "start")
		done <- runErr
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled run did not stop")
	}
	log, _ := store.Log(context.Background(), "cancelled-outcome")
	for _, entry := range log {
		if entry.Kind == EntryToolOutcome {
			t.Fatalf("cancellation placeholder persisted as a completed outcome: %+v", entry)
		}
	}
}

func TestCanonicalToolResultSupersedesOutcomeSideRecord(t *testing.T) {
	toolMessage := Message{Role: RoleTool, ToolCallID: "c1", Name: "read", Content: "done"}
	state := ReduceSession([]SessionEntry{
		{Seq: 1, Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}}}},
		{Seq: 2, Kind: EntryToolOutcome, CallID: "c1", Outcome: &ToolOutcomeRecord{Message: toolMessage}},
		{Seq: 3, Kind: EntryMessage, Message: &toolMessage},
	})
	if len(state.ToolOutcomes) != 0 {
		t.Fatalf("settled side outcome remained live: %+v", state.ToolOutcomes)
	}
}

func TestToolOutcomeRetriesTransientSideWriteFailure(t *testing.T) {
	store := &transientOutcomeStore{MemorySessionStore: NewMemorySessionStore()}
	tool := &outcomeProbeTool{name: "fast"}
	agent, err := New(Config{
		Provider: NewFauxProvider(AssistantToolCall("c1", "fast", "{}"), AssistantText("done")),
		Model:    "test", Tools: NewToolSet(tool), Policy: NewAllowList("fast"),
		Session: store, SessionID: "retry-outcome",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "run"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !store.failed.Load() {
		t.Fatal("test did not exercise the outcome-write failure")
	}
	log, _ := store.Log(context.Background(), "retry-outcome")
	var outcomes int
	for _, entry := range log {
		if entry.Kind == EntryToolOutcome && entry.CallID == "c1" {
			outcomes++
		}
	}
	if outcomes != 1 {
		t.Fatalf("durable outcomes = %d, want one successful retry", outcomes)
	}
}

func TestRewindDropsOutcomeFromAbandonedBranch(t *testing.T) {
	state := ReduceSession([]SessionEntry{
		{Seq: 1, ID: "user", Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "start"}},
		{Seq: 2, ID: "calls", ParentID: "user", Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "write", Arguments: "{}"}}}},
		{Seq: 3, Kind: EntryToolOutcome, CallID: "c1", Outcome: &ToolOutcomeRecord{Message: Message{Role: RoleTool, ToolCallID: "c1", Content: "done"}}},
		{Seq: 4, Kind: EntryLeafMove, Target: "user"},
	})
	if len(state.ToolOutcomes) != 0 {
		t.Fatalf("abandoned branch outcome leaked into active branch: %+v", state.ToolOutcomes)
	}
}

func TestOutcomeUsesExplicitIntentAnchorAcrossInterleavedBranches(t *testing.T) {
	state := ReduceSession([]SessionEntry{
		{Seq: 1, ID: "root", Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "start"}},
		{Seq: 2, ID: "calls-a", ParentID: "root", Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "a", Name: "write", Arguments: "{}"}}}},
		{Seq: 3, Kind: EntryLeafMove, Target: "root"},
		{Seq: 4, ID: "branch-b", ParentID: "root", Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "different branch"}},
		// A late completion from branch A lands after branch B's node. ParentID,
		// not raw append position, determines its ownership.
		{Seq: 5, Kind: EntryToolOutcome, ParentID: "calls-a", CallID: "a", Outcome: &ToolOutcomeRecord{Message: Message{Role: RoleTool, ToolCallID: "a", Content: "done"}}},
	})
	if len(state.ToolOutcomes) != 0 {
		t.Fatalf("interleaved abandoned outcome attached to the active branch: %+v", state.ToolOutcomes)
	}
}
