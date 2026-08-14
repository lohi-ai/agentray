// Package definition installs the authored identity — persona, skills, run
// limits, and host environment — that turns the generic runtime into a
// specific agent. It is the config half of the agents-as-config rule: an agent
// is a definition plus a tool grant, never bespoke Go.
package definition

import "github.com/lohi-ai/agentray/agentcore"

// Plugin installs the agent's identity and bounds.
type Plugin struct {
	// Definition is SOUL.md / AGENTS.md / skills.
	Definition agentcore.AgentDefinition
	// Limits bounds the run (turns, tool calls, context, result size). nil keeps
	// agentcore.DefaultLimits().
	Limits *agentcore.Limits
	// Env injects host capabilities (sandbox, clock, credential resolver). nil
	// keeps agentcore.DefaultEnv().
	Env *agentcore.Env
}

// Name identifies the plugin.
func (Plugin) Name() string { return "definition" }

// Register claims the definition, limits, and env seams it was given.
func (p Plugin) Register(r *agentcore.Registry) error {
	if !p.Definition.IsZero() {
		if err := r.SetDefinition(p.Definition); err != nil {
			return err
		}
	}
	if p.Limits != nil {
		if err := r.SetLimits(*p.Limits); err != nil {
			return err
		}
	}
	if p.Env != nil {
		if err := r.SetEnv(*p.Env); err != nil {
			return err
		}
	}
	return nil
}
