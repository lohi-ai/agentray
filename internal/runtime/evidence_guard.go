package agentruntime

import (
	"context"
	"regexp"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/finishguard"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/sandbox"
)

// evidence_guard.go — the analytics analog of hermes-agent's verify-on-stop
// (agent/verification_stop.py): policy only, never runs a check itself. Where
// hermes gates "edited code without fresh verification", an analytics agent's
// equivalent failure is a FIGURE-SHAPED ANSWER produced without touching the
// data: the model quotes counts, revenue, percentages from nowhere. When that
// happens the guard re-opens the run once with a bounded nudge — verify with a
// granted read tool, or say explicitly that the figures are not from live
// project data. The honest-blocker escape hatch is deliberate: the goal is
// evidence or honesty, never a forced tool call.

// evidenceToolSet is the set of tools whose execution counts as having
// consulted project data: the read tools classified beside scopeTools in
// policy.go (write/side-effect tools deliberately do not count — producing an
// artifact is not reading evidence), plus spawn_subagent, because a delegated
// child runs the read tools itself and only its answer returns to the parent's
// trace, plus web_fetch.
//
// web_fetch is here because "evidence" is not a synonym for "the event store".
// A pre-product agent's whole evidence base is pages it fetched this turn — a
// competitor's real pricing page is a checked source in exactly the sense this
// guard cares about, and treating it as nothing forces the honest answer
// ("their plan is 99k VND/month, fetched from this URL") into the same bucket
// as an invented one.
var evidenceToolSet = func() map[string]bool {
	s := map[string]bool{
		subagent.ToolSpawnSubagent: true,
		sandbox.ToolWebFetch:       true,
	}
	for name := range readTools {
		s[name] = true
	}
	return s
}()

// evidenceNudge is the synthetic follow-up injected on an unbacked numeric
// answer. Mirrors hermes' nudge contract: name the failure, give the repair
// path, and allow an honest disclaimer instead of a forced call. It must not
// claim the figures are fabricated — evidence gathered in an earlier turn of
// the conversation (or before a crash-resume) is invisible to this run's
// trace, so citing that provenance is a legitimate way out.
//
// The last sentence is load-bearing and was learned the hard way. The guard
// injects this as a USER message, so whatever the model says next is what the
// owner reads — there is no back channel. Without an explicit instruction to
// re-emit the answer, a model that judges itself already compliant replies with
// its verdict on the instruction ("I stated no figures, no correction needed"),
// and that verdict silently REPLACES the answer: the owner asks a question and
// gets back a self-audit referring to a "previous reply" they never saw. The
// repair path must always terminate in the full answer, including the case
// where the repair turns out to be nothing.
const evidenceNudge = "[System: your answer states figures, but no data tool was executed in this run. " +
	"If the figures come from data-tool results earlier in this conversation, keep them and briefly " +
	"note that provenance. Otherwise either verify them now with a granted read tool (run_sql, " +
	"explore_events, activity_summary, ...) and correct the answer, or restate the answer saying " +
	"explicitly that the figures are estimates not read from project data. " +
	"This note is not visible to the user and is not a question to answer: your next message is what " +
	"the user reads, so reply with the COMPLETE answer they should see, never a comment about this " +
	"note. If your answer needs no change, send it again unchanged.]"

// runToolNames lifts the run's built tools to their names for the guard's
// arming check.
func runToolNames(tools []agentcore.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

// evidenceFinishGuard returns a FinishGuard for an agent granted at least one
// evidence tool. It nudges at most once per run, and only when the final answer
// carries digits while zero evidence tools executed. Agents with no evidence
// tools get a nil guard — a persona-only chat agent has no data to consult and
// must not be nagged.
//
// runTools is the run's resolved tool names, so a capability granted OUTSIDE
// the scope map counts. Scopes alone are not the whole grant: a pack may name
// non-scope tools (Pack.Tools, e.g. web_fetch), and an agent whose only
// evidence source is the open web must be held to the same rail as one reading
// the event store — and, more to the point, must be CREDITED for using it.
func evidenceFinishGuard(s Scopes, runTools []string) finishguard.Guard {
	granted := false
	for _, name := range append(ScopeToolNames(s), runTools...) {
		if evidenceToolSet[name] {
			granted = true
			break
		}
	}
	if !granted {
		return nil
	}
	return func(_ context.Context, st finishguard.State) string {
		if st.Nudges > 0 {
			// One nudge only: the model has had its bounded repair pass; a second
			// rejection would just burn turns against a model that already chose
			// its answer (hermes caps at 2, we are stricter — the disclaimer
			// escape hatch makes one pass sufficient).
			return ""
		}
		for _, tr := range st.Tools {
			if tr.Allowed && tr.Error == "" && evidenceToolSet[tr.Tool] {
				return ""
			}
		}
		if !containsFigures(st.Final) {
			return ""
		}
		return evidenceNudge
	}
}

// nonFigureDigits matches digit runs that are not quantitative claims —
// markdown list numbering ("1. Open the tab") and calendar dates ("2026-07-01",
// "2026-07") — which are stripped before the digit scan so a step-by-step or
// date-bearing answer is not treated as fabricated figures.
var nonFigureDigits = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^\s*\d+[.)]\s`),
	regexp.MustCompile(`\b\d{4}-\d{2}(-\d{2})?\b`),
}

// containsFigures reports whether the answer states quantities: an ASCII digit
// outside the non-figure patterns above, or a spelled percent. Prose-only
// answers ("usage is trending up") pass unguarded — the rail targets
// fabricated concrete figures, and a digit is the cheapest reliable marker of
// one.
func containsFigures(answer string) bool {
	cleaned := answer
	for _, re := range nonFigureDigits {
		cleaned = re.ReplaceAllString(cleaned, "")
	}
	if strings.ContainsAny(cleaned, "0123456789") {
		return true
	}
	return strings.Contains(strings.ToLower(answer), " percent")
}
