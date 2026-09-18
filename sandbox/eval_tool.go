package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

var evalToolSequence atomic.Uint64

// EvalTool executes one Python cell in a retained, conversation-scoped
// subprocess. The registry—not this per-run tool instance—owns the process, so
// state survives the runtime rebuilding tools on the next chat turn.
type EvalTool struct {
	processSB   agentcore.ProcessSandbox
	workspace   *Workspace
	registry    *EvalSessionRegistry
	config      EvalConfig
	hosted      bool
	namespace   string
	instance    string
	fingerprint string
}

func NewEvalTool(sb agentcore.Sandbox, workspace *Workspace, registry *EvalSessionRegistry, namespace string, config EvalConfig) (*EvalTool, error) {
	if workspace == nil {
		return nil, fmt.Errorf("eval requires an agent workspace")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	hosted := sb == nil
	if _, ok := sb.(*HostSandbox); ok {
		hosted = true
	}
	if sb == nil {
		sb = NewHostSandbox()
	}
	processSB, ok := sb.(agentcore.ProcessSandbox)
	if !ok {
		return nil, fmt.Errorf("eval requires a sandbox backend with interactive process support")
	}
	if registry == nil {
		registry = NewEvalSessionRegistry(0, 0)
	}
	encoded, _ := json.Marshal(config)
	sum := sha256.Sum256(encoded)
	return &EvalTool{
		processSB: processSB, workspace: workspace, registry: registry, config: config,
		hosted: hosted, namespace: strings.TrimSpace(namespace),
		instance:    fmt.Sprintf("eval-%d", evalToolSequence.Add(1)),
		fingerprint: hex.EncodeToString(sum[:8]),
	}, nil
}

func (t *EvalTool) Name() string { return ToolEval }

func (t *EvalTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolEval,
		Description: "Execute one Python cell in a persistent conversation-scoped runtime. Variables, imports, functions, and objects survive later eval calls. " +
			"Use reset=true to discard this conversation's Python state before the cell. Relative file access starts in the shared agent workspace. " +
			"display(value), rich reprs, and final expressions preserve Markdown, JSON, PNG, and JPEG output; vision-capable models receive images natively. " +
			"Output is bounded; interactive input is unsupported. A timeout discards the kernel and never replays the cell. " +
			"The runtime is operator-provisioned and server deployments execute it inside the configured sandbox.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language":        map[string]any{"type": "string", "enum": []string{"python"}, "description": "Runtime language. Python is currently supported."},
				"code":            map[string]any{"type": "string", "description": "One Python cell, executed verbatim. Top-level await and display(value) are supported."},
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": t.config.TimeoutSeconds, "description": "Optional cell timeout; defaults to the configured maximum."},
				"reset":           map[string]any{"type": "boolean", "description": "Discard retained Python state before executing this cell."},
			},
			"required": []string{"language", "code"},
		},
	}
}

func (t *EvalTool) Run(ctx context.Context, args string) (string, error) {
	out, err := t.run(ctx, args)
	return out.Content, err
}

// RunRich preserves MIME displays for the agent loop. Run remains the
// compatibility path for direct callers and returns the identical text.
func (t *EvalTool) RunRich(ctx context.Context, args string) (agentcore.ToolOutput, error) {
	return t.run(ctx, args)
}

func (t *EvalTool) run(ctx context.Context, args string) (agentcore.ToolOutput, error) {
	var in struct {
		Language       string  `json:"language"`
		Code           *string `json:"code"`
		TimeoutSeconds int     `json:"timeout_seconds"`
		Reset          bool    `json:"reset"`
	}
	dec := json.NewDecoder(strings.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return agentcore.ToolOutput{}, fmt.Errorf("eval: invalid arguments: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return agentcore.ToolOutput{}, fmt.Errorf("eval: invalid arguments: expected one JSON object")
	}
	if strings.ToLower(strings.TrimSpace(in.Language)) != "python" {
		return agentcore.ToolOutput{}, fmt.Errorf("eval: unsupported language %q", in.Language)
	}
	if in.Code == nil {
		return agentcore.ToolOutput{}, fmt.Errorf("eval: code is required")
	}
	timeout := in.TimeoutSeconds
	if timeout == 0 {
		timeout = t.config.TimeoutSeconds
	}
	if timeout < 1 || timeout > t.config.TimeoutSeconds {
		return agentcore.ToolOutput{}, fmt.Errorf("eval: timeout_seconds must be between 1 and %d", t.config.TimeoutSeconds)
	}
	if err := ctx.Err(); err != nil {
		return agentcore.ToolOutput{}, err
	}

	key := t.sessionKey(ctx)
	kernel, err := t.registry.acquire(key, in.Reset, t.startProcess)
	if err != nil {
		return agentcore.ToolOutput{}, fmt.Errorf("eval: start Python kernel: %w", err)
	}
	released := false
	release := func() {
		if !released {
			t.registry.release(key, kernel)
			released = true
		}
	}
	defer release()

	cellCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	result, err := kernel.execute(cellCtx, *in.Code, t.config.MaxOutputBytes)
	cancel()
	if err != nil {
		// The cell may already have performed side effects. Kill and forget the
		// process, but never replay the code in a fresh kernel.
		t.registry.invalidate(key, kernel)
		released = true
		if contextDeadline(cellCtx, err) {
			return agentcore.ToolOutput{}, fmt.Errorf("eval: cell timed out after %ds; kernel discarded and cell not replayed", timeout)
		}
		return agentcore.ToolOutput{}, fmt.Errorf("eval: kernel discarded and cell not replayed: %w", err)
	}
	output := strings.TrimSpace(result.Output)
	if output == "" {
		output = "(no output)"
	}
	if result.RuntimeError {
		return agentcore.ToolOutput{}, fmt.Errorf("eval: Python cell %d failed; retained state before the error may remain:\n%s", result.ExecutionCount, output)
	}
	return agentcore.ToolOutput{Content: output, Parts: result.Parts}, nil
}

func contextDeadline(ctx context.Context, err error) bool {
	return ctx.Err() == context.DeadlineExceeded || err == context.DeadlineExceeded
}

func (t *EvalTool) sessionKey(ctx context.Context) string {
	session := strings.TrimSpace(agentcore.SandboxSessionFrom(ctx))
	if session == "" {
		session = t.instance
	}
	namespace := t.namespace
	if namespace == "" {
		namespace = t.instance
	}
	return strings.Join([]string{namespace, session, t.workspace.Root(), "python", t.fingerprint}, "\x00")
}

func (t *EvalTool) startProcess() (*evalProcess, error) {
	env := map[string]string{
		"HOME": sandboxWorkdir, "TMPDIR": sandboxWorkdir,
		"PYTHONUNBUFFERED": "1", "PYTHONIOENCODING": "utf-8", "PYTHONDONTWRITEBYTECODE": "1",
	}
	cleanup := func() {}
	if t.hosted {
		home, err := os.MkdirTemp("", "agentray-eval-home-")
		if err != nil {
			return nil, err
		}
		cleanup = func() { _ = os.RemoveAll(home) }
		env["HOME"], env["TMPDIR"] = home, home
		for _, key := range []string{"PATH", "LANG", "LC_ALL", "PYTHONPATH", "VIRTUAL_ENV", "CONDA_PREFIX"} {
			if value := os.Getenv(key); value != "" {
				env[key] = value
			}
		}
	}
	argv := append([]string{t.config.Python.Command}, t.config.Python.Args...)
	argv = append(argv, "-u", "-c", pythonEvalRunner)
	proc, err := t.processSB.Start(context.Background(), agentcore.SandboxExec{
		Argv: argv, Env: env, Image: t.config.Python.Image,
		Mounts:  []agentcore.SandboxMount{{Source: t.workspace.Root(), Target: shellWorkdir}},
		Workdir: shellWorkdir,
		Constraints: agentcore.SandboxLimits{
			RunAsHostUser: true,
			MemoryMB:      512, CPUs: 1, PidsLimit: 128, TimeoutSeconds: evalProcessLifetime.Seconds(),
		},
	})
	if err != nil {
		cleanup()
		return nil, err
	}
	return newEvalProcess(proc, cleanup), nil
}
