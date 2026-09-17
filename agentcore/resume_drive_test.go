package agentcore

import (
	"context"
	"strings"
	"testing"
)

// seedCrashedLog writes a minimal crashed-run log: the user task, then an
// assistant turn whose tool call never got a result (the process died mid-tool).
func seedCrashedLog(t *testing.T, store SessionStore, sessionID, callID, toolName string) {
	t.Helper()
	ctx := context.Background()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: callID, Name: toolName, Arguments: "{}"}}}},
	} {
		if err := store.Append(ctx, sessionID, e); err != nil {
			t.Fatal(err)
		}
	}
}

// TestResumeSessionReplaysRetrySafeCall pins the drive-level resume contract:
// a dangling call whose tool is retry-safe is re-issued before the first model
// turn, with its ORIGINAL call id, and its real result — not an interrupted
// note — reaches the model and the durable log.
func TestResumeSessionReplaysRetrySafeCall(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	seedCrashedLog(t, store, "r1", "c9", "safe_read")

	provider := NewFauxProvider(AssistantText("done after replay"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Tools:         NewToolSet(retrySafeProbe{}),
		Policy:        NewAllowList("safe_read"),
		Session:       store,
		SessionID:     "r1",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "done after replay" {
		t.Fatalf("final = %q", res.Final)
	}

	// The model's first request saw the replayed result, not an interrupted note.
	var sawResult, sawNote bool
	for _, m := range provider.Recorded[0].Messages {
		if m.Role == RoleTool && m.ToolCallID == "c9" {
			if m.Content == "ok" {
				sawResult = true
			}
			if strings.Contains(m.Content, "[interrupted:") {
				sawNote = true
			}
		}
	}
	if !sawResult || sawNote {
		t.Fatalf("replay must feed the real result (result=%v note=%v): %+v", sawResult, sawNote, provider.Recorded[0].Messages)
	}
	// The replay is traced like a live execution.
	if len(res.Tools) == 0 || res.Tools[0].Tool != "safe_read" {
		t.Fatalf("replayed call missing from traces: %+v", res.Tools)
	}

	// The log completes, holds the replayed result exactly once, and does not
	// duplicate the recovered history.
	log, _ := store.Log(ctx, "r1")
	rs := ReduceSession(log)
	if !rs.Completed {
		t.Fatal("resumed log must reduce Completed")
	}
	results, seeds := 0, 0
	for _, e := range log {
		if e.Kind != EntryMessage || e.Message == nil {
			continue
		}
		if e.Message.Role == RoleTool && e.Message.ToolCallID == "c9" {
			results++
		}
		if e.Message.Role == RoleUser && e.Message.Content == "count the beans" {
			seeds++
		}
	}
	if results != 1 || seeds != 1 {
		t.Fatalf("log shape wrong: results=%d seeds=%d (want 1/1)", results, seeds)
	}
}

// TestResumeSessionClosesUnsafeCall verifies a dangling call whose tool is NOT
// retry-safe is never re-run: it is closed with an interrupted note that both
// reaches the model and is persisted, so the model decides what to do next.
func TestResumeSessionClosesUnsafeCall(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	seedCrashedLog(t, store, "r2", "c9", "noop")

	tool := noopTool{}
	provider := NewFauxProvider(AssistantText("acknowledged the interruption"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Tools:         NewToolSet(tool),
		Policy:        NewAllowList("noop"),
		Session:       store,
		SessionID:     "r2",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "resume"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var sawNote bool
	for _, m := range provider.Recorded[0].Messages {
		if m.Role == RoleTool && m.ToolCallID == "c9" && strings.Contains(m.Content, "[interrupted:") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Fatalf("non-retry-safe dangling call must be closed with a note: %+v", provider.Recorded[0].Messages)
	}
	// The note is durable: a second resume reduces a transcript with no dangling call.
	log, _ := store.Log(ctx, "r2")
	plan := RecoverSession(log, nil, RecoveryMarkInterrupted)
	if len(plan.DroppedCalls) != 0 || len(plan.RetryCalls) != 0 {
		t.Fatalf("persisted note must satisfy the call: %+v", plan)
	}
}

// TestResumeSessionReattachesCompletedLog verifies a log that already reached
// its leaf returns the recorded final answer without a single provider call.
func TestResumeSessionReattachesCompletedLog(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "42 beans"}},
		{Kind: EntryLeaf},
	} {
		if err := store.Append(ctx, "r3", e); err != nil {
			t.Fatal(err)
		}
	}

	provider := NewFauxProvider(AssistantText("should never be produced"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Session:       store,
		SessionID:     "r3",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "42 beans" {
		t.Fatalf("reattach must return the recorded answer, got %q", res.Final)
	}
	if res.StopReason != "reattached" {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if len(provider.Recorded) != 0 {
		t.Fatalf("reattach must not call the provider (%d calls)", len(provider.Recorded))
	}
}

// TestResumeSessionEmptyLogRunsFresh verifies ResumeSession degrades cleanly:
// with nothing in the log the run behaves exactly like a fresh one, persisting
// its seed exactly once.
func TestResumeSessionEmptyLogRunsFresh(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	agent, err := New(Config{
		Provider:      NewFauxProvider(AssistantText("fresh answer")),
		Model:         "test",
		Session:       store,
		SessionID:     "r4",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "fresh answer" {
		t.Fatalf("final = %q", res.Final)
	}
	log, _ := store.Log(ctx, "r4")
	seeds := 0
	for _, e := range log {
		if e.Kind == EntryMessage && e.Message != nil && e.Message.Role == RoleUser && e.Message.Content == "hello" {
			seeds++
		}
	}
	if seeds != 1 {
		t.Fatalf("seed persisted %d times, want 1", seeds)
	}
}

// TestResumeRestoresActiveToolsAndModel verifies the durable run state beyond
// the transcript is honored on resume: a crashed run that had swapped its tool
// set (EntryActiveToolsChange) and escalated to another model
// (EntryModelChange) comes back on THAT set and THAT rung — not the registry
// and primary model it was configured with.
func TestResumeRestoresActiveToolsAndModel(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	// A crashed run: user task, a mid-run tool swap to just "b", an escalation
	// to model "big", then an assistant turn that died mid-work.
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryActiveToolsChange, Tools: []string{"b"}},
		{Kind: EntryModelChange, Model: "big"},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "b", Arguments: "{}"}}}},
	} {
		if err := store.Append(ctx, "r5", e); err != nil {
			t.Fatal(err)
		}
	}

	primary := NewFauxProvider(AssistantText("primary must not be called"))
	escalated := NewFauxProvider(AssistantText("resumed on the recorded rung"))
	agent, err := New(Config{
		Provider:      primary,
		Model:         "small",
		Escalation:    []ModelRung{{Provider: escalated, Model: "big"}},
		Tools:         NewToolSet(&echoTool{name: "a"}, &echoTool{name: "b"}),
		Policy:        NewAllowList("a", "b"),
		Session:       store,
		SessionID:     "r5",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "resumed on the recorded rung" {
		t.Fatalf("final = %q", res.Final)
	}
	if len(primary.Recorded) != 0 {
		t.Fatal("resume must start on the recorded rung, not re-pay the failed primary")
	}
	if len(escalated.Recorded) != 1 {
		t.Fatalf("escalated provider calls = %d, want 1", len(escalated.Recorded))
	}
	// The recorded active set is advertised, not the full registry.
	req := escalated.Recorded[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != "b" {
		t.Fatalf("resumed run must advertise the recorded tool set [b], got %+v", req.Tools)
	}
}

// TestPrepareNextTurnToolSwapIsDurable verifies a save-point tool swap writes
// EntryActiveToolsChange — the entry a resume reads to rebuild the set — and
// that a hook returning the same set every turn writes nothing.
func TestPrepareNextTurnToolSwapIsDurable(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	narrow := NewToolSet(&echoTool{name: "b"})
	faux := NewFauxProvider(
		AssistantToolCall("c1", "a", `{}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(&echoTool{name: "a"}, &echoTool{name: "b"}),
		Policy:    NewAllowList("a", "b"),
		Session:   store,
		SessionID: "r6",
		PrepareNextTurn: func(_ context.Context, s TurnState) TurnState {
			s.Tools = narrow
			return s
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Turn 2 advertised only the swapped-in set.
	if len(faux.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(faux.Recorded))
	}
	if got := faux.Recorded[1].Tools; len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("turn 2 must advertise only [b], got %+v", got)
	}
	// The swap is in the durable log exactly once.
	log, _ := store.Log(ctx, "r6")
	changes := 0
	for _, e := range log {
		if e.Kind == EntryActiveToolsChange {
			changes++
			if len(e.Tools) != 1 || e.Tools[0] != "b" {
				t.Fatalf("recorded tool set = %v, want [b]", e.Tools)
			}
		}
	}
	if changes != 1 {
		t.Fatalf("EntryActiveToolsChange written %d times, want 1", changes)
	}
}
