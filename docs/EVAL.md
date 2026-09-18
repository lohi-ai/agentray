# Persistent Python eval

AgentRay's selectable `eval` tool executes one Python cell per call and retains
the Python process for later calls in the same logical conversation. Variables,
imports, functions, objects, and the event loop survive between cells. The
agent can run incremental analysis without rebuilding state through repeated
`python -c` shell commands.

The first backend is Python 3. JavaScript, rich image displays, kernel-defined
tools, subagent bridges, and background cells remain explicit follow-up work;
the session and framed-protocol seams do not require the model-facing tool name
to change when those arrive.

## Configuration

The empty object is the laptop default:

```json
{}
```

It resolves `python3` from the restricted child `PATH`, allows cells to run for
up to 30 seconds, and returns at most 50 KiB of output. An explicit portable
configuration looks like:

```json
{
  "python": {
    "command": "python3",
    "args": [],
    "image": "your-registry/agentray-python:3.12"
  },
  "timeout_seconds": 60,
  "max_output_bytes": 102400
}
```

`image` is ignored in trusted local host mode and selected by Docker mode. The
image must already contain the configured Python executable; the tool never
downloads a runtime or package. `timeout_seconds` is both the default and the
maximum the model may request for one cell (1–300 seconds).
`max_output_bytes` accepts 4 KiB–1 MiB and retains a bounded head and tail.

The model-facing call is one cell:

```json
{
  "language": "python",
  "code": "import statistics\nvalues = [1, 2, 10]\nstatistics.mean(values)"
}
```

Later calls in the conversation may reference `values`. Set `reset: true` to
discard the retained kernel before executing that call. Top-level `await` and
`display(value)` are supported; interactive `input()` is rejected.

## Lifecycle and failure semantics

Kernels are keyed by tenant/workspace, project, agent, logical conversation,
workspace path, runtime, and normalized config. Calls to one kernel are
serialized. Different conversations remain parallel and cannot share Python
objects accidentally.

The in-process registry has a soft default capacity of 16 kernels, expires an
idle kernel after 15 minutes, and gives every subprocess a one-hour hard
lifetime. Active kernels are never killed merely to enforce capacity. State is
an optimization, not durable data: a laptop restart, server replica miss,
deployment, capacity eviction, or idle expiry starts a clean process. Files
written into the conversation workspace remain durable.

A normal Python exception is returned to the agent and definitions or mutations
completed before it remain in the kernel, matching ordinary REPL behavior. A
cancelled, timed-out, disconnected, or malformed-protocol cell discards the
kernel. The cell is never replayed automatically because it may already have
written a file or performed another side effect.

## Laptop and server boundary

On a laptop, the operator-provisioned Python process runs through
`HostSandbox`: it receives an allowlisted environment and starts in the agent
workspace, but it still has the filesystem and network authority of the
AgentRay OS account. This is trusted local execution, not isolation.

On a hosted server, require `DockerSandbox`. Eval then runs in a fresh hardened
container process with no network, dropped capabilities, resource limits, a
read-only root filesystem, writable tmpfs home, and only the agent workspace
bind-mounted writable. The same NDJSON runner and session registry operate
above both backends.
