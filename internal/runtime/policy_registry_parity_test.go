package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
)

// Every analytics operation the registry exposes must be reachable through at
// least one scope. Three of them were not: run_funnel, run_retention and
// list_tests were registered ops with no tool constant and no scopeTools entry,
// so the default-deny allow-list stripped them before the model ever saw a
// schema — while the insight-digest preset instructed agents to call
// `run_funnel` and `run_retention` by name. A prompt that names a denied tool
// spends turns discovering it cannot be called.
//
// tools.go already states the contract ("They must equal the opcore operation
// names registered in usecase.Registry so the scope -> tool allow-list lines up
// with the registry"). This test is that contract, enforced.
func TestEveryRegisteredOperationIsReachableThroughAScope(t *testing.T) {
	granted := map[string]bool{}
	for _, name := range ScopeToolNames(Scopes{Monitor: true, DataQuality: true, AnalyzeBuild: true, GrowthSuggest: true}) {
		granted[name] = true
	}
	for _, op := range usecase.Registry().Specs() {
		if !granted[op.OpName()] {
			t.Errorf("operation %q is registered but no scope grants it — the policy will deny it", op.OpName())
		}
	}
}

// The inverse: a scope must not promise a tool the registry does not implement.
func TestEveryScopedToolIsARegisteredOperation(t *testing.T) {
	registered := map[string]bool{}
	for _, op := range usecase.Registry().Specs() {
		registered[op.OpName()] = true
	}
	for scope, tools := range scopeTools {
		for _, name := range tools {
			if !registered[name] {
				t.Errorf("scope %q grants %q, which is not a registered operation", scope, name)
			}
		}
	}
}

// The evidence guard derives its evidence set from readTools; a read tool
// missing there is silently treated as non-evidence, so a run that genuinely
// queried the project reads as unsourced.
func TestReadToolsCoversTheReadOperations(t *testing.T) {
	for _, name := range []string{ToolRunFunnel, ToolRunRetention, ToolListTests} {
		if !readTools[name] {
			t.Errorf("%q reads project data but is not classified in readTools", name)
		}
	}
}
