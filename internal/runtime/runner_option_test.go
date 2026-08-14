package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
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
