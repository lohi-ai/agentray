package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// unreadableLogStore accepts appends but cannot read its log back.
type unreadableLogStore struct{ *memSessionStore }

func (unreadableLogStore) Log(context.Context, string) ([]SessionEntry, error) {
	return nil, errors.New("disk exploded")
}

// TestResumeFailsOnUnreadableLog verifies a resume whose durable log cannot be
// read fails loudly instead of silently degrading to a from-scratch run that
// would splice fresh seeds onto (and redo work recorded in) the crashed log.
func TestResumeFailsOnUnreadableLog(t *testing.T) {
	agent, err := New(Config{
		Provider:      NewFauxProvider(AssistantText("should never run")),
		Model:         "test",
		Session:       unreadableLogStore{newMemSessionStore()},
		SessionID:     "r5",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = agent.Prompt(context.Background(), "resume")
	if err == nil || !strings.Contains(err.Error(), "resume: reading durable session log") {
		t.Fatalf("unreadable log must fail the resume, got %v", err)
	}
}

// TestTerminalToolWritesLeaf verifies a run ended by a terminal tool (an After
// hook returning terminate=true) still records its leaf, so the durable log
// reduces to Completed=true and a later resume reattaches instead of replaying
// the terminal call.
func TestTerminalToolWritesLeaf(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	terminal := func(_ context.Context, _ ToolCall, result string, _ error) (string, bool) {
		return result, true
	}
	provider := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("should never be produced"),
	)
	agent, err := New(Config{
		Provider:  provider,
		Model:     "test",
		Tools:     NewToolSet(noopTool{}),
		Policy:    NewAllowList("noop"),
		Hooks:     Hooks{After: []AfterToolCall{terminal}},
		Session:   store,
		SessionID: "r6",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "finish it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.Recorded) != 1 {
		t.Fatalf("terminal tool must end the run after one turn (%d provider calls)", len(provider.Recorded))
	}
	log, _ := store.Log(ctx, "r6")
	if rs := ReduceSession(log); !rs.Completed {
		t.Fatalf("terminal exit must reduce Completed=true: %+v", rs)
	}
}

// TestReplayHonorsMaxToolCalls verifies resume replay runs under the run's tool
// budget: with MaxToolCalls=1 and two dangling retry-safe calls, exactly one is
// re-executed and the other is closed with an interrupted note.
func TestReplayHonorsMaxToolCalls(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "safe_read", Arguments: "{}"},
			{ID: "c2", Name: "safe_read", Arguments: "{}"},
		}}},
	} {
		if err := store.Append(ctx, "r7", e); err != nil {
			t.Fatal(err)
		}
	}

	provider := NewFauxProvider(AssistantText("done"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Tools:         NewToolSet(retrySafeProbe{}),
		Policy:        NewAllowList("safe_read"),
		Limits:        &Limits{MaxTurns: 4, MaxToolCalls: 1, MaxToolResultLen: 1 << 16, MaxContextTokens: 1 << 20},
		Session:       store,
		SessionID:     "r7",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "resume"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	replayed, interrupted := 0, 0
	for _, m := range provider.Recorded[0].Messages {
		if m.Role != RoleTool {
			continue
		}
		switch {
		case m.Content == "ok":
			replayed++
		case strings.Contains(m.Content, "[interrupted:"):
			interrupted++
		}
	}
	if replayed != 1 || interrupted != 1 {
		t.Fatalf("budget must cap replay (replayed=%d interrupted=%d, want 1/1): %+v",
			replayed, interrupted, provider.Recorded[0].Messages)
	}
}
