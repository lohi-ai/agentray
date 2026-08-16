package agentruntime

import (
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/sandbox"
)

// The control plane validates a tool selection at write time, using a context
// built once at startup — which carries no Workspace, because a workspace is
// created per run. The catalog it renders in the same request advertises the
// file and shell tools off WorkspaceBase. If validation disagrees with the
// catalog, the Tools tab offers a tool and then 400s when it is switched on,
// which is every workspace tool on every deployment.
func TestEnablingAWorkspaceToolAgreesWithTheCatalog(t *testing.T) {
	ctx := ToolBuildContext{WorkspaceBase: t.TempDir()}

	// ToolCatalog lists only what the deployment offers, so presence is the claim.
	catalog := map[string]bool{}
	for _, entry := range ToolCatalog(ctx) {
		catalog[entry.Name] = true
	}

	for _, name := range []string{
		sandbox.ToolReadFile, sandbox.ToolWriteFile, sandbox.ToolEditFile,
		sandbox.ToolGrep, sandbox.ToolGlob, sandbox.ToolRunShell,
	} {
		if !catalog[name] {
			t.Fatalf("%s: catalog reports it unavailable with a workspace base configured", name)
		}
		if err := ValidateToolConfig(ctx, name, "{}"); err != nil {
			t.Errorf("%s: catalog offers it but enabling it fails: %v", name, err)
		}
	}
}

// The agreement has to hold in the other direction too: a tool the catalog
// withholds must not be enablable through the API. Isolation is the case that
// matters — a hosted deployment withholds run_shell, and that refusal is the
// governance invariant, not a UI detail.
func TestEnablingAWithheldToolIsRefused(t *testing.T) {
	ctx := ToolBuildContext{WorkspaceBase: t.TempDir(), SandboxRequired: true}

	err := ValidateToolConfig(ctx, sandbox.ToolRunShell, "{}")
	if err == nil {
		t.Fatal("run_shell was accepted with isolation required and no sandbox wired")
	}
	if !strings.Contains(err.Error(), "not available") && !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("refusal does not say why: %v", err)
	}

	// read_file is not an isolation tool and stays enablable — withholding the
	// shell must not take the file tools down with it.
	if err := ValidateToolConfig(ctx, sandbox.ToolReadFile, "{}"); err != nil {
		t.Errorf("read_file refused because the shell is withheld: %v", err)
	}
}

// A non-configurable tool still rejects a config, on the validation path as much
// as the build path — otherwise a stored selection carries settings that nothing
// will ever read.
func TestWorkspaceToolStillRejectsAConfig(t *testing.T) {
	ctx := ToolBuildContext{WorkspaceBase: t.TempDir()}
	if err := ValidateToolConfig(ctx, sandbox.ToolReadFile, `{"root":"/etc"}`); err == nil {
		t.Fatal("read_file accepted a config it does not have")
	}
}
