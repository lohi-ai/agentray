// Package hooks contributes consumer lifecycle listeners.
//
// Listeners registered here run at agentcore.PriorityDefault, so they always
// run after the permission gate: a consumer hook can observe or rewrite a call
// that was allowed, never approve one the gate blocked.
package hooks

import "github.com/lohi-ai/agentray/agentcore"

// Plugin contributes lifecycle listeners.
type Plugin struct {
	// Label distinguishes several hook plugins in one composition.
	Label string
	Hooks agentcore.Hooks
	// Priority overrides the default ordering. Leave zero unless you know why
	// the listener must run before the gate or after every rewrite.
	Priority agentcore.Priority
}

// Name identifies the plugin.
func (p Plugin) Name() string {
	if p.Label != "" {
		return "hooks:" + p.Label
	}
	return "hooks"
}

// Register adds the listeners at the requested priority.
func (p Plugin) Register(r *agentcore.Registry) error {
	r.AddHooks(p.Priority, p.Hooks)
	return nil
}

// Of builds a plugin from a Hooks value at the default priority.
func Of(h agentcore.Hooks) Plugin { return Plugin{Hooks: h} }
