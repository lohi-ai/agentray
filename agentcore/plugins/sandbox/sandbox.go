// Package sandbox installs the isolation substrate for tools that execute
// untrusted code, together with the guard that watches what is sent into it.
//
// It is a HYBRID plugin, the same shape as policy: a seam and its enforcement
// registered by one plugin, because the two are one capability. A composition
// that could install the backend and forget the guard would be a composition
// that can hand the model a shell with nothing reading the arguments — and the
// forgetting would be silent. Here it cannot happen: there is one plugin, and it
// contributes both.
//
// The guard implementation itself is NOT here. What counts as a prompt-injection
// or exfiltration attempt is a product judgment that changes with the deployment,
// so it arrives as configuration (Guard) from the host — the same way the backend
// does. This package owns the wiring and the ordering, not the policy.
package sandbox

import (
	"github.com/lohi-ai/agentray/agentcore"
)

// Plugin installs a sandbox backend and, optionally, the argument guard in front
// of it.
type Plugin struct {
	// Backend is the isolation substrate (a container, a micro-VM, …). nil
	// installs no backend, which leaves sandbox-requiring tools unable to run —
	// a degraded agent, never a silently unisolated one.
	Backend agentcore.Sandbox
	// Guard inspects every tool call's arguments before it executes and may
	// block it. It is registered at agentcore.PriorityGate, alongside the
	// permission gate and ahead of every consumer hook, so a call it blocks is
	// never observed as allowed by anything downstream.
	//
	// nil installs no guard. That is a real choice for a composition whose
	// sandboxed tools take no model-authored free text, so it is permitted — but
	// it is the one field worth being deliberate about.
	Guard agentcore.BeforeToolCall
}

// Name identifies the plugin.
func (Plugin) Name() string { return "sandbox" }

// Register claims the sandbox seam and installs the guard at PriorityGate.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Backend != nil {
		if err := r.SetSandbox(p.Backend); err != nil {
			return err
		}
	}
	if p.Guard != nil {
		r.AddHooks(agentcore.PriorityGate, agentcore.Hooks{
			Before: []agentcore.BeforeToolCall{p.Guard},
		})
	}
	return nil
}

// In builds a plugin for a backend with no argument guard.
func In(backend agentcore.Sandbox) Plugin { return Plugin{Backend: backend} }

// Guarded builds a plugin for a backend with an argument guard in front of it —
// the pairing this package exists to keep together.
func Guarded(backend agentcore.Sandbox, guard agentcore.BeforeToolCall) Plugin {
	return Plugin{Backend: backend, Guard: guard}
}

// Credentials installs the secret vault: the resolver consulted at the trust
// boundary, after a call has been traced and gated and immediately before it
// executes, so a {{cred:NAME}} the model emitted becomes a real secret only in
// the string handed to the running tool.
//
// It lives beside the sandbox because it is the other half of the same
// question — what a tool may reach that the model may not see — but it is its
// own plugin: an agent can hold secrets without running untrusted code, and can
// run untrusted code holding none.
type Credentials struct {
	Resolver agentcore.CredentialResolver
}

// Name identifies the plugin.
func (Credentials) Name() string { return "credentials" }

// Register claims the credentials seam.
func (p Credentials) Register(r *agentcore.Registry) error {
	if p.Resolver == nil {
		return nil
	}
	return r.SetCredentials(p.Resolver)
}

// Vault builds a credentials plugin from a resolver.
func Vault(resolver agentcore.CredentialResolver) Credentials {
	return Credentials{Resolver: resolver}
}
