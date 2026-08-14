// Package tools contributes host tools to a composition.
//
// Unlike a seam this is additive: many plugins may each contribute tools, and
// the model's advertised order is the order they were registered in. What the
// model is actually SHOWN is decided by the permission policy, not by this
// plugin — registering a tool is not granting it.
package tools

import "github.com/lohi-ai/agentray/agentcore"

// Plugin contributes tools to the run's registry.
type Plugin struct {
	// Label distinguishes several tool plugins in one composition (plugin names
	// must be unique). Empty names this plugin "tools".
	Label string
	Tools []agentcore.Tool
}

// Name identifies the plugin.
func (p Plugin) Name() string {
	if p.Label != "" {
		return "tools:" + p.Label
	}
	return "tools"
}

// Register adds the tools.
func (p Plugin) Register(r *agentcore.Registry) error {
	r.AddTools(p.Tools...)
	return nil
}

// Of builds a plugin from loose tools.
func Of(list ...agentcore.Tool) Plugin { return Plugin{Tools: list} }

// FromSet builds a plugin from an existing ToolSet, preserving its
// registration order.
func FromSet(ts *agentcore.ToolSet) Plugin {
	if ts == nil {
		return Plugin{}
	}
	names := ts.Names()
	out := make([]agentcore.Tool, 0, len(names))
	for _, n := range names {
		if t, ok := ts.Get(n); ok {
			out = append(out, t)
		}
	}
	return Plugin{Tools: out}
}
