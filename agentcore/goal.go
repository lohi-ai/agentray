package agentcore

// goal.go — the run's goal as durable STATE.
//
// The loop owns the goal only as a fact about the run: it writes the condition
// to the durable log once, and recovers it when a crashed run resumes. What to
// DO about an unmet goal — the completion protocol, the sentinel, the nudge, the
// stall breaker — is policy, and policy lives in agentcore/plugins/goal as a
// StopInterceptor. That split is deepseek-harness's: dsh-goal is an
// event-sourced service, and continuation is a separate consumer package.
//
// The state stays here for one reason: only the loop may write the durable log.
// A plugin that persisted its own gate condition would be a second writer to the
// record resume depends on, and "model-visible means logged" would stop being a
// property the core can guarantee.

// goalFromLog recovers the goal-gate condition from a durable log, so a crashed
// goal-gated run comes back gated even when the resuming caller cannot re-supply
// the condition.
//
// The fold matches RecoverSession's: the last EntryGoal wins, and EntryLeaf
// clears it — the goal belongs to the run that finished, so a later run chained
// onto the same log (a chat continuation) must not inherit its gate.
func goalFromLog(entries []SessionEntry) string {
	goal := ""
	for _, e := range entries {
		switch e.Kind {
		case EntryGoal:
			goal = e.Goal
		case EntryLeaf:
			goal = ""
		}
	}
	return goal
}
