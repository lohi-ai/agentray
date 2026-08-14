// Package budget installs the two governance rails that can pause or wrap up a
// run from outside it: the per-turn spend ceiling and the step gate.
package budget

import (
	"context"

	"github.com/lohi-ai/agentray/agentcore"
)

// Plugin installs the budget and step gates.
type Plugin struct {
	// Gate is consulted at the top of each turn with the run's accumulated
	// usage. Returning true triggers a graceful stop: the loop injects a final
	// "summarize and stop" turn, strips tools so the model can only write a
	// wrap-up, and ends with StopReason "budget_exhausted". The consumer owns
	// the ceiling and the spend lookup; agentcore only sees the verdict.
	Gate func(ctx context.Context, u agentcore.Usage) bool
	// Step is called at the top of every turn before any work happens and blocks
	// until the consumer permits the turn. A non-nil error halts the run. This
	// is how a step-through debugger pauses a live run without changing any
	// other run behavior.
	Step func(ctx context.Context, turn int) error
}

// Name identifies the plugin.
func (Plugin) Name() string { return "budget" }

// Register claims the gate seams it was given.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Gate != nil {
		if err := r.SetBudgetGate(p.Gate); err != nil {
			return err
		}
	}
	if p.Step != nil {
		return r.SetStepGate(p.Step)
	}
	return nil
}
