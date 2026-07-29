package agentruntime

import (
	"context"
	"regexp"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
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
// trace.
var evidenceToolSet = func() map[string]bool {
	s := map[string]bool{agentcore.ToolSpawnSubagent: true}
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
const evidenceNudge = "[System: your answer states figures, but no data tool was executed in this run. " +
	"If the figures come from data-tool results earlier in this conversation, keep them and briefly " +
	"note that provenance. Otherwise either verify them now with a granted read tool (run_sql, " +
	"explore_events, activity_summary, ...) and correct the answer, or restate the answer saying " +
	"explicitly that the figures are estimates not read from project data.]"

// evidenceFinishGuard returns a FinishGuard for an agent whose enabled scopes
// grant at least one evidence (read) tool. It nudges at most once per run, and
// only when the final answer carries digits while zero evidence tools executed.
// Agents with no evidence tools get a nil guard — a persona-only chat agent
// has no data to consult and must not be nagged.
func evidenceFinishGuard(s Scopes) agentcore.FinishGuard {
	granted := false
	for _, name := range ScopeToolNames(s) {
		if evidenceToolSet[name] {
			granted = true
			break
		}
	}
	if !granted {
		return nil
	}
	return func(_ context.Context, st agentcore.FinishState) string {
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
