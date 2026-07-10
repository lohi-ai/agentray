package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// ToolRunShell is the stable tool name the model calls to run a shell command.
// A consumer's Policy must permit this name before the model is shown the tool.
const ToolRunShell = "run_shell"

// ToolComputerUse is the higher-privilege, Claude-Code-level shell. Unlike
// run_shell (locked: ephemeral, read-only, no network), it runs in a persistent
// session container with network egress and a writable filesystem, so the agent
// can install tooling (pip/apt/npm) and have it — plus any files it writes —
// survive across calls: write code, run it, install a PDF/DOCX/XLSX parser, then
// produce a PDF/PPTX/HTML artifact. It is a deliberate, policy-granted capability
// distinct from run_shell so a project opts into it explicitly.
const ToolComputerUse = "computer_use"

// ShellTool lets the agent run a shell command, but only ever inside the
// injected Sandbox — never in the host process. It is the worked example of a
// capability that, pre-sandbox, would have run with the API's full environment
// (DB creds, API keys), filesystem, and network. With the sandbox it sees none
// of those: a prompt-injected `cat /proc/self/environ` returns the container's
// empty env, not the server's.
type ShellTool struct {
	sb        agentcore.Sandbox
	limits    agentcore.SandboxLimits
	workspace *Workspace

	// name/description let one implementation back both the locked run_shell and
	// the persistent computer_use surface (the only behavioural fork is limits +
	// session awareness).
	name        string
	description string
	// persistent makes the tool reuse one session container across calls (keyed
	// by the conversation session on the context), so installs and files survive.
	persistent bool
}

// ComputerUseLimits is the envelope for the persistent computer_use shell:
// network on (to install tooling), writable filesystem (so package managers can
// write), and generous time/memory so a build, a LibreOffice conversion, or a
// document render can finish. It stays hard-isolated by the backend (no host
// env, all caps dropped, no-new-privileges, resource caps).
func ComputerUseLimits() agentcore.SandboxLimits {
	return agentcore.SandboxLimits{
		Network:        true,
		WritableFS:     true,
		MemoryMB:       2048,
		CPUs:           2,
		PidsLimit:      512,
		TimeoutSeconds: 300,
	}
}

// shellWorkdir is where the workspace is mounted inside the shell sandbox when a
// workspace is configured, so run_shell and the read_file/write_file tools share
// one filesystem (a script written with write_file is runnable, and shell output
// is readable back) — the coherent-workspace behaviour an agent expects.
const shellWorkdir = "/workspace"

// NewShellTool builds a run_shell tool over the given sandbox. limits is the
// per-call isolation envelope (the zero value is fail-closed: no network,
// read-only fs, default resource caps). When ws is non-nil the agent workspace
// is bind-mounted read-write at shellWorkdir and becomes the command's working
// directory, so shell commands see the same files as the file tools; when nil
// the shell runs in an ephemeral, empty scratch dir (legacy behaviour).
func NewShellTool(sb agentcore.Sandbox, limits agentcore.SandboxLimits, ws *Workspace) *ShellTool {
	return &ShellTool{
		sb:        sb,
		limits:    limits,
		workspace: ws,
		name:      ToolRunShell,
		description: "Run a shell command inside an isolated sandbox (no host " +
			"filesystem, no host environment, no network unless granted). " +
			"Returns the combined exit code, stdout, and stderr.",
	}
}

// NewComputerUseTool builds the persistent computer_use shell over sb, with the
// agent workspace mounted (required, so artifacts persist on the host) and the
// ComputerUseLimits envelope. It reuses one session container per conversation,
// so installs and written files survive across calls — the Claude-Code-level
// "write code, install a tool, run it, produce a document" loop.
// networkAllow, when non-empty, confines the session's egress to the listed
// hosts (and their subdomains) via the sandbox's filtering proxy (#5b). Empty
// keeps the current open-network behavior.
func NewComputerUseTool(sb agentcore.Sandbox, ws *Workspace, networkAllow ...string) *ShellTool {
	limits := ComputerUseLimits()
	limits.NetworkAllow = networkAllow
	return &ShellTool{
		sb:         sb,
		limits:     limits,
		workspace:  ws,
		persistent: true,
		name:       ToolComputerUse,
		description: "Run a shell command in a persistent, network-enabled Linux " +
			"sandbox with a writable filesystem. State persists across calls in " +
			"the same conversation: install tools (pip/apt/npm), write and run " +
			"code, and produce files in the workspace (parse or generate PDF, " +
			"DOCX, XLSX, PPTX, HTML, etc.). The workspace is the working directory; " +
			"files written there are saved. Returns exit code, stdout, and stderr.",
	}
}

func (t *ShellTool) Name() string { return t.name }

func (t *ShellTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        t.name,
		Description: t.description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute inside the sandbox.",
				},
			},
			"required": []string{"command"},
		},
	}
}

// Run is sequential-only (no ParallelTool): a shell command may mutate the
// session workdir, so concurrent runs are not opted into.
func (t *ShellTool) Run(ctx context.Context, args string) (string, error) {
	if t.sb == nil {
		return "", fmt.Errorf("run_shell: no sandbox configured")
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("run_shell: invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", fmt.Errorf("run_shell: empty command")
	}

	exec := agentcore.SandboxExec{
		Argv:        []string{"/bin/sh", "-c", in.Command},
		Constraints: t.limits,
	}
	// A persistent tool reuses one container per conversation so installs and
	// files survive between calls. The session id rides the context (set by the
	// runtime); absent it, the call degrades to an ephemeral run.
	if t.persistent {
		exec.Session = agentcore.SandboxSessionFrom(ctx)
	}
	// Share the agent workspace with the file tools: bind-mount it read-write and
	// run from it, so write_file → run_shell (and back) is coherent. The bind
	// mount stays writable even with the read-only root; isolation (no host env,
	// no network) is unchanged.
	if t.workspace != nil {
		exec.Mounts = []agentcore.SandboxMount{{
			Source:   t.workspace.Root(),
			Target:   shellWorkdir,
			ReadOnly: false,
		}}
		exec.Workdir = shellWorkdir
	}
	res, err := t.sb.Exec(ctx, exec)
	if err != nil {
		return "", fmt.Errorf("run_shell: %w", err)
	}
	return formatResult(res), nil
}

// formatResult renders a SandboxResult into a compact, model-readable block.
func formatResult(res agentcore.SandboxResult) string {
	var b strings.Builder
	if res.Killed {
		fmt.Fprintf(&b, "killed: %s\n", res.KillReason)
	}
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if out := strings.TrimRight(res.Stdout, "\n"); out != "" {
		fmt.Fprintf(&b, "stdout:\n%s\n", out)
	}
	if errOut := strings.TrimRight(res.Stderr, "\n"); errOut != "" {
		fmt.Fprintf(&b, "stderr:\n%s\n", errOut)
	}
	return strings.TrimRight(b.String(), "\n")
}
