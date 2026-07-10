package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/httptool"
	"github.com/lohi-ai/agentray/sandbox"
	"github.com/lohi-ai/agentray/internal/storage"
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
	global := fakeTool{name: httptool.ToolHTTPRequest}
	tools, err := resolveRunTools(ToolBuildContext{}, global, nil)
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if names := toolNames(tools); !names[httptool.ToolHTTPRequest] || len(names) != 1 {
		t.Fatalf("expected only the global http_request, got %v", names)
	}
}

func TestResolveRunToolsNoGlobalNoSelections(t *testing.T) {
	tools, err := resolveRunTools(ToolBuildContext{}, nil, nil)
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected no tools, got %d", len(tools))
	}
}

func TestResolveRunToolsEnabledSelectionOverridesGlobal(t *testing.T) {
	global := fakeTool{name: httptool.ToolHTTPRequest}
	tools, err := resolveRunTools(ToolBuildContext{}, global, []storage.AgentToolSelection{
		{Name: httptool.ToolHTTPRequest, Enabled: true, ConfigJSON: `{"allow_hosts":["api.example.com"]}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected exactly one http_request (per-agent overriding global), got %d", len(tools))
	}
	// The per-agent build is a real *httptool.HTTPTool, not the fake global.
	if _, ok := tools[0].(*httptool.HTTPTool); !ok {
		t.Fatalf("expected per-agent *httptool.HTTPTool, got %T", tools[0])
	}
}

func TestResolveRunToolsDisabledSelectionSuppressesGlobal(t *testing.T) {
	global := fakeTool{name: httptool.ToolHTTPRequest}
	tools, err := resolveRunTools(ToolBuildContext{}, global, []storage.AgentToolSelection{
		{Name: httptool.ToolHTTPRequest, Enabled: false},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("a disabled selection must suppress the global default, got %d tools", len(tools))
	}
}

func TestResolveRunToolsFailsClosedOnBadConfig(t *testing.T) {
	_, err := resolveRunTools(ToolBuildContext{}, nil, []storage.AgentToolSelection{
		{Name: httptool.ToolHTTPRequest, Enabled: true, ConfigJSON: `{"allow_hosts":[]}`},
	})
	if err == nil {
		t.Fatal("expected fail-closed error for an enabled selection with an empty allowlist")
	}
}

func TestResolveRunToolsBuildsRunShellFromSandboxContext(t *testing.T) {
	tools, err := resolveRunTools(ToolBuildContext{Sandbox: stubSandbox{}}, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolRunShell, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if names := toolNames(tools); !names[sandbox.ToolRunShell] || len(names) != 1 {
		t.Fatalf("expected only run_shell, got %v", names)
	}
}

func TestResolveRunToolsSkipsRunShellWhenNoSandbox(t *testing.T) {
	// A stale run_shell selection on a deployment with no sandbox must be skipped,
	// not abort the run — the agent still runs with its remaining tools.
	tools, err := resolveRunTools(ToolBuildContext{}, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolRunShell, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("expected run_shell to be skipped, got error: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected no tools (run_shell unavailable), got %d", len(tools))
	}
}

func TestResolveRunToolsSkipsUnavailableButKeepsAvailable(t *testing.T) {
	// run_shell is unavailable (no sandbox) but http_request is fine: the run
	// proceeds with http_request rather than dying on the stale shell selection.
	tools, err := resolveRunTools(ToolBuildContext{}, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolRunShell, Enabled: true, ConfigJSON: `{}`},
		{Name: httptool.ToolHTTPRequest, Enabled: true, ConfigJSON: `{"allow_hosts":["api.example.com"]}`},
	})
	if err != nil {
		t.Fatalf("resolveRunTools: %v", err)
	}
	if names := toolNames(tools); !names[httptool.ToolHTTPRequest] || names[sandbox.ToolRunShell] || len(names) != 1 {
		t.Fatalf("expected only http_request, got %v", names)
	}
}

func TestResolveRunToolsFailsClosedOnUnknownTool(t *testing.T) {
	// An unregistered tool is not "unavailable" — it is a real misconfiguration,
	// so it must still fail closed rather than being silently dropped.
	_, err := resolveRunTools(ToolBuildContext{}, nil, []storage.AgentToolSelection{
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
	tools, err := resolveRunTools(ToolBuildContext{Sandbox: stubSandbox{}, Workspace: ws}, nil, []storage.AgentToolSelection{
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
	tools, err := resolveRunTools(ToolBuildContext{}, nil, []storage.AgentToolSelection{
		{Name: sandbox.ToolReadFile, Enabled: true, ConfigJSON: `{}`},
	})
	if err != nil {
		t.Fatalf("expected read_file to be skipped, got error: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected no tools (read_file unavailable), got %d", len(tools))
	}
}
