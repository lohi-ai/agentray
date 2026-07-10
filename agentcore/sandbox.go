package agentcore

import "context"

// sandboxSessionKey carries the persistent-session id down the context into a
// session-aware Sandbox backend. agentcore stays generic: it never learns what
// the id means; it only lets a session-capable tool (computer_use) key a
// long-lived execution environment so installed packages and written files
// survive across tool calls in one conversation — the property that turns a
// one-shot shell into a Claude-Code-level computer-use surface.
type sandboxSessionKey struct{}

// WithSandboxSession tags ctx with a stable session id a SessionSandbox uses to
// reuse one persistent container across tool calls. The consumer (the Runner)
// sets it to the conversation session id just before driving the loop; an empty
// id is a no-op and leaves tools on the ephemeral, throwaway-container path.
func WithSandboxSession(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sandboxSessionKey{}, id)
}

// SandboxSessionFrom reads the session id set by WithSandboxSession ("" when
// absent — the safe default that keeps execution ephemeral and unshared).
func SandboxSessionFrom(ctx context.Context) string {
	if v, ok := ctx.Value(sandboxSessionKey{}).(string); ok {
		return v
	}
	return ""
}

// Sandbox executes untrusted commands in an isolated environment that cannot
// reach the host's environment, filesystem, or network unless explicitly
// granted. It is the substrate the agent's shell / file / browser tools run in,
// so a prompt-injected command cannot read server secrets, touch the DB, or
// exfiltrate over the network — the in-process Policy gate decides *whether* a
// tool may run; the Sandbox decides *what the host it runs against can see*.
//
// agentcore defines the contract only. A concrete backend (Docker container,
// gVisor/Kata, micro-VM, …) is injected by the host via Env.Sandbox, keeping
// this package a leaf with no infrastructure imports — the same boundary that
// keeps the core reusable across agents (see docs/ARCHITECT-AGENT-BOUNDARY.md).
type Sandbox interface {
	// Exec runs one command to completion inside an ephemeral sandbox and
	// returns its captured output. It MUST NOT inherit the host process
	// environment; only req.Env is exposed inside. A non-zero exit code is a
	// SandboxResult, not an error — error is reserved for the sandbox itself
	// failing to run (backend unavailable, image missing).
	Exec(ctx context.Context, req SandboxExec) (SandboxResult, error)
}

// SessionSandbox is an optional capability a Sandbox backend may also implement
// to support persistent computer-use sessions. When req.Session is non-empty,
// Exec reuses one long-lived container keyed by that id so packages installed
// and files written by an earlier call are visible to a later one (e.g. `pip
// install python-docx` then a script that imports it). CloseSession tears the
// container down when the conversation ends; a backend that does not implement
// this interface simply runs every call ephemerally.
type SessionSandbox interface {
	Sandbox
	// CloseSession reaps the persistent container for id, if any. It is
	// idempotent: closing an unknown or already-closed session is not an error.
	CloseSession(id string) error
}

// SandboxExec is one sandboxed command request.
type SandboxExec struct {
	// Argv is the command and its arguments; Argv[0] is resolved against the
	// sandbox image's PATH, not the host's.
	Argv []string
	// Stdin is fed to the command's standard input.
	Stdin string
	// Env is the ONLY environment visible inside the sandbox. The host process
	// environment is never inherited — this is the property that stops a
	// prompt-injected command from reading DB creds or API keys.
	Env map[string]string
	// Mounts are explicit host paths exposed to the sandbox. Backends must reject
	// mounts they cannot enforce; callers provide only paths already narrowed by a
	// workspace guard.
	Mounts []SandboxMount
	// Workdir overrides the working directory the command starts in. Empty keeps
	// the backend default (the ephemeral scratch workdir). Set it to a mount
	// target so the command runs against shared workspace files.
	Workdir string
	// Session, when non-empty, requests persistent execution: a SessionSandbox
	// reuses one long-lived container keyed by this id across calls, so installed
	// packages and written files survive between tool invocations. Empty (the
	// default) runs the command in a fresh, throwaway container — the fail-safe
	// path every backend supports. The container's network/mount/limit envelope
	// is fixed by the first call that opens the session.
	Session string
	// Image, when non-empty, requests a specific sandbox image for this exec,
	// overriding the backend's default image selection. It lets distinct tools run
	// in purpose-built images within one host (e.g. computer_use in a doc-toolchain
	// image, browser_use in a Chrome image) without sharing a container. A backend
	// that cannot honor it MUST ignore it and fall back to its default — the
	// fail-safe path; agentcore stays generic and never interprets the value.
	Image string
	// Constraints are the resource + isolation caps for this execution.
	Constraints SandboxLimits
}

// SandboxMount exposes one host directory or file inside a sandboxed execution.
type SandboxMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// SandboxLimits are the isolation and resource caps applied to one execution.
// The zero value is fail-closed: no network, read-only root filesystem, and the
// default resource caps applied by the backend.
type SandboxLimits struct {
	Network        bool     // false (default) = no network egress at all
	NetworkAllow   []string // when Network is true and non-empty, egress is confined to these hosts (+subdomains) via the sandbox filtering proxy; empty = open network
	WritableFS     bool     // false (default) = read-only root + small writable workdir
	MemoryMB       int      // 0 = backend default
	CPUs           float64  // 0 = backend default
	PidsLimit      int      // 0 = backend default
	TimeoutSeconds float64  // 0 = backend default; hard-kill after this elapses
}

// SandboxResult is the captured outcome of a sandboxed execution.
type SandboxResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	Killed     bool   // true if the timeout fired and the command was killed
	KillReason string // human-readable kill cause, set when Killed
}
