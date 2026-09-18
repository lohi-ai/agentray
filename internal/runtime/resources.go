package agentruntime

import (
	"sync"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/sandbox"
)

// RuntimeResources owns the bounded process-local state shared by every Runner
// in one host process. HTTP handlers intentionally construct short-lived
// Runners, while provider conversations, eval kernels, and language servers
// belong to a logical conversation and must survive that construction boundary.
//
// The bundle is an optimization boundary, never durable truth: a laptop restart
// or server replica miss creates clean state and remains correct. The durable
// session log and workspace are responsible for semantic continuity.
type RuntimeResources struct {
	ProviderSessions *agentcore.ProviderSessionRegistry
	EvalSessions     *sandbox.EvalSessionRegistry
	LSPSessions      *sandbox.LSPSessionRegistry

	closeOnce sync.Once
}

// NewRuntimeResources creates one host-owned bundle with conservative bounded
// defaults. A server should construct exactly one and close it during shutdown.
func NewRuntimeResources() *RuntimeResources {
	return &RuntimeResources{
		ProviderSessions: agentcore.NewProviderSessionRegistry(0, 0),
		EvalSessions:     sandbox.NewEvalSessionRegistry(0, 0),
		LSPSessions:      sandbox.NewLSPSessionRegistry(0, 0),
	}
}

// WithRuntimeResources shares a host bundle with a Runner. Nil fields degrade
// independently to that Runner's defaults, keeping partial embedded setups
// valid without creating a global singleton.
func WithRuntimeResources(resources *RuntimeResources) RunnerOption {
	return func(r *Runner) {
		if resources == nil {
			return
		}
		if resources.ProviderSessions != nil {
			r.ProviderSessions = resources.ProviderSessions
		}
		if resources.EvalSessions != nil {
			r.EvalSessions = resources.EvalSessions
		}
		if resources.LSPSessions != nil {
			r.LSPSessions = resources.LSPSessions
		}
	}
}

// Close releases every provider transport and retained language process once.
// Hosts must stop admitting and drain runs before calling it.
func (r *RuntimeResources) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		if r.ProviderSessions != nil {
			r.ProviderSessions.Close()
		}
		if r.EvalSessions != nil {
			r.EvalSessions.Close()
		}
		if r.LSPSessions != nil {
			r.LSPSessions.Close()
		}
	})
}
