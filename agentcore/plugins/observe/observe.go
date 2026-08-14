// Package observe installs monitoring and observation: the watchers that report
// on a run without changing it.
//
// Three kinds live here, and the split is deliberate:
//
//   - Hooks (this file) observe the run's OUTWARD events — turns, provider
//     calls, executed tool calls, the finished run. They register at
//     agentcore.PriorityLate so they see the FINAL decision, after every gate
//     and rewrite has had its say. An observer that ran first would faithfully
//     report calls that were subsequently blocked, which is worse than no
//     telemetry because it looks correct.
//   - LogInvariant (loginvariant.go) observes the run's INWARD consistency:
//     whether everything the model was shown actually reached the durable log.
//   - Monitor (monitor.go) observes the PROVIDER seam: one priced TraceRecord
//     per LLM call, via a decorator (Registry.WrapProvider) rather than a hook,
//     because accounting has to bracket the call to time it and must see the
//     calls that failed.
package observe

import "github.com/lohi-ai/agentray/agentcore"

// Hooks installs the run's watchers. Every field is optional; the zero value
// installs nothing, so a composition can wire the plugin unconditionally and
// let configuration decide what is actually observed.
type Hooks struct {
	// Label distinguishes several observers in one composition.
	Label string
	// OnTurnStart / OnTurnEnd observe turn boundaries on every run, streamed or
	// not — so metering and audit never depend on a viewer being attached.
	OnTurnStart []agentcore.TurnHook
	OnTurnEnd   []agentcore.TurnHook
	// OnMessageEnd observes each completed assistant message.
	OnMessageEnd []agentcore.MessageEndHook
	// OnProviderResponse meters each raw provider call — the right place to hang
	// token and latency accounting.
	OnProviderResponse []agentcore.ProviderResponseHook
	// OnToolCall observes each executed tool call and its result. It runs after
	// the gate, so it never sees a blocked call as executed.
	OnToolCall []agentcore.AfterToolCall
	// OnAgentEnd observes the finished run on every exit path (final answer,
	// budget stop, abort, max turns, error).
	OnAgentEnd []agentcore.AgentEndHook
	// OnHookError receives attributed hook failures, including this plugin's.
	OnHookError func(source string, err error)
}

// Name identifies the plugin.
func (p Hooks) Name() string {
	if p.Label != "" {
		return "observe:" + p.Label
	}
	return "observe"
}

// Register adds the observers at PriorityLate.
func (p Hooks) Register(r *agentcore.Registry) error {
	r.AddHooks(agentcore.PriorityLate, agentcore.Hooks{
		TurnStart:             p.OnTurnStart,
		TurnEnd:               p.OnTurnEnd,
		MessageEnd:            p.OnMessageEnd,
		AfterProviderResponse: p.OnProviderResponse,
		After:                 p.OnToolCall,
		AgentEnd:              p.OnAgentEnd,
		OnError:               p.OnHookError,
	})
	return nil
}
