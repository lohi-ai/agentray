package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

// HostSandbox runs commands directly on the host machine. It is the substrate
// every tool in this package falls back to when no agentcore.Sandbox is
// injected, which makes the sandbox parameter optional in both directions: nil
// means "run here", non-nil means "run in there", and the tools above keep a
// single code path either way.
//
// It is NOT isolation. It is the closest honest host analogue of the
// agentcore.Sandbox contract, and it keeps exactly the parts of that contract a
// plain process can keep:
//
//   - Env is the only environment the child sees. os.Environ() is never
//     inherited, so a prompt-injected `env` or `cat /proc/self/environ` cannot
//     read the server's DB creds or API keys — the one guarantee worth the most
//     and the one a bare exec.CommandContext would have thrown away.
//   - Workdir is resolved through Mounts, so a command runs in the agent
//     workspace rather than the server's working directory.
//   - TimeoutSeconds is enforced with a hard kill of the whole process group, so
//     a command that backgrounded work does not outlive its own timeout.
//
// What it cannot keep, and what a caller must weigh before choosing it:
//
//   - The filesystem is the host's. WritableFS is not enforceable; a command can
//     read anything the server process can read and write anywhere it can write.
//   - The network is the host's. Network and NetworkAllow are not enforceable —
//     there is no container to route through the egress proxy.
//   - MemoryMB / CPUs / PidsLimit are not enforceable; only the timeout is.
//   - Image is meaningless and ignored.
//   - Session is ignored: the host filesystem already persists across calls, so
//     a persistent tool needs no session container to keep its state.
//
// So: HostSandbox is the right substrate for an embedded or local consumer of
// this package (a CLI on a developer's machine, a single-tenant deployment
// where the agent is already trusted with the box). It is the wrong substrate
// for a hosted, multi-tenant deployment running model-authored commands, which
// is why internal/runtime keeps the shell/computer/browser tools gated on a
// real sandbox being wired.
type HostSandbox struct{}

// NewHostSandbox returns the host execution substrate. The zero value is
// equally usable; the constructor exists so call sites read symmetrically with
// NewDockerSandbox.
func NewHostSandbox() *HostSandbox { return &HostSandbox{} }

// hostDefaultTimeoutS bounds a host command that named no timeout, mirroring
// DockerSandbox's defaultTimeoutS so the substrate swap does not silently change
// how long a runaway command may run.
const hostDefaultTimeoutS = 30.0

// Exec runs one command on the host and captures its output. A non-zero exit is
// a SandboxResult, not an error — error is reserved for the host failing to run
// the command at all (binary missing, workdir unusable), matching the contract
// DockerSandbox implements.
func (h *HostSandbox) Exec(ctx context.Context, req agentcore.SandboxExec) (agentcore.SandboxResult, error) {
	if len(req.Argv) == 0 {
		return agentcore.SandboxResult{}, fmt.Errorf("sandbox: empty argv")
	}

	timeoutS := req.Constraints.TimeoutSeconds
	if timeoutS <= 0 {
		timeoutS = hostDefaultTimeoutS
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS*float64(time.Second)))
	defer cancel()

	dir, cleanup, err := hostWorkdir(req)
	if err != nil {
		return agentcore.SandboxResult{}, err
	}
	defer cleanup()

	cmd := exec.CommandContext(runCtx, req.Argv[0], req.Argv[1:]...)
	// Put the command in its own process group and kill the group on timeout.
	// os/exec's default cancel kills only the direct child, which would leave a
	// `sleep 300 &` the shell spawned running past the deadline.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.Dir = dir
	// A non-nil Env is what stops os/exec from handing the child the host
	// process environment. hostEnv always returns non-nil, including for an empty
	// req.Env — the difference between "no variables" and "all of the server's".
	cmd.Env = hostEnv(req.Env)
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := agentcore.SandboxResult{Stdout: stdout.String(), Stderr: stderr.String()}

	if runCtx.Err() == context.DeadlineExceeded {
		res.Killed = true
		res.KillReason = fmt.Sprintf("exceeded %.0fs timeout", timeoutS)
		return res, nil
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("sandbox: host exec: %w", runErr)
	}
	return res, nil
}

// hostEnv renders req.Env as a KEY=VALUE slice. It returns a non-nil slice even
// when env is empty, because os/exec treats a nil Env as "inherit the parent's"
// — the exact leak this substrate exists to avoid.
func hostEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// hostWorkdir maps req.Workdir — a path inside the sandbox's filesystem — back
// to a host directory by looking it up in req.Mounts. A Workdir with no
// matching mount, or no Workdir at all, gets a fresh temp directory: the host
// analogue of the backend's ephemeral scratch workdir, so a command that names
// no workspace cannot start in (and pollute) the server's own working
// directory. The returned cleanup removes that temp directory; it is a no-op
// for a mounted workdir.
func hostWorkdir(req agentcore.SandboxExec) (string, func(), error) {
	if w := strings.TrimSpace(req.Workdir); w != "" {
		for _, m := range req.Mounts {
			if strings.TrimSpace(m.Target) == w && strings.TrimSpace(m.Source) != "" {
				return m.Source, func() {}, nil
			}
		}
	}
	dir, err := os.MkdirTemp("", "agentray-host-")
	if err != nil {
		return "", func() {}, fmt.Errorf("sandbox: host scratch dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
