package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/sandbox"
)

// fakeTool is a minimal agentcore.Tool used to stand in for a host-global
// default and to assert which named tools resolveRunTools emits.
type fakeTool struct{ name string }

func (f fakeTool) Name() string                                { return f.name }
func (f fakeTool) Schema() agentcore.ToolSchema                { return agentcore.ToolSchema{Name: f.name} }
func (f fakeTool) Run(context.Context, string) (string, error) { return "", nil }

func toolNames(tools []agentcore.Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		out[t.Name()] = true
	}
	return out
}

func TestResolveRunToolsNoSelectionsUsesGlobal(t *testing.T) {
	global := fakeTool{name: sandbox.ToolHTTPRequest}
	tools, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, global, nil)
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if names := toolNames(tools); !names[sandbox.ToolHTTPRequest] || len(names) != 1 {
		t.Fatalf("expected only the global http_request, got %v", names)
	}
}

func TestResolveRunToolsNoGlobalNoSelections(t *testing.T) {
	tools, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, nil)
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected no tools, got %d", len(tools))
	}
}

func TestResolveRunToolsEnabledSelectionOverridesGlobal(t *testing.T) {
	global := fakeTool{name: sandbox.ToolHTTPRequest}
	tools, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, global, []storage.AgentToolSelection{
		{Name: sandbox.ToolHTTPRequest, Enabled: true, ConfigJSON: `{"allow_hosts":["api.example.com"]}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected exactly one http_request (per-agent overriding global), got %d", len(tools))
	}
	// The per-agent build is a real *sandbox.HTTPTool, not the fake global.
	if _, ok := tools[0].(*sandbox.HTTPTool); !ok {
		t.Fatalf("expected per-agent *sandbox.HTTPTool, got %T", tools[0])
	}
}

func TestResolveRunToolsDisabledSelectionSuppressesGlobal(t *testing.T) {
	global := fakeTool{name: sandbox.ToolHTTPRequest}
	tools, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, global, []storage.AgentToolSelection{
		{Name: sandbox.ToolHTTPRequest, Enabled: false},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("a disabled selection must suppress the global default, got %d tools", len(tools))
	}
}

func TestResolveRunToolsFailsClosedOnBadConfig(t *testing.T) {
	_, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolHTTPRequest, Enabled: true, ConfigJSON: `{"allow_hosts":[]}`},
	})
	if err == nil {
		t.Fatal("expected fail-closed error for an enabled selection with an empty allowlist")
	}
}

func TestResolveRunToolsBuildsRunShellFromSandboxContext(t *testing.T) {
	ctx := ToolBuildContext{Sandbox: stubSandbox{}, Workspace: testWorkspace(t)}
	tools, _, err := resolveRunTools(context.Background(), ctx, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolRunShell, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if names := toolNames(tools); !names[sandbox.ToolRunShell] || len(names) != 1 {
		t.Fatalf("expected only run_shell, got %v", names)
	}
}

// TestResolveRunToolsBuildsRunShellWithoutASandbox is the ungating, stated as
// the behavior a user actually meets: install AgentRay, grant the shell, no
// Docker anywhere, and the shell works — on the host, inside the run's
// workspace.
//
// The rule it replaces (no sandbox, no shell) looked like the safe default and
// was not one. It made the first thing a new user tried fail with a
// configuration error, which is not a security control; it is a wall in front of
// the people the control was supposed to protect.
func TestResolveRunToolsBuildsRunShellWithoutASandbox(t *testing.T) {
	ctx := ToolBuildContext{Workspace: testWorkspace(t)}
	tools, _, err := resolveRunTools(context.Background(), ctx, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolRunShell, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if names := toolNames(tools); !names[sandbox.ToolRunShell] {
		t.Fatalf("run_shell was withheld from a deployment with no sandbox: %v", names)
	}
}

// TestResolveRunToolsWithholdsRunShellWhenTheSandboxIsRequired is the other
// half, and the one a hosted deployment depends on: having ASKED for isolation,
// not getting it must withhold the tool rather than quietly run it on the host.
func TestResolveRunToolsWithholdsRunShellWhenTheSandboxIsRequired(t *testing.T) {
	ctx := ToolBuildContext{Workspace: testWorkspace(t), SandboxRequired: true}
	tools, _, err := resolveRunTools(context.Background(), ctx, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolRunShell, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("a withheld tool must skip, not abort the run: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("AGENTRAY_SANDBOX_REQUIRED is set and no sandbox is wired, yet run_shell was built "+
			"on the host anyway: %v", toolNames(tools))
	}
}

func TestResolveRunToolsSkipsUnavailableButKeepsAvailable(t *testing.T) {
	// run_shell is unavailable (isolation demanded, none wired) but http_request
	// is fine: the run proceeds with http_request rather than dying on the stale
	// shell selection.
	ctx := ToolBuildContext{Workspace: testWorkspace(t), SandboxRequired: true}
	tools, _, err := resolveRunTools(context.Background(), ctx, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolRunShell, Enabled: true, ConfigJSON: `{}`},
		{Name: sandbox.ToolHTTPRequest, Enabled: true, ConfigJSON: `{"allow_hosts":["api.example.com"]}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if names := toolNames(tools); !names[sandbox.ToolHTTPRequest] || names[sandbox.ToolRunShell] || len(names) != 1 {
		t.Fatalf("expected only http_request, got %v", names)
	}
}

// testWorkspace is a throwaway workspace for a run under test. Every
// code-executing tool needs somewhere to work, so these tests supply one for the
// same reason a real run does.
func testWorkspace(t *testing.T) *sandbox.Workspace {
	t.Helper()
	ws, err := sandbox.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws
}

func TestResolveRunToolsFailsClosedOnUnknownTool(t *testing.T) {
	// An unregistered tool is not "unavailable" — it is a real misconfiguration,
	// so it must still fail closed rather than being silently dropped.
	_, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, []storage.AgentToolSelection{
		{Name: "definitely_not_a_tool", Enabled: true, ConfigJSON: `{}`},
	})
	if err == nil {
		t.Fatal("expected unknown tool selection to fail closed")
	}
}

func TestResolveRunToolsBuildsWorkspaceTools(t *testing.T) {
	ws, err := sandbox.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	tools, _, err := resolveRunTools(context.Background(), ToolBuildContext{Sandbox: stubSandbox{}, Workspace: ws}, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolReadFile, Enabled: true, ConfigJSON: `{}`},
		{Name: sandbox.ToolWriteFile, Enabled: true, ConfigJSON: `{}`},
		{Name: sandbox.ToolBrowserUse, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	names := toolNames(tools)
	for _, name := range []string{sandbox.ToolReadFile, sandbox.ToolWriteFile, sandbox.ToolBrowserUse} {
		if !names[name] {
			t.Fatalf("missing %s from %v", name, names)
		}
	}
}

func TestResolveRunToolsSkipsWorkspaceToolsWhenWorkspaceMissing(t *testing.T) {
	// A workspace-dependent selection on a deployment with no workspace is stale
	// and must be skipped, not abort the run.
	tools, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolReadFile, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("expected read_file to be skipped, got error: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected no tools (read_file unavailable), got %d", len(tools))
	}
}
