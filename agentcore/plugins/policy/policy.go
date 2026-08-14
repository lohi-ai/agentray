// Package policy installs the permission gate: the trust boundary between what
// an agent is configured to do and what it may actually do.
//
// The gate is a BeforeToolCall hook registered at agentcore.PriorityGate, so it
// is consulted before any consumer hook regardless of where this plugin sits in
// the list. That ordering is a property of the composition, not of construction
// order — a governance guarantee you cannot accidentally reorder away.
package policy

import "github.com/lohi-ai/agentray/agentcore"

// Plugin installs the permission gate. A nil Policy denies everything, which is
// the safe default for a composition that forgot to configure one.
type Plugin struct {
	Policy agentcore.Policy
}

// Name identifies the plugin.
func (Plugin) Name() string { return "policy" }

// Register claims the policy seam and registers the gate hook that enforces it.
func (p Plugin) Register(r *agentcore.Registry) error { return r.UsePolicy(p.Policy) }

// AllowList permits exactly the named tools.
func AllowList(names ...string) Plugin { return Plugin{Policy: agentcore.NewAllowList(names...)} }

// DenyAll permits nothing. Useful as an explicit statement in a composition
// that grants capabilities elsewhere.
func DenyAll() Plugin { return Plugin{Policy: agentcore.DenyAll{}} }
