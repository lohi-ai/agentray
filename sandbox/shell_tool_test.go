package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// stubSandbox records the last request and returns a canned result, so the
// shell tool's contract can be tested without a container runtime.
type stubSandbox struct {
	last   agentcore.SandboxExec
	result agentcore.SandboxResult
	err    error
}

func (s *stubSandbox) Exec(_ context.Context, req agentcore.SandboxExec) (agentcore.SandboxResult, error) {
	s.last = req
	return s.result, s.err
}

func TestShellToolRunsThroughSandbox(t *testing.T) {
	stub := &stubSandbox{result: agentcore.SandboxResult{ExitCode: 0, Stdout: "hi\n"}}
	tool := NewShellTool(stub, agentcore.SandboxLimits{}, nil)

	if tool.Name() != ToolRunShell {
		t.Fatalf("name = %q, want %q", tool.Name(), ToolRunShell)
	}

	out, err := tool.Run(context.Background(), `{"command":"echo hi"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The command must be executed via the sandbox, never the host.
	wantArgv := []string{"/bin/sh", "-c", "echo hi"}
	if strings.Join(stub.last.Argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("argv = %v, want %v", stub.last.Argv, wantArgv)
	}
	// Default limits are fail-closed: no network, read-only fs.
	if stub.last.Constraints.Network || stub.last.Constraints.WritableFS {
		t.Fatalf("expected fail-closed constraints, got %+v", stub.last.Constraints)
	}
	if !strings.Contains(out, "exit_code: 0") || !strings.Contains(out, "hi") {
		t.Fatalf("formatted output missing fields: %q", out)
	}
}

// computer_use is the persistent, network-enabled, writable profile and must
// thread the conversation session id from the context onto the exec so the
// backend reuses one container across calls.
func TestComputerUseToolThreadsSessionAndLimits(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	stub := &stubSandbox{result: agentcore.SandboxResult{ExitCode: 0, Stdout: "ok\n"}}
	tool := NewComputerUseTool(stub, ws)

	if tool.Name() != ToolComputerUse {
		t.Fatalf("name = %q, want %q", tool.Name(), ToolComputerUse)
	}

	ctx := agentcore.WithSandboxSession(context.Background(), "conv-123")
	if _, err := tool.Run(ctx, `{"command":"pip install python-docx"}`); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stub.last.Session != "conv-123" {
		t.Fatalf("session = %q, want conv-123", stub.last.Session)
	}
	// The computer-use envelope is the opposite of run_shell's lock: network on,
	// writable fs, so installs work.
	if !stub.last.Constraints.Network || !stub.last.Constraints.WritableFS {
		t.Fatalf("expected networked writable constraints, got %+v", stub.last.Constraints)
	}
	// Workspace is mounted read-write so produced files persist on the host.
	if len(stub.last.Mounts) != 1 || stub.last.Mounts[0].ReadOnly {
		t.Fatalf("expected one rw workspace mount, got %+v", stub.last.Mounts)
	}
}

// Without a session on the context the computer_use tool still runs (degrades to
// an ephemeral execution) rather than failing.
func TestComputerUseToolWithoutSessionDegrades(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	stub := &stubSandbox{result: agentcore.SandboxResult{ExitCode: 0}}
	tool := NewComputerUseTool(stub, ws)
	if _, err := tool.Run(context.Background(), `{"command":"echo hi"}`); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stub.last.Session != "" {
		t.Fatalf("expected empty session, got %q", stub.last.Session)
	}
}

// TestShellToolSpillsOversizedOutput: output past the visible cap is persisted
// to the workspace with a tail note naming the path (which middle-truncation
// preserves), so the overflow is recoverable instead of lost.
func TestShellToolSpillsOversizedOutput(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	huge := strings.Repeat("build log line\n", 4000) // ~60KB, over the 24KB spill threshold
	stub := &stubSandbox{result: agentcore.SandboxResult{ExitCode: 1, Stdout: huge, Stderr: "final error: it broke"}}
	tool := NewShellTool(stub, agentcore.SandboxLimits{}, ws)

	out, err := tool.Run(context.Background(), `{"command":"make"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "full output saved to "+shellLogDir+"/") {
		t.Fatalf("expected spill note in output tail: %q", out[len(out)-300:])
	}
	// The note names a real, readable file containing the complete output.
	start := strings.Index(out, shellLogDir+"/")
	rel := out[start:]
	rel = rel[:strings.IndexAny(rel, "; ")]
	logOut, rerr := NewReadFileTool(ws).Run(context.Background(), `{"path":"`+rel+`","offset":1,"limit":2}`)
	if rerr != nil {
		t.Fatalf("spilled log not readable via read_file: %v", rerr)
	}
	if !strings.Contains(logOut, "exit_code: 1") {
		t.Fatalf("spilled log missing content: %q", logOut)
	}
}

// TestShellToolSmallOutputDoesNotSpill: in-cap output must stay exactly as
// before — no note, no log file.
func TestShellToolSmallOutputDoesNotSpill(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	stub := &stubSandbox{result: agentcore.SandboxResult{ExitCode: 0, Stdout: "ok\n"}}
	tool := NewShellTool(stub, agentcore.SandboxLimits{}, ws)
	out, err := tool.Run(context.Background(), `{"command":"echo ok"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "full output saved") {
		t.Fatalf("small output must not spill: %q", out)
	}
}

func TestShellToolRejectsEmptyAndBadArgs(t *testing.T) {
	tool := NewShellTool(&stubSandbox{}, agentcore.SandboxLimits{}, nil)
	if _, err := tool.Run(context.Background(), `{"command":"  "}`); err == nil {
		t.Fatal("expected error on empty command")
	}
	if _, err := tool.Run(context.Background(), `not json`); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

func TestShellToolWithoutSandboxErrors(t *testing.T) {
	tool := NewShellTool(nil, agentcore.SandboxLimits{}, nil)
	if _, err := tool.Run(context.Background(), `{"command":"echo hi"}`); err == nil {
		t.Fatal("expected error when no sandbox configured")
	}
}

// When a workspace is configured the shell must share it with the file tools:
// the workspace is bind-mounted read-write and is the command's working dir, so
// write_file → run_shell (and back) operate on one filesystem.
func TestShellToolMountsWorkspace(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	stub := &stubSandbox{result: agentcore.SandboxResult{ExitCode: 0}}
	tool := NewShellTool(stub, agentcore.SandboxLimits{}, ws)

	if _, err := tool.Run(context.Background(), `{"command":"ls"}`); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stub.last.Workdir != shellWorkdir {
		t.Fatalf("workdir = %q, want %q", stub.last.Workdir, shellWorkdir)
	}
	if len(stub.last.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(stub.last.Mounts))
	}
	m := stub.last.Mounts[0]
	if m.Source != ws.Root() || m.Target != shellWorkdir || m.ReadOnly {
		t.Fatalf("mount = %+v, want rw %s->%s", m, ws.Root(), shellWorkdir)
	}
}
