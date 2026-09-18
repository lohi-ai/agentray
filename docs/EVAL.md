# Persistent Python and JavaScript eval

AgentRay's selectable `eval` tool executes one Python or JavaScript cell per call
and retains a separate language process for later calls in the same logical
conversation. Variables, imports, functions, objects, and asynchronous runtime
state survive between cells. The agent can run incremental analysis without
rebuilding state through repeated one-shot shell commands.

Python uses a self-contained standard-library runner. JavaScript uses Node's
REPL evaluator for persistent lexical bindings and native top-level await, with
an AgentRay-owned framed protocol, TypeScript cell transform, static-import
rewrite, and `display(value)` bridge. Both emit rich JSON and PNG/JPEG images
through the same provider-neutral result path. Cells can call the run's
registered host tools through a capability-scoped bridge; background cell/job
handles remain explicit follow-up work.

## Configuration

The empty object is the laptop default:

```json
{}
```

It resolves `python3` and `node` from the restricted child `PATH`, allows cells
to run for up to 30 seconds, and returns at most 50 KiB of output. An explicit
portable server configuration looks like:

```json
{
  "python": {
    "command": "python3",
    "args": [],
    "image": "agentray-eval:latest"
  },
  "javascript": {
    "command": "node",
    "args": [],
    "image": "agentray-eval:latest"
  },
  "timeout_seconds": 60,
  "max_output_bytes": 102400,
  "max_bridge_calls": 16
}
```

Build the supplied combined image with `make sandbox-build-eval` (or
`docker build -f Dockerfile.eval -t agentray-eval:latest .`). `image` is ignored
in trusted local host mode and selected by Docker mode. A custom image must
already contain the configured executable; the tool never downloads a runtime
or package. `timeout_seconds` is both the default and the maximum the model may
request for one cell (1–300 seconds).
`max_output_bytes` accepts 4 KiB–1 MiB and retains a bounded head and tail.
`max_bridge_calls` accepts 1–64 (default 16) and bounds host-tool requests from
one cell independently of the run-wide tool-execution budget.

Plain JavaScript works on older maintained Node releases. TypeScript cell syntax
requires Node 22.13 or newer. The runner uses Node's built-in transform, so it
adds no npm or Bun dependency. Transforming
imported `.ts`/`.mts` modules additionally requires Node 22.15 or newer; the
supplied image pins Node 22.23.2.

The model-facing call is one cell:

```json
{
  "language": "python",
  "code": "import statistics\nvalues = [1, 2, 10]\nstatistics.mean(values)"
}
```

Later calls in the conversation may reference `values`. Set `reset: true` to
discard only that language's retained kernel before executing the call.
JavaScript uses `language: "javascript"` and supports persistent `let`/`const`,
`require()`, static and dynamic `import`, top-level `await`, final-expression
display, and `display(value)`. Static imports—including default, named,
namespace, side-effect, and import-attribute forms—are rewritten to awaited
dynamic imports that resolve from the workspace. Imported bindings persist like
other top-level bindings.

TypeScript annotations, interfaces, type-only imports, generics, `satisfies`,
enums, namespaces, and parameter properties are transformed before evaluation.
This is execution convenience, not type checking. TSX is not supported, and the
module hook treats explicitly imported `.ts` and `.mts` files as ESM, recursively
transforming their TypeScript before execution. Use explicit file extensions.
TSX and CommonJS `.cts` modules are not transformed. Python interactive
`input()` is rejected.

## Governed host-tool bridge

Inside an eval call made by a live Agent, JavaScript can use either form:

```javascript
const text = await tool.read_file({ path: "README.md" });
const same = await tool("read_file", { path: "README.md" });
const [a, b] = await Promise.all([
  tool.grep({ pattern: "TODO" }),
  tool.glob({ pattern: "**/*.go" }),
]);
```

Python exposes the synchronous equivalent:

```python
text = tool("read_file", {"path": "README.md"})
```

This is a capability owned by the current Agent run, not a raw registry lookup.
Every bridged call re-enters the normal dispatch boundary: argument preparation
and schema validation, default-deny policy and hooks, credential resolution,
result interception/bounding, idempotency, cancellation, circuit breaking, and
tracing all still apply. Calls share the run's atomic `MaxToolCalls` budget;
parallel provider batches and JavaScript `Promise.all` cannot race past it.
Recursive calls to an already-active tool are rejected, and a nested tool may
not park the run for human input. Calling `EvalTool` directly outside an Agent
does not grant bridge authority and returns an explicit unavailable error.

Nested calls are recorded in the outer eval outcome and in `RunResult.Tools`,
with paired stream lifecycle events, but do not become fake provider-authored
tool messages. This preserves function-call/result adjacency on every provider
wire and makes the same audit record durable across resume. A registered and
permitted `spawn_subagent` can therefore be awaited like any other host tool,
without giving the cell broader tools or policy than its parent.

JavaScript host calls may run concurrently; their audit records are folded in
request order. Python calls are synchronous. A JavaScript cell drains every
bridge request it started before completing, including an unawaited immediate
call, and turns floating promise rejections into cell errors instead of letting
them crash the retained Node kernel. The cell timeout includes time spent in
host tools; cancellation is propagated and the kernel is discarded without
replay on timeout. AgentRay does not yet expose oh-my-pi-style background
handles, timeout pausing around host tools, or speculative shadow cells.

## Rich display contract

Python `display(value)` and final expressions inspect `_repr_mimebundle_`,
Markdown, HTML, SVG, LaTeX, JSON, PNG, and JPEG; matplotlib figures render to
PNG automatically. JavaScript objects become structured JSON, primitives become
text, and `{type: "image", mimeType, data}` accepts base64, Buffer,
Uint8Array, ArrayBuffer, or typed-array image data. Textual forms stay in the
ordinary tool result; PNG/JPEG bytes become typed image content
parts so vision-capable OpenAI Chat/Responses, Codex, Google-compatible, and
Anthropic paths can receive them natively. A model path that explicitly lacks
image input receives an omission notice instead of silently losing the plot.
The complete outgoing request is also capped for the active provider/model;
when history exceeds that cap, the oldest images are omitted copy-on-write with
a visible breadcrumb, leaving the canonical transcript intact for a more
capable fallback rung.

One cell may attach at most eight images and 768 KiB of decoded image data;
individual protocol frames remain capped at 1 MiB. Invalid image signatures,
over-budget images, and limit exhaustion are reported in text. Compaction and
deterministic pruning count and explicitly elide image parts, while session
stores snapshot them with the transcript. The PostgreSQL session adapter stores
large base64 payloads once in its session-fenced, content-addressed artifact
table and hydrates them transparently on resume; the in-memory laptop store
keeps them inline.

At the shared rich-tool boundary, decoded images are validated and normalized
before they enter canonical history. Inputs are bounded to 16 MiB and 16
megapixels; the longest edge is capped at 1568 px, short edges below 200 px are
raised, and PNG/JPEG recompression targets 500 KiB while preserving the 768 KiB
aggregate cap. Dimension changes add a coordinate-mapping note. WebP input is
decoded but converted for local-model portability. The pipeline is pure Go and
therefore identical in trusted laptop mode and the hardened server runtime.
Raw-byte/object storage is not implemented yet, so large durable plots should
also be saved to the workspace when later retrieval matters.

## Lifecycle and failure semantics

Kernels are keyed by tenant/workspace, project, agent, logical conversation,
workspace path, language, and normalized config. Calls to one language kernel
are serialized; Python and JavaScript state/reset are independent. Different
conversations remain parallel and cannot share objects accidentally.

The in-process registry has a soft default capacity of 16 kernels, expires an
idle kernel after 15 minutes, and gives every subprocess a one-hour hard
lifetime. Active kernels are never killed merely to enforce capacity. State is
an optimization, not durable data: a laptop restart, server replica miss,
deployment, capacity eviction, or idle expiry starts a clean process. Files
written into the conversation workspace remain durable.

A normal Python or JavaScript exception is returned to the agent and definitions
or mutations completed before it remain in that language kernel, matching REPL
behavior. A cancelled, timed-out, disconnected, or malformed-protocol cell
discards the kernel. The cell is never replayed automatically because it may
already have written a file or performed another side effect.

## Laptop and server boundary

On a laptop, the operator-provisioned Python/Node process runs through
`HostSandbox`: it receives an allowlisted environment and starts in the agent
workspace, but it still has the filesystem and network authority of the
AgentRay OS account. This is trusted local execution, not isolation.

On a hosted server, require `DockerSandbox`. Each language then runs in a fresh
hardened container process with no network, dropped capabilities, resource
limits, a read-only root filesystem, writable tmpfs home, and only the agent
workspace bind-mounted writable. The same NDJSON runner and session registry
operate above both backends. JavaScript protocol frames use a dedicated
descriptor, so ordinary stdout/stderr—including inherited child-process
output—cannot be misread as control traffic.
