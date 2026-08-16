package agentruntime

import (
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/sandbox"
)

func TestToolCatalogContainsHTTPRequest(t *testing.T) {
	cat := ToolCatalog()
	var found bool
	for _, e := range cat {
		if e.Name == sandbox.ToolHTTPRequest {
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
	if !IsRegisteredTool(sandbox.ToolHTTPRequest) {
		t.Error("http_request should be registered")
	}
	if IsRegisteredTool("not_a_tool") {
		t.Error("unknown tool should not be registered")
	}
}

func TestBuildToolHTTPRequestHappyPath(t *testing.T) {
	tool, err := BuildTool(sandbox.ToolHTTPRequest, `{"allow_hosts":["api.example.com"],"allow_http":false}`)
	if err != nil {
		t.Fatalf("BuildTool: %v", err)
	}
	if tool.Name() != sandbox.ToolHTTPRequest {
		t.Fatalf("tool name = %q", tool.Name())
	}
	ht, ok := tool.(*sandbox.HTTPTool)
	if !ok {
		t.Fatalf("expected *sandbox.HTTPTool, got %T", tool)
	}
	if hosts := ht.AllowHosts(); len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Fatalf("allow hosts = %v", hosts)
	}
}

func TestBuildToolHTTPRequestRejectsEmptyAllowlist(t *testing.T) {
	for _, cfg := range []string{``, `{}`, `{"allow_hosts":[]}`, `{"allow_hosts":["  "]}`} {
		if _, err := BuildTool(sandbox.ToolHTTPRequest, cfg); err == nil {
			t.Errorf("config %q: expected empty-allowlist error", cfg)
		}
	}
}

func TestBuildToolRejectsInvalidConfigAndUnknownName(t *testing.T) {
	if _, err := BuildTool(sandbox.ToolHTTPRequest, `{not json`); err == nil {
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
	ctx := ToolBuildContext{Sandbox: stubSandbox{}, Workspace: testWorkspace(t)}
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

// catalogNames is the set of tools an operator would see offered on the agent's
// tool page for a given deployment.
func catalogNames(ctxs ...ToolBuildContext) map[string]bool {
	cat := ToolCatalog(ctxs...)
	names := make(map[string]bool, len(cat))
	for _, e := range cat {
		names[e.Name] = true
	}
	return names
}

// TestToolCatalogOffersTheShellOnceThereIsAWorkspace pins what the tool page
// shows, which is the surface the ungating exists for: someone who just ran the
// binary should be offered the shell, not told to install Docker first.
//
// A workspace is the one thing it does still need — the shell's whole value is
// running what the file tools wrote — and every deployment now has one by
// default, so this is a floor, not a gate.
func TestToolCatalogOffersTheShellOnceThereIsAWorkspace(t *testing.T) {
	if catalogNames()[sandbox.ToolRunShell] {
		t.Fatal("run_shell was offered by a deployment with nowhere to run it")
	}

	hostOnly := catalogNames(ToolBuildContext{WorkspaceBase: t.TempDir()})
	for _, name := range []string{sandbox.ToolRunShell, sandbox.ToolComputerUse, sandbox.ToolBrowserUse} {
		if !hostOnly[name] {
			t.Errorf("%s is hidden from a deployment with no sandbox; that is the hard gate this change removed", name)
		}
	}

	// The one deployment that still hides them: isolation was demanded and is
	// not there. Withholding beats silently running on the host.
	demanded := catalogNames(ToolBuildContext{WorkspaceBase: t.TempDir(), SandboxRequired: true})
	for _, name := range []string{sandbox.ToolRunShell, sandbox.ToolComputerUse, sandbox.ToolBrowserUse} {
		if demanded[name] {
			t.Errorf("%s offered even though AGENTRAY_SANDBOX_REQUIRED is set and no sandbox is wired", name)
		}
	}
	if !demanded[sandbox.ToolReadFile] {
		t.Error("read_file should survive a missing sandbox — it can only ever touch the workspace")
	}

	sandboxed := catalogNames(ToolBuildContext{WorkspaceBase: t.TempDir(), SandboxRequired: true, Sandbox: stubSandbox{}})
	if !sandboxed[sandbox.ToolRunShell] {
		t.Error("run_shell missing from a deployment that both requires and has a sandbox")
	}
}

// TestBuildToolRunShellNeedsAWorkspaceNotASandbox states the same rule at the
// builder, where a stale per-agent selection is resolved.
func TestBuildToolRunShellNeedsAWorkspaceNotASandbox(t *testing.T) {
	if _, err := BuildTool(sandbox.ToolRunShell, `{}`); err == nil {
		t.Fatal("expected run_shell to require a workspace")
	}

	tool, err := BuildToolWithContext(ToolBuildContext{Workspace: testWorkspace(t)}, sandbox.ToolRunShell, `{}`)
	if err != nil {
		t.Fatalf("run_shell must build on the host when no sandbox is wired: %v", err)
	}
	if tool.Name() != sandbox.ToolRunShell {
		t.Fatalf("tool name = %q", tool.Name())
	}

	_, err = BuildToolWithContext(
		ToolBuildContext{Workspace: testWorkspace(t), SandboxRequired: true},
		sandbox.ToolRunShell, `{}`)
	if err == nil {
		t.Fatal("run_shell must fail closed when isolation was demanded and is absent")
	}
}

func TestToolCatalogIncludesWorkspaceToolsOnlyWhenWorkspaceReady(t *testing.T) {
	names := catalogNames(ToolBuildContext{Workspace: testWorkspace(t)})
	if !names[sandbox.ToolReadFile] || !names[sandbox.ToolWriteFile] {
		t.Fatalf("workspace tools missing from catalog: %v", names)
	}

	bare := catalogNames()
	for _, name := range []string{sandbox.ToolReadFile, sandbox.ToolWriteFile, sandbox.ToolGrep, sandbox.ToolGlob} {
		if bare[name] {
			t.Errorf("%s offered by a deployment with no workspace at all", name)
		}
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

func TestBuildBrowserUseRequiresAWorkspaceAlways(t *testing.T) {
	// The workspace is where a screenshot or a download lands, so browser_use
	// needs one in every mode.
	if _, err := BuildToolWithContext(ToolBuildContext{Sandbox: stubSandbox{}}, sandbox.ToolBrowserUse, `{}`); err == nil {
		t.Fatal("expected browser_use to require workspace")
	}
	if _, err := BuildToolWithContext(
		ToolBuildContext{Workspace: testWorkspace(t), SandboxRequired: true},
		sandbox.ToolBrowserUse, `{}`); err == nil {
		t.Fatal("expected browser_use to fail closed when isolation was demanded and is absent")
	}

	for _, ctx := range []ToolBuildContext{
		{Workspace: testWorkspace(t)},
		{Workspace: testWorkspace(t), Sandbox: stubSandbox{}},
	} {
		tool, err := BuildToolWithContext(ctx, sandbox.ToolBrowserUse, `{}`)
		if err != nil {
			t.Fatalf("BuildToolWithContext(sandbox=%v): %v", ctx.Sandbox != nil, err)
		}
		if tool.Name() != sandbox.ToolBrowserUse {
			t.Fatalf("tool name = %q", tool.Name())
		}
	}
}
