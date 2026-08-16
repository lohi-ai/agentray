# Agent Governance

AgentRay agents are safe by construction: they can only use explicit tools, those
tools can only reach data through the usecase boundary, and risky capabilities are
gated at the trust boundary before execution.

Read this before adding, wiring, or exposing any agent capability.

## Non-negotiable boundary

**Agents never touch infrastructure.** No agent, tool handler, prompt, skill, or
sub-agent may import `internal/dataplane/store`, hold a DB/NATS/Redis/ClickHouse handle,
or open its own connection to agentray data.

All product data access goes through one path:

```text
model tool call
  → opcore Tool
  → opcore.Operation.Handler
  → usecase.depsFrom(cc).Repo   (interface)
  → storage.Store               (concrete impl injected only at the edge)
```

The same operation definition projects to:

| Adapter | Consumer |
|---|---|
| in-process tool | backend agent |
| REST endpoint | web client |
| CLI command | client-side agent |
| MCP server (`POST /mcp`) | external agent (Claude Code, Codex) |

One operation means one schema, one permission name, and one usecase handler. Web,
CLI, in-house agents, and external MCP clients cannot drift.

The MCP adapter (`opcore.MountMCP`) authenticates per request via the project API
key (`X-API-Key` / `?api_key=`) and scopes every call to that project, inheriting
the same `Repo`-only data path — an external agent reaches infra through exactly
the same wall as the in-house one. A portable skill teaching an external agent to
use it ships at `.agents/skills/agentray-analytics/SKILL.md`.

## Layer ownership

| Layer | Imports infra? | Role |
|---|---:|---|
| `agentcore` | No | Generic loop, tools, policy hooks, credential/sandbox contracts. |
| `opcore` | No | Operation/tool/HTTP/CLI adapter mechanism. This is the structural wall. |
| `usecase` | Interface only | Capability handlers and the `Repo` contract. |
| `agentruntime` / `app` | Yes, edge only | Select tools, scopes, secrets, sandbox, and inject concrete storage. |

If `opcore` imports storage, or a handler bypasses `depsFrom(cc).Repo`, the design is
wrong.

## Extending capabilities

Which path applies is decided by *who* is extending — see
[ARCHITECT-EXTENSIONS.md](ARCHITECT-EXTENSIONS.md) for the full contract. A
tenant adds capabilities through MCP servers, tool config, and skills; a
maintainer adds them as operations (below) or as `agentcore.Hooks`. AgentRay
never loads tenant-authored code in this process — the tenant extension boundary
is a network boundary, not a plugin loader.

Add a first-party capability once as an operation:

1. Add the narrow method to `usecase.Repo` and implement it on `storage.Store`.
2. Declare an `opcore.Operation[I,O]` in `internal/dataplane/usecase/*`.
3. In the handler, use only `depsFrom(cc).Repo` (and approved memory deps).
4. Register the operation in the usecase registry.
5. Permit the tool name through the runtime policy/tool registry.

Required inputs must use `required:"true"`; opcore validates them before handlers
run. Put new data access behind `Repo`; never add a tool that reaches infra.

Compile-time guards keep this honest: `storage.Store` must satisfy `usecase.Repo`,
and `agentruntime` assigns its data source into the `Repo` field during build.

## Runtime defenses

These compose independently and default closed where possible:

| Defense | Purpose | Where |
|---|---|---|
| Policy gate | Agent sees/calls only allowed tools. | `agentcore.Policy`, `agentruntime/policy.go` |
| Injection guard | Blocks obvious prompt-injection payloads in tool args. | `sandbox.InjectionGuard` hook |
| Credential vault | Resolves `{{cred:NAME}}` after trace + policy, before tool execution. Secrets stay out of model context and traces. | `agentcore.CredentialResolver`, `internal/shared/credential` |
| Sandbox | Runs untrusted shell/file/browser-like work in an isolated container. | `agentcore.Sandbox`, `sandbox` |
| Computer-use isolation | `computer_use` is a deliberate higher-privilege tool (persistent session, network, writable, container-root) distinct from the locked `run_shell` (ephemeral, no-net, read-only, nobody). Still `--cap-drop ALL`, no-new-privileges, no host env, resource caps; granted only when explicitly selected. | `sandbox.NewComputerUseTool`, `Dockerfile.computeruse` |
| Browser-use isolation | `browser_use` drives a real browser via the `agent-browser` CLI in its **own** persistent session (browser-scoped `::browser` session id, dedicated Chromium image) — same hard isolation as computer-use (`--cap-drop ALL`, no-new-privileges, no host env, caps). The agent-browser daemon self-reaps on idle (`AGENT_BROWSER_IDLE_TIMEOUT_MS`) and `CloseSession` removes the container, so no zombie Chrome survives a conversation. Granted only when explicitly selected; optional cloakbrowser stealth is opt-in at build time. | `sandbox.NewBrowserTool`, `Dockerfile.browser` |
| HTTP tool guard | Allows controlled egress only to configured hosts; blocks SSRF and redirects. | `sandbox/http_tool.go` |
| MCP client boundary | Remote tools come from servers the tenant operates, reached over Streamable HTTP only (no stdio — nothing tenant-named is ever spawned on the host). Connections use the same SSRF-guarded client; server auth headers resolve `{{cred:NAME}}` host-side at build time, never through the tool loop; every `mcp__` tool counts as external-write for the unattended-publish rail. | `internal/shared/mcpclient`, `agentruntime.ToolMCP` |
| Evidence guard (verify-on-stop) | A figure-shaped final answer produced with zero executed read tools re-opens the run once: verify with a granted data tool, cite earlier-turn tool results as the figures' provenance, or restate the figures as not read from project data. Delegation counts (`spawn_subagent` is evidence — the child ran the read tools); list numbering and dates don't count as figures. Policy-only, capped at one nudge, skipped for agents with no read tools. | `finishguard.Guard`, `agentruntime/evidence_guard.go` |

Important properties:

- policy is default-deny;
- unknown credentials fail closed;
- secret values are write-only from APIs and never logged as tool traces;
- sandbox has no host env, no network by default, non-root, read-only root, resource
  caps, and timeout kill;
- `http_request` is per-agent allowlisted and re-checks resolved IPs to block
  metadata, loopback, private, and DNS-rebinding paths — including when the
  request is made from inside a container, because the egress proxy applies the
  same IP guard the host dialer does;
- every constructor in the `sandbox` package takes an `agentcore.Sandbox` and
  tolerates `nil`, which runs the tool on the host machine instead. `HostSandbox`
  keeps what a plain process can keep of the contract — only the declared env is
  visible, the workspace is the working directory, the timeout hard-kills — and
  enforces **none** of the filesystem, network or resource caps in the table
  above. The isolation rows describe the sandboxed path only.
  `config.SandboxRequired` decides which path a deployment gets, and it defaults
  to `config.Hosted`: a single-operator self-host runs `run_shell` on the machine
  its owner already trusts rather than demanding Docker before the first agent
  works, while **a hosted, multi-tenant deployment still never gains a host shell
  by omission** — the default follows the deployment shape, so no one has to
  remember a second env var for the install where forgetting it matters. It also
  fails closed the other way: an operator who set `AGENTRAY_SANDBOX_ENABLED` and
  whose Docker turned out to be unreachable gets the tools **withheld**, not
  moved to the host (`internal/app/app.go`), because "someone forgot to wire
  Docker" and "this operator chose host execution" must not be the same outcome.

## AgentGarden and teams

AgentGarden creates/manages/tests agents as data: definitions, skills, tools,
secrets, triggers, and task tiers. Adding an agent should not require an AgentRay Go
PR.

Team orchestration is deferred. A team will still use the same boundary: the lead
agent and member agents delegate by tool calls; sub-agents inherit and may only
narrow permissions.

## Fleet roadmap

Deferred fleet controls, in likely build order:

1. tamper-evident audit chain over run/tool traces;
2. kill switch by agent/project/fleet;
3. protocol facets for policy decisions beyond tool name;
4. signed per-agent identity;
5. ring/resource tiers;
6. trust scoring;
7. external MCP/tool-definition scanner. Third-party tools have now arrived (the
   `mcp` catalog entry), and a remote server's advertised names, descriptions,
   and schemas enter the model's context unreviewed — a tool description is a
   prompt-injection surface. The transport is already fenced (SSRF guard, no
   stdio, external-write rail); what is missing is inspecting what a server
   *says*.

Already shipped: hardened sandbox image and credential vault.

## Source map

| Concern | File |
|---|---|
| Structural rules, enforced as tests (kernel is a leaf, names no plugin, holds only `plugins/`; plugins never name each other) | `agentcore/boundary_test.go` |
| Extension-point contract (what a plugin may do) | `agentcore/extension.go` |
| Plugin composition (seams, priorities, unload) | `agentcore/plugin.go`, `agentcore/compose.go` |
| Plugin packages (one folder + README per capability) | `agentcore/plugins/*` — see its README |
| Default composition + parity + eject tests | `agentcore/plugins/preset/` |
| Composition diagnostics | `Registry.Describe()`, `Agent.Describe()` (`agentcore/describe.go`) |
| Loop seam (swappable control flow) | `agentcore/driver.go` |
| Turn loop (reason → act, batches, durable writes) | `agentcore/loop.go` |
| Tool trust boundary (gate, validation, credential resolution, bounding) | `agentcore/tooldispatch.go` |
| Model call (same-rung retry, then ladder escalation) | `agentcore/turn.go` |
| Child-agent construction (scope may only narrow) | `agentcore/fork.go` |
| Delegation depth (survives crossing agents) | `agentcore/delegation.go` |
| Compaction strategy contract (replaceable) | `agentcore/compactor.go` |
| Goal as durable state (write + resume recovery) | `agentcore/goal.go` |
| Goal gate policy (contract, sentinel, nudge, stall) | `agentcore/plugins/goal/` |
| Policy contract (`Policy`, `Decision`, `DenyAll`, `AllowList`) | `agentcore/permission.go` |
| Oversized tool output (spill + `read_spill`) | `agentcore/plugins/spill/` |
| Background jobs (`job_*` tools, run-fenced) | `agentcore/plugins/jobs/` |
| Session retrieval (`session_query`) | `agentcore/plugins/sessionquery/` |
| Repeated-tool-call reminder | `agentcore/plugins/repeatguard/` |
| Verify-on-stop (evidence guard) | `agentcore/plugins/finishguard/` |
| Delegation (`spawn_subagent`) | `agentcore/plugins/subagent/` |
| "Model-visible means logged" invariant | `agentcore/plugins/observe/` |
| Sandbox contract | `agentcore/sandbox.go` |
| Docker sandbox + injection guard | `sandbox/` |
| Credential vault | `internal/shared/credential/` |
| HTTP tool + SSRF guard | `sandbox/httpguard.go` + `sandbox/http_tool.go` |
| MCP client (remote tools) | `internal/shared/mcpclient/` |
| Lifecycle hooks (first-party extension seam) | `agentcore/hooks.go` |
| Operation adapters | `internal/shared/opcore/` |
| Usecase repo + analytics operations | `internal/dataplane/usecase/` |
| Runtime tool/policy wiring | `internal/runtime/` |
| App config/wiring | `internal/app/`, `internal/shared/config/` |
