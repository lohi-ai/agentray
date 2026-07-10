# Agent Governance

AgentRay agents are safe by construction: they can only use explicit tools, those
tools can only reach data through the usecase boundary, and risky capabilities are
gated at the trust boundary before execution.

Read this before adding, wiring, or exposing any agent capability.

## Non-negotiable boundary

**Agents never touch infrastructure.** No agent, tool handler, prompt, skill, or
sub-agent may import `internal/storage`, hold a DB/NATS/Redis/ClickHouse handle,
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

Add a capability once as an operation:

1. Add the narrow method to `usecase.Repo` and implement it on `storage.Store`.
2. Declare an `opcore.Operation[I,O]` in `internal/usecase/*`.
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
| Credential vault | Resolves `{{cred:NAME}}` after trace + policy, before tool execution. Secrets stay out of model context and traces. | `agentcore.CredentialResolver`, `internal/credential` |
| Sandbox | Runs untrusted shell/file/browser-like work in an isolated container. | `agentcore.Sandbox`, `sandbox` |
| Computer-use isolation | `computer_use` is a deliberate higher-privilege tool (persistent session, network, writable, container-root) distinct from the locked `run_shell` (ephemeral, no-net, read-only, nobody). Still `--cap-drop ALL`, no-new-privileges, no host env, resource caps; granted only when explicitly selected. | `sandbox.NewComputerUseTool`, `Dockerfile.computeruse` |
| Browser-use isolation | `browser_use` drives a real browser via the `agent-browser` CLI in its **own** persistent session (browser-scoped `::browser` session id, dedicated Chromium image) — same hard isolation as computer-use (`--cap-drop ALL`, no-new-privileges, no host env, caps). The agent-browser daemon self-reaps on idle (`AGENT_BROWSER_IDLE_TIMEOUT_MS`) and `CloseSession` removes the container, so no zombie Chrome survives a conversation. Granted only when explicitly selected; optional cloakbrowser stealth is opt-in at build time. | `sandbox.NewBrowserTool`, `Dockerfile.browser` |
| HTTP tool guard | Allows controlled egress only to configured hosts; blocks SSRF and redirects. | `internal/httptool` |

Important properties:

- policy is default-deny;
- unknown credentials fail closed;
- secret values are write-only from APIs and never logged as tool traces;
- sandbox has no host env, no network by default, non-root, read-only root, resource
  caps, and timeout kill;
- `http_request` is per-agent allowlisted and re-checks resolved IPs to block
  metadata, loopback, private, and DNS-rebinding paths.

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
7. external MCP/tool-definition scanner when third-party tools arrive.

Already shipped: hardened sandbox image and credential vault.

## Source map

| Concern | File |
|---|---|
| Tool loop / trust-boundary credential resolution | `agentcore/loop.go` |
| Policy contract | `agentcore/policy.go` |
| Sandbox contract | `agentcore/sandbox.go` |
| Docker sandbox + injection guard | `sandbox/` |
| Credential vault | `internal/credential/` |
| HTTP tool + SSRF guard | `internal/httptool/` |
| Operation adapters | `internal/opcore/` |
| Usecase repo + analytics operations | `internal/usecase/` |
| Runtime tool/policy wiring | `internal/agentruntime/` |
| App config/wiring | `internal/app/`, `internal/config/` |
