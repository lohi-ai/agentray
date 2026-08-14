// Package goal installs the run-level completion contract: a goal-gated run may
// only stop when it says so.
//
// The condition is stated in the system prompt up front, and the model must end
// its final answer with STATUS: DONE once the goal is met, or STATUS: BLOCKED
// plus the reason when only the user can unblock it. A finish with neither
// sentinel re-opens the run with a keep-going nudge.
//
// Unlike the finishguard plugin (a capped verify nudge), the gate is UNCAPPED by
// design: it holds the line until the model declares done or blocked. It cannot
// wedge a run — MaxTurns, MaxToolCalls, and the budget gate still bound the
// loop, a budget wrap-up never consults a stop interceptor at all, and the stall
// breaker below stops a model that has nothing new to say.
//
// The goal STRING is durable state the loop owns: it writes the EntryGoal record
// and recovers it on resume, then hands it to this plugin as RunInfo.Goal. So a
// crashed run comes back gated even when the resuming caller could not re-supply
// the condition, without this package ever writing to the log.
//
// # Composition order
//
// The loop consults stop interceptors in registration order and the first one to
// continue wins, so this plugin must be registered BEFORE finishguard: an unmet
// goal makes any verify pass on that answer moot.
package goal

import (
	"context"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// Done and Blocked are the completion sentinels the gate looks for in the final
// answer's closing line (case-insensitive, so "Status: done" also counts).
const (
	Done    = "STATUS: DONE"
	Blocked = "STATUS: BLOCKED"
)

// StopReasonStalled is recorded when the stall breaker accepts a finish because
// the model repeated its previous answer verbatim after a nudge.
const StopReasonStalled = "goal_stalled"

// Plugin installs the goal gate. An empty Goal leaves the run ungated, so a
// composition can wire the plugin unconditionally and let configuration decide.
type Plugin struct {
	Goal string
}

// Until gates the run on a condition.
func Until(condition string) Plugin { return Plugin{Goal: condition} }

// Name identifies the plugin and the extension it installs.
func (Plugin) Name() string { return "goal" }

// Register claims the goal seam (the loop persists the condition) and adds the
// gate itself as a run extension. Both halves are one call because a recorded
// goal nobody enforces, or an enforced goal nobody recorded, is a bug either
// way — the first resumes ungated, the second gates a run whose condition is
// invisible to the log.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Goal != "" {
		if err := r.SetGoal(p.Goal); err != nil {
			return err
		}
	}
	// The extension is added even with no configured goal, because a RESUMED
	// run gets its condition from the durable log rather than from this field —
	// the caller that restarts a crashed run usually cannot re-supply it. An
	// extension installed for a run that turns out to be ungated declines at
	// BeginRun and costs nothing; one that was never installed cannot gate a
	// recovered goal at all, which is the case gating matters most.
	r.AddExtension(p)
	return nil
}

// BeginRun arms the gate for this run. It declines an ungated run — the run has
// no condition to hold it to, so there is nothing to consult and no contract to
// put in the prompt.
//
// The condition comes from RunInfo, not from p.Goal: on a resume the loop
// recovers it from the durable log, and a plugin that trusted its own field
// would come back ungated exactly when gating matters most.
func (p Plugin) BeginRun(_ context.Context, info agentcore.RunInfo) (agentcore.Extension, error) {
	if info.Goal == "" {
		return nil, nil
	}
	return &gateRun{goal: info.Goal}, nil
}

// gateRun is one run's gate state. lastFinal is why this is a factory rather
// than a bare function: the stall breaker compares against the previous nudged
// answer, which is per-run.
type gateRun struct {
	goal      string
	nudges    int
	lastFinal string
}

// Name identifies the extension in composition diagnostics.
func (*gateRun) Name() string { return "goal" }

// SystemPrompt states the completion protocol before the model's first turn.
// Stating it once as a fixed prefix costs less than repeating it per turn, and
// it is in front of the model before the first answer rather than after it.
func (g *gateRun) SystemPrompt() string {
	return "## Goal gate\nThis run has a completion goal:\n" + strings.TrimSpace(g.goal) +
		"\nDo not stop until it is satisfied. End your final answer with the line \"" + Done +
		"\" once the goal is met, or \"" + Blocked +
		"\" plus the reason if only the user can unblock you. A finish without either line re-opens the run."
}

// TurnStopping holds the run to its goal.
//
// The stall breaker fires first: after at least one nudge, a finish that repeats
// the previous answer verbatim means the model has nothing further to say, so
// the run stops as goal_stalled instead of burning turns until MaxTurns.
func (g *gateRun) TurnStopping(_ context.Context, info agentcore.StopInfo) agentcore.StopDecision {
	if satisfied(info.Final) {
		return agentcore.StopDecision{}
	}
	if g.nudges > 0 && info.Final == g.lastFinal {
		return agentcore.StopDecision{StopReason: StopReasonStalled}
	}
	g.nudges++
	g.lastFinal = info.Final
	return agentcore.StopDecision{
		Continue: true,
		Inject:   []agentcore.Message{{Role: agentcore.RoleUser, Content: g.nudge()}},
		Note:     "Goal not met — continuing.",
	}
}

// nudge is the synthetic user message injected when the model tries to finish
// without declaring the goal met or blocked.
func (g *gateRun) nudge() string {
	return "[goal gate] The run's goal is not met yet:\n" + strings.TrimSpace(g.goal) +
		"\nKeep working toward it. When it is fully satisfied, end your answer with the line \"" + Done +
		"\". If you are genuinely blocked by something only the user can resolve, explain the blocker and end with \"" + Blocked + "\"."
}

// satisfied reports whether the final answer declares the goal met or the run
// honestly blocked. Only the LAST non-empty line is checked — the contract says
// "end your final answer with the line", and an unanchored match would let an
// answer that merely mentions the sentinel mid-prose ("I cannot yet write
// STATUS: DONE — the tests still fail") falsely close the gate. The line match
// is a contains, not a prefix, so markdown decoration around the sentinel
// ("**STATUS: DONE**") still counts.
func satisfied(final string) bool {
	lines := strings.Split(final, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		up := strings.ToUpper(line)
		return strings.Contains(up, Done) || strings.Contains(up, Blocked)
	}
	return false
}
