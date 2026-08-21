package agentruntime

import (
	"context"
	"slices"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

// A read-only run is the other half of the shared demo's write guard. The HTTP
// layer refuses a visitor's direct mutations and then lets them ask the agent
// anything — which is only safe if the agent they are asking cannot make those
// same mutations on request. These tests pin that, tool by tool.

// allScopes turns everything on, so the filter is measured against the largest
// possible grant rather than a conveniently small one.
var allScopes = Scopes{Monitor: true, DataQuality: true, AnalyzeBuild: true, GrowthSuggest: true}

func TestReadOnlyRunKeepsReadsAndDropsWrites(t *testing.T) {
	full := ScopeToolNames(allScopes)
	readOnly := ReadOnlyToolNames(full)

	mustKeep := []string{
		ToolActivitySummary, ToolRecentEvents, ToolExploreEvents, ToolPersons,
		ToolRunSQL, ToolRunInsight, ToolRunFunnel, ToolRunRetention, ToolListDashboards,
	}
	for _, name := range mustKeep {
		if !slices.Contains(readOnly, name) {
			t.Errorf("read-only run lost %q — a demo viewer must still be able to read the project", name)
		}
	}
	mustDrop := []string{
		ToolCreateDashboard, ToolCreateChart, ToolSubmitRec, ToolProposeTest,
		ToolRemember, ToolSendNotification,
	}
	for _, name := range mustDrop {
		if slices.Contains(readOnly, name) {
			t.Errorf("read-only run kept %q — the agent could make the change its caller may not", name)
		}
	}
}

// The filter is an allow-list over readTools, not a deny-list of today's write
// tools. A scope that gains a tool and does not classify it as a read must be
// withheld, because the alternative is a new write capability that silently
// reaches demo visitors.
func TestAnUnclassifiedToolIsWithheldFromAReadOnlyRun(t *testing.T) {
	got := ReadOnlyToolNames([]string{ToolRunSQL, "some_new_tool_nobody_classified"})
	if slices.Contains(got, "some_new_tool_nobody_classified") {
		t.Fatal("an unclassified tool survived a read-only run")
	}
	if !slices.Contains(got, ToolRunSQL) {
		t.Fatal("the read it was mixed with was dropped too")
	}
}

// The permission policy is the trust boundary (agentcore installs it as the
// gate hook), so what matters is the allow-list Build ends up with — not just
// the scope helper in isolation.
func TestPermittedToolNamesUnderReadOnly(t *testing.T) {
	shell := stubTool{name: "run_shell"}
	http := stubTool{name: "http_request"}
	base := BuildParams{
		Scopes:    allScopes,
		Tools:     []agentcore.Tool{shell},
		HTTPTool:  http,
		Todo:      &todo.Store{},
		Subagents: &subagent.Plugin{},
	}

	open := permittedToolNames(base)
	for _, name := range []string{ToolCreateChart, "run_shell", "http_request", subagent.ToolSpawnSubagent} {
		if !slices.Contains(open, name) {
			t.Fatalf("an ordinary run lost %q — the read-only branch changed the normal path", name)
		}
	}

	locked := base
	locked.ReadOnly = true
	got := permittedToolNames(locked)
	for _, name := range []string{
		ToolCreateChart, ToolCreateDashboard, ToolRemember, ToolSendNotification,
		// The host-reaching and delegating capabilities go too: a sub-agent is
		// built from its own grants, so leaving spawn in would hand back exactly
		// the writes this is taking away.
		"run_shell", "http_request", subagent.ToolSpawnSubagent,
	} {
		if slices.Contains(got, name) {
			t.Errorf("read-only run permits %q", name)
		}
	}
	for _, name := range []string{ToolRunSQL, ToolRunFunnel, ToolListDashboards, todo.ToolName} {
		if !slices.Contains(got, name) {
			t.Errorf("read-only run withheld %q, which reads (or is the loop's own plumbing)", name)
		}
	}
}

// The policy really is default-deny over that list, so a withheld tool is
// blocked at the gate rather than merely absent from a slice.
func TestReadOnlyPolicyBlocksAWriteTool(t *testing.T) {
	names := permittedToolNames(BuildParams{Scopes: allScopes, ReadOnly: true})
	policy := agentcore.NewAllowList(names...)
	if d := policy.Allow(t.Context(), agentcore.ToolCall{Name: ToolCreateChart}); d.Allow {
		t.Error("create_chart was allowed in a read-only run")
	}
	if d := policy.Allow(t.Context(), agentcore.ToolCall{Name: ToolRunSQL}); !d.Allow {
		t.Error("run_sql was blocked in a read-only run")
	}
}

type stubTool struct{ name string }

func (s stubTool) Name() string { return s.name }
func (s stubTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: s.name, Parameters: map[string]any{"type": "object"}}
}
func (s stubTool) Run(_ context.Context, _ string) (string, error) { return "", nil }
