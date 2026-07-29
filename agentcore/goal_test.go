package agentcore

import (
	"context"
	"strings"
	"testing"
)

// TestGoalGateHoldsUntilDone verifies a finish without a sentinel re-opens the
// run with the keep-going nudge, and a later STATUS: DONE answer is accepted.
func TestGoalGateHoldsUntilDone(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("made some progress"),
		AssistantText("still working on it"),
		AssistantText("all fixed.\nSTATUS: DONE"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Goal:     "all tests pass",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "fix the build")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(res.Final, "STATUS: DONE") {
		t.Fatalf("final = %q, want the STATUS: DONE answer", res.Final)
	}
	if res.StopReason != "stop" {
		t.Fatalf("stop reason = %q, want normal stop", res.StopReason)
	}
	if len(faux.Recorded) != 3 {
		t.Fatalf("provider calls = %d, want 3 (two nudges)", len(faux.Recorded))
	}
	// The contract must ride in the system prompt of every request.
	sys := faux.Recorded[0].Messages[0]
	if sys.Role != RoleSystem || !strings.Contains(sys.Content, "all tests pass") || !strings.Contains(sys.Content, GoalDone) {
		t.Fatalf("goal contract missing from system prompt: %q", sys.Content)
	}
	// Each re-opened request must carry the nudge as a user message.
	var nudges int
	for _, req := range faux.Recorded {
		for _, m := range req.Messages {
			if m.Role == RoleUser && strings.Contains(m.Content, "[goal gate]") {
				nudges++
				break
			}
		}
	}
	if nudges != 2 {
		t.Fatalf("nudge present in %d requests, want 2", nudges)
	}
}

// TestGoalGateAcceptsBlocked verifies the honest-blocker escape hatch: a
// STATUS: BLOCKED answer ends the run on the first finish.
func TestGoalGateAcceptsBlocked(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("Need prod credentials only you have.\nSTATUS: BLOCKED"),
	)
	agent, err := New(Config{Provider: faux, Model: "test", Goal: "deploy to prod"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "ship it")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) != 1 {
		t.Fatalf("provider calls = %d, want 1 (no nudge)", len(faux.Recorded))
	}
	if res.StopReason != "stop" {
		t.Fatalf("stop reason = %q, want normal stop", res.StopReason)
	}
}

// TestGoalGateCaseInsensitive verifies sentinel matching tolerates casing.
func TestGoalGateCaseInsensitive(t *testing.T) {
	faux := NewFauxProvider(AssistantText("Everything works now. Status: done"))
	agent, err := New(Config{Provider: faux, Model: "test", Goal: "make it work"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) != 1 {
		t.Fatalf("provider calls = %d, want 1 — lower-case sentinel must count", len(faux.Recorded))
	}
}

// TestGoalGateSentinelMustCloseAnswer verifies a mid-prose mention of the
// sentinel does not close the gate: only the answer's last non-empty line
// counts, per the contract's "end your final answer with the line".
func TestGoalGateSentinelMustCloseAnswer(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("I cannot yet write STATUS: DONE — the tests still fail.\nNext I will retry the flaky suite."),
		AssistantText("all green now.\nSTATUS: DONE"),
	)
	agent, err := New(Config{Provider: faux, Model: "test", Goal: "all tests pass"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "fix the build")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2 — a mid-prose sentinel mention must nudge", len(faux.Recorded))
	}
	if !strings.Contains(res.Final, "all green") {
		t.Fatalf("final = %q, want the post-nudge answer", res.Final)
	}
}

// TestGoalGateSurvivesResume verifies a crashed goal-gated run resumed WITHOUT
// re-supplying Config.Goal is still gated: the goal comes back from the durable
// log's EntryGoal record.
func TestGoalGateSurvivesResume(t *testing.T) {
	store := newMemSessionStore()
	ctx := context.Background()
	// Simulate the crashed run's log: the goal record plus the seed prompt,
	// no leaf.
	for _, e := range []SessionEntry{
		{Kind: EntryGoal, Goal: "the report is published"},
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "publish the report"}},
	} {
		if err := store.Append(ctx, "sess-goal-resume", e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	faux := NewFauxProvider(
		AssistantText("resumed, still working"),
		AssistantText("published.\nSTATUS: DONE"),
	)
	agent, err := New(Config{
		Provider:      faux,
		Model:         "test",
		Session:       store,
		SessionID:     "sess-goal-resume",
		ResumeSession: true,
		// Goal deliberately empty: the resume caller cannot re-supply it.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2 — the recovered goal must nudge the sentinel-free finish", len(faux.Recorded))
	}
	sys := faux.Recorded[0].Messages[0]
	if sys.Role != RoleSystem || !strings.Contains(sys.Content, "the report is published") {
		t.Fatalf("goal contract missing from resumed system prompt: %q", sys.Content)
	}
	if !strings.Contains(res.Final, "STATUS: DONE") {
		t.Fatalf("final = %q, want the STATUS: DONE answer", res.Final)
	}
}

// TestGoalGateStallBreaker verifies a model repeating the same sentinel-free
// answer after a nudge stops the run as goal_stalled instead of burning turns.
func TestGoalGateStallBreaker(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("I cannot make further progress."),
		AssistantText("I cannot make further progress."),
	)
	agent, err := New(Config{Provider: faux, Model: "test", Goal: "impossible thing"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "try")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "goal_stalled" {
		t.Fatalf("stop reason = %q, want goal_stalled", res.StopReason)
	}
	if len(faux.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2 (one nudge, then stall accept)", len(faux.Recorded))
	}
	if res.Final != "I cannot make further progress." {
		t.Fatalf("final = %q, want the repeated answer", res.Final)
	}
}

// TestGoalGateSkippedOnBudgetWrapUp verifies the budget ceiling always wins:
// a wrap-up turn ends the run even without a sentinel.
func TestGoalGateSkippedOnBudgetWrapUp(t *testing.T) {
	faux := NewFauxProvider(AssistantText("partial progress summary"))
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Goal:       "never satisfied",
		BudgetGate: func(context.Context, Usage) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "budget_exhausted" {
		t.Fatalf("stop reason = %q, want budget_exhausted", res.StopReason)
	}
	if len(faux.Recorded) != 1 {
		t.Fatalf("provider calls = %d, want 1 — no goal nudge on wrap-up", len(faux.Recorded))
	}
}

// TestGoalGateBoundedByMaxTurns verifies an ever-changing sentinel-free answer
// still terminates at the MaxTurns budget.
func TestGoalGateBoundedByMaxTurns(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("attempt one"),
		AssistantText("attempt two"),
		AssistantText("attempt three"),
		AssistantText("attempt four"),
	)
	limits := DefaultLimits()
	limits.MaxTurns = 3
	agent, err := New(Config{Provider: faux, Model: "test", Goal: "unreachable", Limits: &limits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "max_turns" {
		t.Fatalf("stop reason = %q, want max_turns", res.StopReason)
	}
	if len(faux.Recorded) != 3 {
		t.Fatalf("provider calls = %d, want 3 (MaxTurns bound)", len(faux.Recorded))
	}
}

// TestGoalGateNudgePersistedDurably verifies the injected nudge lands in the
// durable log, so a resumed run replays the same conversation the model saw.
func TestGoalGateNudgePersistedDurably(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("not there yet"),
		AssistantText("done now.\nSTATUS: DONE"),
	)
	store := newMemSessionStore()
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Goal:      "finish the report",
		Session:   store,
		SessionID: "sess-goal",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	entries, err := store.Log(context.Background(), "sess-goal")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Kind == EntryMessage && e.Message != nil && e.Message.Role == RoleUser && strings.Contains(e.Message.Content, "[goal gate]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("goal nudge not persisted to the durable log (%d entries)", len(entries))
	}
}

// TestGoalGateRunsBeforeFinishGuard verifies ordering: an unmet goal nudges
// without consulting the finish guard; the guard sees only sentinel-bearing
// finishes.
func TestGoalGateRunsBeforeFinishGuard(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("no sentinel yet"),
		AssistantText("finished.\nSTATUS: DONE"),
	)
	var guardSaw []string
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Goal:     "do the thing",
		FinishGuard: func(_ context.Context, s FinishState) string {
			guardSaw = append(guardSaw, s.Final)
			return ""
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(guardSaw) != 1 || !strings.Contains(guardSaw[0], "STATUS: DONE") {
		t.Fatalf("finish guard consulted with %v, want only the sentinel-bearing final", guardSaw)
	}
}
