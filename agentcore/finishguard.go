package agentcore

import "context"

// finishguard.go — verify-on-stop (hermes-agent's turn-end verification guard,
// generalized). The guard never runs checks itself; it turns passive run
// evidence — the tool trace, the answer text — into one bounded synthetic
// follow-up at the exact moment the model tries to finish. The loop consults it
// only on a NORMAL finish (the model produced a text answer with no tool
// calls); a budget wrap-up, tool-budget stop, MaxTurns stop, abort, or a
// terminal tool never re-opens the run.

// maxFinishNudges caps guard injections per run so a guard that is never
// satisfied cannot loop the run against MaxTurns (hermes max_attempts).
const maxFinishNudges = 2

// FinishState is the evidence snapshot handed to a FinishGuard: the answer the
// model just produced, the run shape so far, and how many nudges this guard has
// already spent. Tools aliases the run's live trace slice — read-only.
type FinishState struct {
	// Final is the assistant text the run would return.
	Final string
	// Turns is the number of reasoning turns consumed, including this one.
	Turns int
	// Tools is the run's tool trace so far (allowed and blocked calls).
	Tools []ToolTrace
	// Nudges counts guard messages already injected this run (0 on the first
	// consultation), so a guard can weaken or drop its demand on the retry.
	Nudges int
}

// FinishGuard is consulted when the model produces a final answer and the run
// would end normally. Returning a non-empty string injects it as a synthetic
// user message — persisted to the durable log like a steer — and the loop
// continues instead of returning, giving the model a bounded chance to verify
// or repair before finishing. Returning "" accepts the finish. Injections are
// capped at maxFinishNudges per run regardless of what the guard returns.
type FinishGuard func(ctx context.Context, state FinishState) string

// consultFinishGuard runs the guard panic-hardened: a panicking guard accepts
// the finish (the run ends normally) rather than taking the loop down.
func consultFinishGuard(ctx context.Context, guard FinishGuard, state FinishState) (nudge string) {
	if guard == nil {
		return ""
	}
	_ = safe(func() { nudge = guard(ctx, state) })
	return nudge
}
