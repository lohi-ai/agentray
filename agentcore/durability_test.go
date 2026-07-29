package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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

// bigResultTool returns an oversized result so the context estimate crosses the
// compaction threshold within one turn.
type bigResultTool struct{}

func (bigResultTool) Name() string { return "big" }
func (bigResultTool) Schema() ToolSchema {
	return ToolSchema{Name: "big", Description: "returns a big result", Parameters: map[string]any{"type": "object"}}
}
func (bigResultTool) Run(context.Context, string) (string, error) { return bigText(8000), nil }
