package agentcore

import (
	"context"
	"strings"
	"testing"
)

func TestLogInvariant_QuietWhenEverythingIsLogged(t *testing.T) {
	var violations []LogInvariantViolation
	li := newLogInvariant(true, func(v LogInvariantViolation) { violations = append(violations, v) })

	history := []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	}
	li.noteAll(history)
	li.check(1, history)
	if len(violations) != 0 {
		t.Fatalf("a fully logged history must not report: %+v", violations)
	}
}

func TestLogInvariant_CatchesAnUnloggedInjection(t *testing.T) {
	var violations []LogInvariantViolation
	li := newLogInvariant(true, func(v LogInvariantViolation) { violations = append(violations, v) })

	li.note(Message{Role: RoleUser, Content: "hello"})
	// A nudge reached the model but was never persisted — the exact bug the
	// invariant exists to catch.
	history := []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleUser, Content: "keep going, you did not finish"},
	}
	li.check(3, history)
	if len(violations) != 1 {
		t.Fatalf("expected exactly one violation, got %d", len(violations))
	}
	v := violations[0]
	if v.Turn != 3 || v.Role != RoleUser || !strings.Contains(v.Excerpt, "keep going") {
		t.Fatalf("violation does not identify the message: %+v", v)
	}
	if !strings.Contains(v.Error(), "never logged") {
		t.Fatalf("error text = %q", v.Error())
	}
}

func TestLogInvariant_CountsDuplicatesSeparately(t *testing.T) {
	var violations []LogInvariantViolation
	li := newLogInvariant(true, func(v LogInvariantViolation) { violations = append(violations, v) })

	m := Message{Role: RoleUser, Content: "same text"}
	li.note(m) // logged once
	li.check(1, []Message{m, m})
	if len(violations) != 1 {
		t.Fatal("a second identical message must not hide behind the first")
	}
	// Once both are logged, it goes quiet.
	violations = nil
	li.note(m)
	li.check(1, []Message{m, m})
	if len(violations) != 0 {
		t.Fatalf("both copies are logged now: %+v", violations)
	}
}

func TestLogInvariant_ToolResultsWithSameTextButDifferentCallsAreDistinct(t *testing.T) {
	var violations []LogInvariantViolation
	li := newLogInvariant(true, func(v LogInvariantViolation) { violations = append(violations, v) })

	logged := Message{Role: RoleTool, Name: "run_sql", ToolCallID: "call_1", Content: "0 rows"}
	unlogged := Message{Role: RoleTool, Name: "run_sql", ToolCallID: "call_2", Content: "0 rows"}
	li.note(logged)
	li.check(1, []Message{logged, unlogged})
	if len(violations) != 1 {
		t.Fatal("identical text under a different call id is a different message")
	}
}

func TestLogInvariant_RebaseAcceptsADeliberateRewrite(t *testing.T) {
	var violations []LogInvariantViolation
	li := newLogInvariant(true, func(v LogInvariantViolation) { violations = append(violations, v) })

	li.noteAll([]Message{
		{Role: RoleUser, Content: "a"},
		{Role: RoleAssistant, Content: "b"},
	})
	// Compaction replaces the transcript; the log carries the bracket.
	compacted := []Message{{Role: RoleUser, Content: "summary of a and b"}}
	li.rebase(compacted)
	li.check(2, compacted)
	if len(violations) != 0 {
		t.Fatalf("a rebased history must not report: %+v", violations)
	}
	// And the pre-rebase messages are no longer accepted, so a rewrite that
	// silently kept stale history would still be caught.
	li.check(2, []Message{{Role: RoleUser, Content: "a"}})
	if len(violations) != 1 {
		t.Fatal("rebase must replace the baseline, not extend it")
	}
}

func TestLogInvariant_ReportsAtMostOnePerCheck(t *testing.T) {
	var violations []LogInvariantViolation
	li := newLogInvariant(true, func(v LogInvariantViolation) { violations = append(violations, v) })
	li.check(1, []Message{
		{Role: RoleUser, Content: "one"},
		{Role: RoleUser, Content: "two"},
		{Role: RoleUser, Content: "three"},
	})
	if len(violations) != 1 {
		t.Fatalf("a systemic divergence must not flood the sink: got %d reports", len(violations))
	}
}

func TestLogInvariant_DisabledIsANoop(t *testing.T) {
	li := newLogInvariant(false, func(LogInvariantViolation) { t.Fatal("must not report when disabled") })
	if li != nil {
		t.Fatal("disabled must yield a nil tracker")
	}
	// Every method is nil-safe, so the loop never has to branch on it.
	li.note(Message{Role: RoleUser, Content: "x"})
	li.noteAll([]Message{{Role: RoleUser, Content: "y"}})
	li.rebase([]Message{{Role: RoleUser, Content: "z"}})
	li.check(1, []Message{{Role: RoleUser, Content: "unlogged"}})

	// A nil report function also disables it: a check with nowhere to report is
	// wasted work, not a silent check.
	if newLogInvariant(true, nil) != nil {
		t.Fatal("no reporter means no tracker")
	}
}

// TestLogInvariant_EndToEndOnADurableRun drives the real loop and proves the
// wiring: every message the loop injects on a durable run is logged, so a clean
// run reports nothing.
func TestLogInvariant_EndToEndCleanRun(t *testing.T) {
	store := newMemSessionStore()
	var violations []LogInvariantViolation

	provider := &FauxProvider{Responses: []ChatResponse{
		{Message: Message{Role: RoleAssistant, Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "done"}},
	}}
	agent, err := New(Config{
		Provider:                provider,
		Model:                   "m",
		Tools:                   NewToolSet(&echoTool{name: "echo"}),
		Policy:                  NewAllowList("echo"),
		Session:                 store,
		SessionID:               "s1",
		OnLogInvariantViolation: func(v LogInvariantViolation) { violations = append(violations, v) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "say hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("a clean durable run must log everything it shows the model: %+v", violations)
	}
}

// TestLogInvariant_EndToEndWithRepeatNudgeStaysClean is the regression guard for
// the newest injector: the repeat-tool reminder writes a synthetic user message
// into the live history, and this proves it reaches the durable log too. If a
// future change drops that appendEntry, this test fails instead of resume
// quietly diverging.
func TestLogInvariant_EndToEndWithRepeatNudgeStaysClean(t *testing.T) {
	store := newMemSessionStore()
	var violations []LogInvariantViolation

	// Three identical tool calls in a row trip the repeat guard, which injects a
	// synthetic user message. If that injection were not persisted, this test
	// would fail — which is exactly the regression guard we want.
	repeat := ChatResponse{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "echo", Arguments: `{"text":"same"}`}}}}
	provider := &FauxProvider{Responses: []ChatResponse{
		repeat, repeat, repeat,
		{Message: Message{Role: RoleAssistant, Content: "done"}},
	}}
	agent, err := New(Config{
		Provider:                provider,
		Model:                   "m",
		Tools:                   NewToolSet(&echoTool{name: "echo"}),
		Policy:                  NewAllowList("echo"),
		Session:                 store,
		SessionID:               "s1",
		RepeatGuard:             &RepeatGuardSettings{Thresholds: []int{3}},
		OnLogInvariantViolation: func(v LogInvariantViolation) { violations = append(violations, v) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("the repeat-guard nudge must be persisted like a steer: %+v", violations)
	}
	// And it really did fire, so the test is not vacuous.
	found := false
	for _, m := range res.Messages {
		if m.Role == RoleUser && strings.Contains(m.Content, "repeating the exact same tool call") {
			found = true
		}
	}
	if !found {
		t.Fatal("the repeat nudge never fired — this test proves nothing without it")
	}
	// The log carries it too.
	log, _ := store.Log(context.Background(), "s1")
	loggedNudge := false
	for _, e := range log {
		if e.Kind == EntryMessage && e.Message != nil && strings.Contains(e.Message.Content, "repeating the exact same tool call") {
			loggedNudge = true
		}
	}
	if !loggedNudge {
		t.Fatal("nudge is visible to the model but missing from the durable log")
	}
}
