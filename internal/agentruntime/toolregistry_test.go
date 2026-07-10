package agentruntime

import (
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/httptool"
	"github.com/lohi-ai/agentray/sandbox"
)

func TestToolCatalogContainsHTTPRequest(t *testing.T) {
	cat := ToolCatalog()
	var found bool
	for _, e := range cat {
		if e.Name == httptool.ToolHTTPRequest {
			found = true
			if !e.Configurable {
				t.Error("http_request should be configurable")
			}
		}
	}
	if !found {
		t.Fatalf("http_request not in catalog: %+v", cat)
	}
}

func TestIsRegisteredTool(t *testing.T) {
	if !IsRegisteredTool(httptool.ToolHTTPRequest) {
		t.Error("http_request should be registered")
	}
	if IsRegisteredTool("not_a_tool") {
		t.Error("unknown tool should not be registered")
	}
}

func TestBuildToolHTTPRequestHappyPath(t *testing.T) {
	tool, err := BuildTool(httptool.ToolHTTPRequest, `{"allow_hosts":["api.example.com"],"allow_http":false}`)
	if err != nil {
		t.Fatalf("BuildTool: %v", err)
	}
	if tool.Name() != httptool.ToolHTTPRequest {
		t.Fatalf("tool name = %q", tool.Name())
	}
	ht, ok := tool.(*httptool.HTTPTool)
	if !ok {
		t.Fatalf("expected *httptool.HTTPTool, got %T", tool)
	}
	if hosts := ht.AllowHosts(); len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Fatalf("allow hosts = %v", hosts)
	}
}

func TestBuildToolHTTPRequestRejectsEmptyAllowlist(t *testing.T) {
	for _, cfg := range []string{``, `{}`, `{"allow_hosts":[]}`, `{"allow_hosts":["  "]}`} {
		if _, err := BuildTool(httptool.ToolHTTPRequest, cfg); err == nil {
			t.Errorf("config %q: expected empty-allowlist error", cfg)
		}
	}
}

func TestBuildToolRejectsInvalidConfigAndUnknownName(t *testing.T) {
	if _, err := BuildTool(httptool.ToolHTTPRequest, `{not json`); err == nil {
		t.Error("expected invalid-config error")
	}
	if _, err := BuildTool("not_a_tool", `{}`); err == nil {
		t.Error("expected unknown-tool error")
	} else if !strings.Contains(err.Error(), "not_a_tool") {
		t.Errorf("error should name the tool, got %v", err)
	}
}

// TestBuildNonConfigurableToolRejectsStrayConfig verifies a non-configurable
// built-in tool (run_shell) refuses a populated config rather than silently
// ignoring it: an operator who thinks they constrained the tool would otherwise
// be granted an unconstrained one. Empty / "{}" config is accepted.
func TestBuildNonConfigurableToolRejectsStrayConfig(t *testing.T) {
	ctx := ToolBuildContext{Sandbox: stubSandbox{}}
	for _, ok := range []string{``, `  `, `{}`, ` {} `} {
		if _, err := BuildToolWithContext(ctx, sandbox.ToolRunShell, ok); err != nil {
			t.Errorf("config %q should be accepted, got %v", ok, err)
		}
	}
	if _, err := BuildToolWithContext(ctx, sandbox.ToolRunShell, `{"allow_hosts":["x"]}`); err == nil {
		t.Error("run_shell should reject a populated config")
	} else if !strings.Contains(err.Error(), "does not accept config") {
		t.Errorf("error should explain config is unaccepted, got %v", err)
	}
	if _, err := BuildToolWithContext(ctx, sandbox.ToolRunShell, `{not json`); err == nil {
		t.Error("run_shell should reject malformed config JSON")
	}
}

func TestToolCatalogIncludesRunShellOnlyWhenSandboxReady(t *testing.T) {
	for _, e := range ToolCatalog() {
		if e.Name == sandbox.ToolRunShell {
			t.Fatalf("run_shell should be hidden without a sandbox: %+v", e)
		}
	}
	cat := ToolCatalog(ToolBuildContext{Sandbox: stubSandbox{}})
	var found bool
	for _, e := range cat {
		if e.Name == sandbox.ToolRunShell {
			found = true
			if e.Configurable {
				t.Error("run_shell should not be configurable")
			}
		}
	}
	if !found {
		t.Fatalf("run_shell not in sandbox-ready catalog: %+v", cat)
	}
}

func TestBuildToolRunShellRequiresSandbox(t *testing.T) {
	if _, err := BuildTool(sandbox.ToolRunShell, `{}`); err == nil {
		t.Fatal("expected run_shell to require a sandbox")
	}
	tool, err := BuildToolWithContext(ToolBuildContext{Sandbox: stubSandbox{}}, sandbox.ToolRunShell, `{}`)
	if err != nil {
		t.Fatalf("BuildToolWithContext: %v", err)
	}
	if tool.Name() != sandbox.ToolRunShell {
		t.Fatalf("tool name = %q", tool.Name())
	}
}

func TestToolCatalogIncludesWorkspaceToolsOnlyWhenWorkspaceReady(t *testing.T) {
	ws, err := sandbox.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	cat := ToolCatalog(ToolBuildContext{Workspace: ws})
	names := make(map[string]bool, len(cat))
	for _, e := range cat {
		names[e.Name] = true
	}
	if !names[sandbox.ToolReadFile] || !names[sandbox.ToolWriteFile] {
		t.Fatalf("workspace tools missing from catalog: %+v", cat)
	}
	if names[sandbox.ToolBrowserUse] {
		t.Fatalf("browser_use should require sandbox + workspace: %+v", cat)
	}

	cat = ToolCatalog(ToolBuildContext{Sandbox: stubSandbox{}, Workspace: ws})
	names = make(map[string]bool, len(cat))
	for _, e := range cat {
		names[e.Name] = true
	}
	if !names[sandbox.ToolBrowserUse] {
		t.Fatalf("browser_use missing from sandbox+workspace catalog: %+v", cat)
	}
}

func TestBuildWorkspaceToolsRequireWorkspace(t *testing.T) {
	if _, err := BuildTool(sandbox.ToolReadFile, `{}`); err == nil {
		t.Fatal("expected read_file to require workspace")
	}
	ws, err := sandbox.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	for _, name := range []string{sandbox.ToolReadFile, sandbox.ToolWriteFile} {
		tool, err := BuildToolWithContext(ToolBuildContext{Workspace: ws}, name, `{}`)
		if err != nil {
			t.Fatalf("BuildToolWithContext(%s): %v", name, err)
		}
		if tool.Name() != name {
			t.Fatalf("tool name = %q, want %q", tool.Name(), name)
		}
	}
}

func TestBuildBrowserUseRequiresSandboxAndWorkspace(t *testing.T) {
	ws, err := sandbox.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	if _, err := BuildToolWithContext(ToolBuildContext{Workspace: ws}, sandbox.ToolBrowserUse, `{}`); err == nil {
		t.Fatal("expected browser_use to require sandbox")
	}
	if _, err := BuildToolWithContext(ToolBuildContext{Sandbox: stubSandbox{}}, sandbox.ToolBrowserUse, `{}`); err == nil {
		t.Fatal("expected browser_use to require workspace")
	}
	tool, err := BuildToolWithContext(ToolBuildContext{Sandbox: stubSandbox{}, Workspace: ws}, sandbox.ToolBrowserUse, `{}`)
	if err != nil {
		t.Fatalf("BuildToolWithContext: %v", err)
	}
	if tool.Name() != sandbox.ToolBrowserUse {
		t.Fatalf("tool name = %q", tool.Name())
	}
}
