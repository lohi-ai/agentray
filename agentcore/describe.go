package agentcore

import (
	"fmt"
	"sort"
	"strings"
)

// Describe renders what an Agent is actually configured with.
//
// The question it answers is the one that is hard to answer from a config file
// or a plugin list: after every default, every override, and every plugin, what
// does this agent actually do? Reach for it when a run behaves in a way the
// configuration does not explain — an ungated tool, compaction that never
// fires, a resume that starts fresh.
//
// It reports presence, never values, for anything that could carry a secret or
// a closure: a credential resolver, a budget gate, and a steering source each
// print as "set". Tool NAMES are printed because the model already sees them.
//
// The output is stable and ordered, so two agents can be compared by string —
// which is how agentcore/plugins/preset proves that composing from plugin
// packages produces the same agent as New(Config).
func (a *Agent) Describe() string {
	var b strings.Builder

	line := func(k string, v any) { fmt.Fprintf(&b, "%-22s %v\n", k+":", v) }
	// present renders a closure or interface as set/unset — never its value.
	present := func(k string, set bool) {
		if set {
			line(k, "set")
		} else {
			line(k, "-")
		}
	}

	driver := "-"
	if a.driver != nil {
		driver = a.driver.Name()
	}
	line("driver", driver)
	line("model", a.model)
	if n := len(a.escalation); n > 0 {
		rungs := make([]string, 0, n)
		for _, r := range a.escalation {
			rungs = append(rungs, r.Model)
		}
		line("escalation", strings.Join(rungs, " → "))
	}
	line("max_tokens", a.maxTokens)
	line("reasoning_effort", orDash(a.reasoningEffort))
	present("output_schema", a.outputSchema != nil)
	line("prompt_cache", orDash(a.cacheKey))
	present("refresh_key", a.refreshKey != nil)
	line("retry", fmt.Sprintf("%d attempts", a.retry.MaxAttempts))

	// Identity and bounds.
	line("scope", orDash(a.def.ScopeID))
	line("skills", len(a.def.Skills))
	line("limits", fmt.Sprintf("turns=%d tools=%d ctx=%d result=%d",
		a.limits.MaxTurns, a.limits.MaxToolCalls, a.limits.MaxContextTokens, a.limits.MaxToolResultLen))
	present("sandbox", a.env.Sandbox != nil)

	// Governance. The policy TYPE is named rather than its rules: the rules are
	// the policy's business, and printing them here would drift.
	line("policy", fmt.Sprintf("%T", a.policy))
	line("goal", orDash(a.goal))
	present("budget_gate", a.budgetGate != nil)
	present("step_gate", a.stepGate != nil)

	// Durability and context.
	present("session", a.session != nil)
	line("session_resume", a.resumeSession)
	line("seed_disabled", strings.Join(sorted(a.seedDisabledTools), ", "))
	present("memory", a.memory != nil)
	// The compaction budget an operator configured is not the one that applies:
	// the answering model's window caps it. Print what will actually be used,
	// since a run that compacts unexpectedly early or late is diagnosed here.
	line("context_window", a.contextWindow)
	line("compaction", fmt.Sprintf("keep_recent=%d budget=%d",
		a.compaction.KeepRecentTokens, effectiveBudget(a.limits.MaxContextTokens, a.contextWindow)))
	line("compactor", compactorName(a.compactor))
	line("compaction_model", orDash(a.compactionModel))

	// Extensions — the capabilities this agent has beyond the core loop. Named
	// rather than described: what each one does is its own package's README, and
	// restating it here would drift. An empty list is a complete answer, not a
	// missing one.
	line("extensions", strings.Join(extensionNames(a.extensions), ", "))

	// Steering.
	present("steering", a.getSteering != nil)
	present("follow_up", a.getFollowUp != nil)
	present("prepare_next_turn", a.prepareNextTurn != nil)

	// Surface. Tool order is the order the model is shown, so it is NOT sorted.
	line("tools", strings.Join(a.tools.Names(), ", "))
	h := a.hooks
	line("hooks", fmt.Sprintf("before=%d after=%d context=%d turn_start=%d turn_end=%d message_end=%d provider=%d agent_end=%d",
		len(h.Before), len(h.After), len(h.Context), len(h.TurnStart), len(h.TurnEnd),
		len(h.MessageEnd), len(h.AfterProviderResponse), len(h.AgentEnd)))
	line("hook_error_policy", string(h.ErrorPolicy))

	return b.String()
}

// orDash renders an empty string as a dash so a missing value is visibly
// missing rather than an empty column.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sorted returns a sorted copy, so a set-shaped field describes identically
// regardless of the order it was configured in.
func sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// extensionNames lists the registered factories in composition order, which is
// also the order they intercept in.
func extensionNames(fs []ExtensionFactory) []string {
	if len(fs) == 0 {
		return nil
	}
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.Name())
	}
	return names
}

// compactorName names the installed compaction strategy, so "what is actually
// running?" distinguishes the built-in summarizer from a replacement.
func compactorName(c Compactor) string {
	if c == nil {
		return DefaultCompactor().Name()
	}
	return c.Name()
}
