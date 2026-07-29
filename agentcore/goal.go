package agentcore

import "strings"

// goal.go — run-level goal gate (Claude Code /goal, codex goal-mode analog).
// A goal declares the condition under which the run may stop. The contract is
// injected into the system prompt up front: the model must end its final
// answer with a STATUS: DONE line once the goal is satisfied, or STATUS:
// BLOCKED with the reason when only the user can unblock it. A normal finish
// without either sentinel is not accepted — the loop injects a synthetic
// "keep going" user message and continues.
//
// Unlike FinishGuard (a capped verify nudge), the gate is uncapped by design:
// it holds the line until the model declares done or blocked. It cannot wedge
// a run — MaxTurns, MaxToolCalls, and the budget gate all still bound the
// loop, a budget wrap-up bypasses the gate entirely, and a stall breaker
// accepts the finish when the model repeats the same answer verbatim after a
// nudge (StopReason "goal_stalled").

// GoalDone / GoalBlocked are the completion sentinels the gate looks for in
// the final answer's closing line (case-insensitive, so "Status: done" also
// counts).
const (
	GoalDone    = "STATUS: DONE"
	GoalBlocked = "STATUS: BLOCKED"
)

// goalSatisfied reports whether the final answer declares the goal met or the
// run honestly blocked. Only the LAST non-empty line is checked — the contract
// says "end your final answer with the line", and an unanchored match would let
// an answer that merely mentions the sentinel mid-prose ("I cannot yet write
// STATUS: DONE — the tests still fail") falsely close the gate. The line match
// is a contains, not a prefix, so markdown decoration around the sentinel
// ("**STATUS: DONE**") still counts.
func goalSatisfied(final string) bool {
	lines := strings.Split(final, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		up := strings.ToUpper(line)
		return strings.Contains(up, GoalDone) || strings.Contains(up, GoalBlocked)
	}
	return false
}

// goalContract is appended to the system prompt when a goal is set, so the
// model knows the completion protocol before its first turn.
func goalContract(goal string) string {
	return "\n\n## Goal gate\nThis run has a completion goal:\n" + strings.TrimSpace(goal) +
		"\nDo not stop until it is satisfied. End your final answer with the line \"" + GoalDone +
		"\" once the goal is met, or \"" + GoalBlocked +
		"\" plus the reason if only the user can unblock you. A finish without either line re-opens the run."
}

// goalNudge is the synthetic user message injected when the model tries to
// finish without declaring the goal met or blocked.
func goalNudge(goal string) string {
	return "[goal gate] The run's goal is not met yet:\n" + strings.TrimSpace(goal) +
		"\nKeep working toward it. When it is fully satisfied, end your answer with the line \"" + GoalDone +
		"\". If you are genuinely blocked by something only the user can resolve, explain the blocker and end with \"" + GoalBlocked + "\"."
}
