package agentcore

import (
	"context"
	"strings"
	"testing"
)

// TestFinishGuardNudgeReopensRun verifies a guard rejecting the first finish
// injects its nudge as a user message, the loop continues, and the guard's
// evidence snapshot carries the answer that triggered it.
func TestFinishGuardNudgeReopensRun(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("unverified claim"),
		AssistantText("verified answer"),
	)
	var states []FinishState
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		FinishGuard: func(_ context.Context, s FinishState) string {
			states = append(states, s)
			if s.Nudges == 0 {
				return "NUDGE: verify before finishing"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "verified answer" {
		t.Fatalf("final = %q, want the post-nudge answer", res.Final)
	}
	if len(states) != 2 {
		t.Fatalf("guard consulted %d times, want 2", len(states))
	}
	if states[0].Final != "unverified claim" || states[0].Nudges != 0 {
		t.Fatalf("first consult state wrong: %+v", states[0])
	}
	if states[1].Nudges != 1 {
		t.Fatalf("second consult should see Nudges=1: %+v", states[1])
	}
	// The nudge must appear in the second provider request as a user message.
	if len(faux.Recorded) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(faux.Recorded))
	}
	var seen bool
	for _, m := range faux.Recorded[1].Messages {
		if m.Role == RoleUser && strings.Contains(m.Content, "NUDGE: verify before finishing") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("nudge not present in reopened request: %+v", faux.Recorded[1].Messages)
	}
}

// TestFinishGuardCapped verifies an always-rejecting guard is cut off at
// maxFinishNudges so the run still terminates with the model's answer.
func TestFinishGuardCapped(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("try 1"),
		AssistantText("try 2"),
		AssistantText("try 3"),
	)
	consults := 0
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		FinishGuard: func(context.Context, FinishState) string {
			consults++
			return "never satisfied"
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if consults != maxFinishNudges {
		t.Fatalf("guard consulted %d times, want %d (cap)", consults, maxFinishNudges)
	}
	if res.Final != "try 3" {
		t.Fatalf("final = %q, want the answer after the capped nudges", res.Final)
	}
	if res.StopReason != "stop" {
		t.Fatalf("stop reason = %q, want normal stop", res.StopReason)
	}
}

// TestFinishGuardSkippedOnBudgetWrapUp verifies the guard never fires on a
// budget-exhausted wrap-up turn: the ceiling is hit, so nothing may re-open the
// run.
func TestFinishGuardSkippedOnBudgetWrapUp(t *testing.T) {
	faux := NewFauxProvider(AssistantText("wrap-up summary"))
	consulted := false
	agent, err := New(Config{
		Provider:    faux,
		Model:       "test",
		BudgetGate:  func(context.Context, Usage) bool { return true },
		FinishGuard: func(context.Context, FinishState) string { consulted = true; return "reopen" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "budget_exhausted" {
		t.Fatalf("stop reason = %q, want budget_exhausted", res.StopReason)
	}
	if consulted {
		t.Fatal("finish guard must not be consulted on a budget wrap-up turn")
	}
}

// TestFinishGuardPanicAcceptsFinish verifies a panicking guard is hardened: the
// finish is accepted rather than crashing the run goroutine.
func TestFinishGuardPanicAcceptsFinish(t *testing.T) {
	faux := NewFauxProvider(AssistantText("answer"))
	agent, err := New(Config{
		Provider:    faux,
		Model:       "test",
		FinishGuard: func(context.Context, FinishState) string { panic("reckless guard") },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "answer" {
		t.Fatalf("final = %q, want the model's answer despite guard panic", res.Final)
	}
}

// TestFinishGuardNudgePersistedDurably verifies the injected nudge lands in the
// durable session log as a message entry, so a resume replays the same
// conversation the model saw.
func TestFinishGuardNudgePersistedDurably(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("first"),
		AssistantText("second"),
	)
	store := newMemSessionStore()
	nudged := false
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "sess-guard",
		FinishGuard: func(_ context.Context, s FinishState) string {
			if nudged {
				return ""
			}
			nudged = true
			return "GUARD-NUDGE"
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	entries, err := store.Log(context.Background(), "sess-guard")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Kind == EntryMessage && e.Message != nil && e.Message.Role == RoleUser && e.Message.Content == "GUARD-NUDGE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("guard nudge not persisted to the durable log (%d entries)", len(entries))
	}
}
