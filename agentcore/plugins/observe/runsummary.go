package observe

import (
	"sort"

	"github.com/lohi-ai/agentray/agentcore"
)

// Run-level rollup of facts agentcore has already finished producing.
//
// Adopted from omp's packages/agent/src/run-collector.ts
// (`AgentRunSummary` / `AgentRunCoverage`, and their `aggregate*` folds), whose
// header states the design intent this file keeps: "Pure aggregation — no
// references to spans, no callbacks, no live state. Safe to persist / diff /
// assert."
//
// Two defects close here, and both are agentray's, not omp's:
//
//   - A BLOCKED tool call was counted nowhere. Every existing rollup is fed by
//     the After hook, which runs post-gate and therefore structurally cannot
//     see a denial (README, "No hook for a *blocked* tool call"). This folds
//     agentcore.RunResult.Tools instead, where ToolTrace.Allowed/Reason have
//     carried the denial all along — so the fix costs nothing but the fold.
//   - Tool COVERAGE was computed nowhere. The advertised set (TraceRecord.Tools)
//     and the invoked set (RunResult.Tools[].Tool) both exist and were both
//     discarded. "This persona advertises 11 tools and used 3" is a prompt-token
//     bill every turn, and under docs/AGENT-GOVERNANCE.md § "Extending
//     capabilities" it is a governance signal about a preset's scope list.
//
// This file registers nothing, persists nothing, and touches no run state. It
// is a pure function of two values a run has already returned, mirroring the
// precedent of LogInvariant: a detector that reports rather than acts. That
// also means the value is DERIVED AND DISCARDED — deliberately not a second
// source of truth. If it is ever wanted durably, that is a separate governance
// call and it should be a materialized view over agent_llm_calls, not a new
// write on the run path.
//
// It deliberately carries NO token or cost totals. agent_runs already sums
// those in Postgres (internal/dataplane/store/agent_runtime.go), and the README
// documents pushing them there as a decision, not an omission.

// ToolStatus is the closed vocabulary of tool outcomes (omp's ToolStatus,
// narrowed to the four states agentcore can actually distinguish — it has no
// separate "skipped" or "timeout" trace, a timeout surfacing as an ordinary
// error or as the aborted reason).
type ToolStatus string

const (
	// ToolOK is a call the gate allowed and the tool completed without error.
	ToolOK ToolStatus = "ok"
	// ToolError is a call that executed and failed — including "unknown tool",
	// which the dispatcher marks allowed so the model gets a real error back.
	ToolError ToolStatus = "error"
	// ToolBlocked is a call that never executed: refused by the permission gate
	// or a Before hook, by argument validation, by credential resolution, by the
	// per-run circuit breaker, or by the tool-call budget.
	ToolBlocked ToolStatus = "blocked"
	// ToolAborted is a call dropped because the run's context was cancelled
	// before its dispatch group started.
	ToolAborted ToolStatus = "aborted"
)

// ClassifyTool maps one persisted tool trace onto the closed vocabulary. It is
// exported because the mapping is the interesting part: the loop stamps a
// denial as Allowed=false plus a reason string, and every caller that wants to
// count denials must agree on how those reasons bucket.
func ClassifyTool(t agentcore.ToolTrace) ToolStatus {
	if !t.Allowed {
		// ToolDenialAborted is the loop's cancel-denial (agentcore/loop.go);
		// everything else that failed closed is a refusal. Routed through
		// DeniedAborted so a typo of the wire literal cannot silently
		// recategorize historical rows.
		if t.DeniedAborted() {
			return ToolAborted
		}
		return ToolBlocked
	}
	if t.Error != "" {
		return ToolError
	}
	return ToolOK
}

// ToolCounters is one tool's (or the whole run's) outcome histogram. Total is
// every call the MODEL ASKED FOR, executed or not — omp's recordOrphanTool
// insistence, which is the whole point: a denial the model kept retrying is
// invisible if only executions are counted.
type ToolCounters struct {
	Total          int   `json:"total"`
	OK             int   `json:"ok"`
	Error          int   `json:"error,omitempty"`
	Blocked        int   `json:"blocked,omitempty"`
	Aborted        int   `json:"aborted,omitempty"`
	TotalLatencyMS int64 `json:"total_latency_ms,omitempty"`
}

// add folds one classified trace into the counters.
func (c *ToolCounters) add(t agentcore.ToolTrace) {
	c.Total++
	c.TotalLatencyMS += t.LatencyMS
	switch ClassifyTool(t) {
	case ToolOK:
		c.OK++
	case ToolError:
		c.Error++
	case ToolBlocked:
		c.Blocked++
	case ToolAborted:
		c.Aborted++
	}
}

// merge folds another counter set into this one (the aggregate path).
func (c *ToolCounters) merge(o ToolCounters) {
	c.Total += o.Total
	c.OK += o.OK
	c.Error += o.Error
	c.Blocked += o.Blocked
	c.Aborted += o.Aborted
	c.TotalLatencyMS += o.TotalLatencyMS
}

// RunSummary is a stable, sorted, diffable rollup of one run — or, after
// AggregateRunSummaries, of N runs in the SAME shape, so a consumer that renders
// one renders a hundred unchanged.
//
// It deliberately carries no token or cost totals: agent_runs owns those (see
// the README's "No aggregation" note, which this file amends rather than
// contradicts — per-call to per-run folding now exists in process, per-agent
// totals remain Postgres's job).
type RunSummary struct {
	// Runs is how many runs this value folds — 1 straight out of FoldRun. It
	// exists so an aggregate stays honest about its denominator.
	Runs                int            `json:"runs"`
	Turns               int            `json:"turns"`
	ProviderCalls       int            `json:"provider_calls"`
	FailedProviderCalls int            `json:"failed_provider_calls,omitempty"`
	ByStopReason        map[string]int `json:"by_stop_reason,omitempty"`
	// Tools is the run-wide histogram; ByTool is the same split per tool name.
	Tools  ToolCounters            `json:"tools"`
	ByTool map[string]ToolCounters `json:"by_tool,omitempty"`
	// Ghost marks this run as infrastructure noise rather than a model result
	// (see IsGhostRun). On an aggregate it means EVERY folded run was a ghost;
	// GhostRuns carries the count, so a score denominator is Runs - GhostRuns.
	Ghost     bool `json:"ghost_run,omitempty"`
	GhostRuns int  `json:"ghost_runs,omitempty"`
}

// RunCoverage is the prompt-surface audit: which tools this run PAID to
// advertise, which it actually called, and which it never touched.
//
// Every slice is sorted and deduped so the value is stable for diffing.
//
// Scope caveat, and it is the easy thing to get wrong: coverage is a
// PER-INVOCATION claim, not a per-session one. On a resumed run the trace
// records start at the resume point, so ToolsUnused describes only that
// invocation — a reader who conflates the two will call a tool dead when the
// pre-crash span used it heavily. AggregateRunCoverage is the supported way to
// span invocations.
type RunCoverage struct {
	// ToolsAvailable is the union of names advertised on any provider call.
	ToolsAvailable []string `json:"tools_available,omitempty"`
	// ToolsInvoked is the union of names the model actually asked for, allowed
	// or not. A name here that is absent from ToolsAvailable is the asymmetric
	// case worth catching: the model called a tool that was never offered.
	ToolsInvoked []string `json:"tools_invoked,omitempty"`
	// ToolsUnused is ToolsAvailable minus ToolsInvoked — schema tokens spent
	// every turn for nothing.
	ToolsUnused   []string `json:"tools_unused,omitempty"`
	ModelsUsed    []string `json:"models_used,omitempty"`
	ProvidersUsed []string `json:"providers_used,omitempty"`
}

// FoldRun computes both values from facts agentcore already produced: res is
// what AgentEndHook receives, records is the Monitor's per-call stream.
//
// It filters records to delegation depth 0. A parent and its spawned children
// share one provider and one Sink (see TraceRecord.SessionKey/Depth), so
// without the filter a subagent's advertisements land in the parent's
// ToolsAvailable and its untouched ones in the parent's ToolsUnused — which
// would produce confidently wrong governance advice. omp has no delegation
// depth and therefore no guidance here.
//
// It runs at or after run end and is never on the request path.
func FoldRun(res agentcore.RunResult, records []TraceRecord) (RunSummary, RunCoverage) {
	s := RunSummary{Runs: 1, Turns: res.Turns, ByStopReason: map[string]int{}, ByTool: map[string]ToolCounters{}}

	available := map[string]bool{}
	models := map[string]bool{}
	providers := map[string]bool{}
	for _, r := range records {
		if r.Depth != 0 {
			continue
		}
		s.ProviderCalls++
		if r.Err != "" {
			s.FailedProviderCalls++
		}
		if r.StopReason != "" {
			s.ByStopReason[r.StopReason]++
		}
		for _, name := range r.Tools {
			available[name] = true
		}
		if r.Model != "" {
			models[r.Model] = true
		}
		if r.Provider != "" {
			providers[r.Provider] = true
		}
	}

	invoked := map[string]bool{}
	for _, t := range res.Tools {
		s.Tools.add(t)
		c := s.ByTool[t.Tool]
		c.add(t)
		s.ByTool[t.Tool] = c
		invoked[t.Tool] = true
	}

	s.Ghost = IsGhostRun(s, res.Usage)
	if s.Ghost {
		s.GhostRuns = 1
	}

	cov := RunCoverage{
		ToolsAvailable: sortedKeys(available),
		ToolsInvoked:   sortedKeys(invoked),
		ToolsUnused:    sortedMissing(available, invoked),
		ModelsUsed:     sortedKeys(models),
		ProvidersUsed:  sortedKeys(providers),
	}
	return s, cov
}

// IsGhostRun reports whether a run is infrastructure noise rather than a model
// result: a failed provider call, and nothing to show for the run — no tool the
// model asked for, and not one billable token.
//
// Adopted from omp's isGhostRun/isTransportFailure (adapters/edit/runner.ts),
// whose comment is the reason to have it: "These don't reflect edit-tool
// quality, so we exclude them from the score denominator." A flaky provider
// must not be scored as a bad agent. bench/results/game-2026-08-14-run2 is a
// live instance in this repo — one failed provider call, otherwise identical to
// the clean run beside it, and today nothing reads the difference.
//
// Usage is passed in rather than carried on RunSummary precisely because this
// file does not aggregate tokens; the caller supplies whichever total it trusts
// (RunResult.Usage live, agent_runs after the fact).
func IsGhostRun(s RunSummary, u agentcore.Usage) bool {
	billable := u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
	return s.FailedProviderCalls > 0 && s.Tools.Total == 0 && billable == 0
}

// AggregateRunSummaries folds N run summaries into one of the SAME shape, so a
// caller driving the loop N times (a verify pass, a bench harness repeating a
// task) has somewhere to put the repetitions. Same shape in, same shape out is
// omp's stated reason for exporting its aggregate, and it is why dashboards and
// artifacts need no second code path.
func AggregateRunSummaries(summaries ...RunSummary) RunSummary {
	out := RunSummary{ByStopReason: map[string]int{}, ByTool: map[string]ToolCounters{}}
	for _, s := range summaries {
		out.Runs += s.Runs
		out.Turns += s.Turns
		out.ProviderCalls += s.ProviderCalls
		out.FailedProviderCalls += s.FailedProviderCalls
		out.GhostRuns += s.GhostRuns
		out.Tools.merge(s.Tools)
		for reason, n := range s.ByStopReason {
			out.ByStopReason[reason] += n
		}
		for name, c := range s.ByTool {
			merged := out.ByTool[name]
			merged.merge(c)
			out.ByTool[name] = merged
		}
	}
	// A rollup is "a ghost" only when there is nothing real in it at all;
	// otherwise Runs - GhostRuns is the denominator to score against.
	out.Ghost = out.Runs > 0 && out.GhostRuns == out.Runs
	return out
}

// AggregateRunCoverage folds N coverages into one, again in the same shape.
//
// ToolsUnused is recomputed from the merged sets rather than unioned: a tool
// unused in run 1 and called in run 2 is NOT unused across the pair, and
// unioning the per-run answers would claim it was.
func AggregateRunCoverage(coverages ...RunCoverage) RunCoverage {
	available := map[string]bool{}
	invoked := map[string]bool{}
	models := map[string]bool{}
	providers := map[string]bool{}
	for _, c := range coverages {
		addAll(available, c.ToolsAvailable)
		addAll(invoked, c.ToolsInvoked)
		addAll(models, c.ModelsUsed)
		addAll(providers, c.ProvidersUsed)
	}
	return RunCoverage{
		ToolsAvailable: sortedKeys(available),
		ToolsInvoked:   sortedKeys(invoked),
		ToolsUnused:    sortedMissing(available, invoked),
		ModelsUsed:     sortedKeys(models),
		ProvidersUsed:  sortedKeys(providers),
	}
}

func addAll(set map[string]bool, names []string) {
	for _, n := range names {
		set[n] = true
	}
}

// sortedKeys returns the set's members sorted, or nil when empty (so an absent
// dimension marshals away instead of rendering as []).
func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedMissing returns the members of have that are absent from used.
func sortedMissing(have, used map[string]bool) []string {
	var out []string
	for k := range have {
		if !used[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
