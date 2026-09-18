package agentruntime

import (
	"context"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/sandbox"
)

// stubSandbox is a no-op agentcore.Sandbox for wiring tests.
type stubSandbox struct{}

func (stubSandbox) Exec(context.Context, agentcore.SandboxExec) (agentcore.SandboxResult, error) {
	return agentcore.SandboxResult{}, nil
}

func TestWithSandboxThreadsIntoRunner(t *testing.T) {
	sb := stubSandbox{}
	r := NewRunner(nil, WithSandbox(sb))
	if r.Sandbox == nil {
		t.Fatal("WithSandbox should populate Runner.Sandbox")
	}
}

func TestNewRunnerDefaultsToNilSandbox(t *testing.T) {
	r := NewRunner(nil)
	if r.Sandbox != nil {
		t.Fatal("Runner.Sandbox must be nil by default (analytics-only)")
	}
}

func TestWithSandboxNilIsNoOp(t *testing.T) {
	// Passing a nil sandbox (the disabled path) must not flip the agent into
	// sandbox mode — it stays nil so the analytics agent is unchanged.
	r := NewRunner(nil, WithSandbox(nil))
	if r.Sandbox != nil {
		t.Fatal("WithSandbox(nil) must leave Runner.Sandbox nil")
	}
}

func TestWithProviderSessionRegistryThreadsIntoRunner(t *testing.T) {
	registry := agentcore.NewProviderSessionRegistry(4, time.Minute)
	defer registry.Close()
	r := NewRunner(nil, WithProviderSessionRegistry(registry))
	if r.ProviderSessions != registry {
		t.Fatal("WithProviderSessionRegistry did not preserve the supplied registry")
	}

	first, releaseFirst := r.acquireProviderSession("conversation")
	releaseFirst()
	second, releaseSecond := r.acquireProviderSession("conversation")
	releaseSecond()
	if first != second {
		t.Fatal("runner did not retain provider state across short-lived run leases")
	}
}

func TestWithEvalSessionRegistryThreadsIntoRunner(t *testing.T) {
	registry := sandbox.NewEvalSessionRegistry(2, time.Minute)
	defer registry.Close()
	r := NewRunner(nil, WithEvalSessionRegistry(registry))
	if r.EvalSessions != registry || r.evalSessionRegistry() != registry {
		t.Fatal("WithEvalSessionRegistry did not preserve the supplied registry")
	}
}

func TestWithLSPSessionRegistryThreadsIntoRunner(t *testing.T) {
	registry := sandbox.NewLSPSessionRegistry(2, time.Minute)
	defer registry.Close()
	r := NewRunner(nil, WithLSPSessionRegistry(registry))
	if r.LSPSessions != registry || r.lspSessionRegistry() != registry {
		t.Fatal("WithLSPSessionRegistry did not preserve the supplied registry")
	}
}
