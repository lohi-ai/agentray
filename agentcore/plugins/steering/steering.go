// Package steering installs the mid-run input queues and the per-turn
// save-point.
//
// Steering is what makes a long run correctable: a user can inject a
// correction that is honored on the next turn instead of having to kill the
// run and start over. Follow-ups continue a conversation within one bounded
// run rather than opening a new one.
package steering

import (
	"context"

	"github.com/lohi-ai/agentray/agentcore"
)

// Plugin installs the steering queues.
type Plugin struct {
	// Steer is drained at the top of every turn; its messages are threaded in
	// before the model reasons.
	Steer func(ctx context.Context) []agentcore.Message
	// FollowUp is drained once the model produces a final answer; returning
	// messages restarts the loop instead of ending the run.
	FollowUp func(ctx context.Context) []agentcore.Message
	// PrepareNextTurn is called after each turn with the current TurnState; the
	// returned state (model / tools / system) drives the NEXT turn and never
	// mutates the in-flight one.
	PrepareNextTurn func(ctx context.Context, state agentcore.TurnState) agentcore.TurnState
}

// Name identifies the plugin.
func (Plugin) Name() string { return "steering" }

// Register claims the steering seams it was given.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Steer != nil {
		if err := r.SetSteeringSource(p.Steer); err != nil {
			return err
		}
	}
	if p.FollowUp != nil {
		if err := r.SetFollowUpSource(p.FollowUp); err != nil {
			return err
		}
	}
	if p.PrepareNextTurn != nil {
		return r.SetPrepareNextTurn(p.PrepareNextTurn)
	}
	return nil
}
