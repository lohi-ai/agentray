package sandbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/definition"
	"github.com/lohi-ai/agentray/agentcore/plugins/model"
	"github.com/lohi-ai/agentray/agentcore/plugins/sandbox"
)

// fakeSandbox is a backend that records nothing — presence is the whole
// assertion here.
type fakeSandbox struct{}

func (fakeSandbox) Exec(context.Context, agentcore.SandboxExec) (agentcore.SandboxResult, error) {
	return agentcore.SandboxResult{}, nil
}

func spine() []agentcore.Plugin {
	return []agentcore.Plugin{
		model.Plugin{Provider: &agentcore.FauxProvider{}, Model: "m"},
	}
}

// TestSandboxInstallsBackendAndGuard is the hybrid claim: one plugin, both
// halves. A composition cannot end up with the substrate and no guard because
// there is no way to ask for only one.
func TestSandboxInstallsBackendAndGuard(t *testing.T) {
	guard := func(context.Context, agentcore.ToolCall) agentcore.Decision {
		return agentcore.Blocked("argument tripped an injection vector")
	}

	reg, err := agentcore.BuildRegistry(append(spine(), sandbox.Guarded(fakeSandbox{}, guard))...)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if owner := reg.Provider("sandbox"); owner != "sandbox" {
		t.Fatalf("the sandbox seam is owned by %q, want the sandbox plugin", owner)
	}
	agent, err := reg.Agent()
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	if dump := agent.Describe(); !strings.Contains(dump, "sandbox") {
		t.Fatalf("the backend did not reach the agent:\n%s", dump)
	}
	// The guard is a real listener, not a stored field — and it actually runs.
	if !strings.Contains(reg.Describe(), "before=1") {
		t.Fatalf("the guard did not register as a before-hook:\n%s", reg.Describe())
	}
	if dec := guard(context.Background(), agentcore.ToolCall{Name: "run_shell"}); dec.Allow {
		t.Fatal("the guard under test does not block")
	}
}

// TestSandboxSurvivesAnEnvFromAnotherPlugin is the order-independence property
// the split seams exist for. definition owns the env as a whole; sandbox owns
// one capability inside it. Neither may clobber the other, and the answer must
// not depend on which was listed first.
func TestSandboxSurvivesAnEnvFromAnotherPlugin(t *testing.T) {
	env := agentcore.DefaultEnv()

	orders := map[string][]agentcore.Plugin{
		"definition first": {
			definition.Plugin{Env: &env},
			sandbox.In(fakeSandbox{}),
		},
		"sandbox first": {
			sandbox.In(fakeSandbox{}),
			definition.Plugin{Env: &env},
		},
	}
	for name, extra := range orders {
		t.Run(name, func(t *testing.T) {
			agent, err := agentcore.Build(append(spine(), extra...)...)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !describes(agent.Describe(), "sandbox", "set") {
				t.Fatalf("an Env from another plugin erased the sandbox:\n%s", agent.Describe())
			}
		})
	}
}

// TestCredentialsIsItsOwnSeam: holding secrets and running untrusted code are
// separate capabilities, so they must be separately installable — and the
// credentials plugin must not need the sandbox one to work.
func TestCredentialsIsItsOwnSeam(t *testing.T) {
	reg, err := agentcore.BuildRegistry(append(spine(), sandbox.Vault(stubResolver{}))...)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if owner := reg.Provider("credentials"); owner != "credentials" {
		t.Fatalf("the credentials seam is owned by %q", owner)
	}
	if owner := reg.Provider("sandbox"); owner != "" {
		t.Fatalf("installing a vault claimed the sandbox seam (owner %q)", owner)
	}
}

// TestTwoSandboxPluginsConflict: a seam has exactly one provider, and two
// plugins fighting over the substrate must fail the composition by name rather
// than let one win silently.
func TestTwoSandboxPluginsConflict(t *testing.T) {
	_, err := agentcore.BuildRegistry(append(spine(),
		sandbox.In(fakeSandbox{}),
		renamed{sandbox.In(fakeSandbox{})},
	)...)
	if err == nil {
		t.Fatal("two sandbox backends composed without complaint")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("the conflict does not name the seam: %v", err)
	}
}

type renamed struct{ sandbox.Plugin }

func (renamed) Name() string { return "sandbox:other" }

type stubResolver struct{}

func (stubResolver) Resolve(_ context.Context, args string) (string, error) { return args, nil }

// describes reports whether Describe()'s line for key carries the given value.
func describes(dump, key, value string) bool {
	for _, ln := range strings.Split(dump, "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v) == value
		}
	}
	return false
}
