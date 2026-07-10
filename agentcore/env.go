package agentcore

import (
	"context"
	"time"
)

// Clock is the time source the runtime depends on, injected so scheduling and
// tests are deterministic (pi harness ExecutionEnv pattern).
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Env carries injected capabilities the core depends on rather than reaching
// for concrete infrastructure. Extend with FileSystem/Shell as consumers need.
type Env struct {
	Clock Clock
	// Sandbox is the optional isolation substrate for tools that execute
	// untrusted code (shell, file, browser). nil when no such tools are wired —
	// the analytics tools never touch it. The concrete backend lives outside
	// this leaf package and is injected by the host.
	Sandbox Sandbox
	// Credentials is the optional secret resolver. When set, tool arguments are
	// passed through it at the trust boundary — after the call has been traced
	// and gated, immediately before the tool executes — so opaque {{cred:NAME}}
	// placeholders the model emits become real secret values the tool can use
	// (an API key, a scoped DB DSN) without the model, the persisted trace, or
	// the before-hooks ever seeing the literal. nil = no resolution; arguments
	// reach the tool unchanged.
	Credentials CredentialResolver
}

// CredentialResolver substitutes opaque {{cred:NAME}} placeholders in a tool's
// argument JSON with real secret values. It runs at the trust boundary inside
// the tool loop: the call is traced and permission-gated in placeholder form
// first, so neither the model nor the persisted trace ever observes the literal
// secret — it exists only in the string handed to the executing tool.
//
// agentcore defines the contract only; the concrete vault (which holds the
// secrets and decides which the agent may resolve) is injected by the host via
// Env.Credentials, keeping this package a leaf with no infrastructure imports.
// Resolve MUST fail closed: an unknown or disallowed placeholder returns an
// error, which blocks the call and feeds the reason back to the model rather
// than silently leaving the placeholder in place.
type CredentialResolver interface {
	Resolve(ctx context.Context, args string) (string, error)
}

// DefaultEnv returns an Env backed by real implementations.
func DefaultEnv() Env { return Env{Clock: realClock{}} }

// now is a nil-safe accessor for the env clock.
func (e Env) now() time.Time {
	if e.Clock == nil {
		return time.Now().UTC()
	}
	return e.Clock.Now()
}
