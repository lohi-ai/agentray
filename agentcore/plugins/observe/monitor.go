package observe

import "github.com/lohi-ai/agentray/agentcore"

// Monitor traces and prices every model call the composition can make — the
// primary rung, every escalation rung, and the compaction rung. A run that
// escalates does not silently stop being accounted for.
//
// It is the accounting layer: one TraceRecord per LLM call carrying what was
// sent, what came back, how long it took, and what it cost. Cost is computed
// from a Pricing table and stamped onto the response Usage, so
// Result.Usage.CostUSD is honest whether or not anything is listening.
//
// It is a decorator, not a hook, and deliberately so. A hook fires either
// before the request or after the response; monitoring has to BRACKET the call
// to measure duration, and has to see the calls that FAILED — neither of which
// a one-sided hook can do. agentcore therefore exposes the bracket as a
// contribution (Registry.WrapProvider) and this type is its first consumer.
type Monitor struct {
	// Pricing is the model price table. Nil uses DefaultPricing.
	Pricing Pricing
	// Sink receives one record per call. Nil still prices the call (so
	// Usage.CostUSD is filled) and drops the record.
	Sink Sink
}

// Name identifies the plugin.
func (Monitor) Name() string { return "monitor" }

// Register contributes the provider decorator.
func (p Monitor) Register(r *agentcore.Registry) error {
	pricing := p.Pricing
	if pricing == nil {
		pricing = DefaultPricing()
	}
	r.WrapProvider(func(inner agentcore.LLMProvider) agentcore.LLMProvider {
		return newTracingProvider(inner, pricing, p.Sink)
	})
	return nil
}

// Wrap is the decorator on its own, for a caller that builds providers by hand
// rather than through a composition — a connectivity probe, a one-shot
// classifier call, a provider handed to something that is not an Agent. It is
// the same implementation Monitor installs, so a call priced this way and a
// call priced through the registry produce identical records.
func Wrap(inner agentcore.LLMProvider, pricing Pricing, sink Sink) agentcore.LLMProvider {
	if inner == nil {
		return nil
	}
	if pricing == nil {
		pricing = DefaultPricing()
	}
	return newTracingProvider(inner, pricing, sink)
}
