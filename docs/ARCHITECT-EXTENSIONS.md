# Extending AgentRay

There are exactly two ways to add behavior to an agent, and which one applies is
decided by **who is doing the extending**, not by how big the change is.

| Extender | Mechanism | Where it lives | Runs as |
|---|---|---|---|
| A tenant / project operator | **MCP servers** + tool config + skills | a config row, and the tenant's own infrastructure | the tenant's process, their credentials |
| An AgentRay maintainer | **`agentcore.Hooks`** | Go, in this repo, compile-time | the AgentRay server |

There is deliberately no third option. AgentRay does not load tenant-authored
code, in any language, in this process. A plugin executing here would hold the
server's identity and reach every project's data, so the tenant extension
boundary is a **network** boundary instead. This is the one structural departure
from pi, whose extension system (`packages/coding-agent`, `docs/extensions.md`)
loads TypeScript from `~/.pi/agent/extensions/` with, in its own words, "your
full system permission" — correct for a single-user CLI on your own machine,
inapplicable to a hosted multi-tenant backend.

Read with:

- [ARCHITECT-AGENTGARDEN.md](ARCHITECT-AGENTGARDEN.md) — agent-as-config, the tool catalog.
- [AGENT-GOVERNANCE.md](AGENT-GOVERNANCE.md) — the trust boundary and runtime defenses.

---

## Part 1 — MCP: the tenant extension path

AgentRay is an MCP **server** (`internal/app/mcp_routes.go`) so external clients
can call our operations. It is now also an MCP **client**
(`internal/shared/mcpclient`) so an agent can call tools a project hosts itself.

```text
agent_tools row: name="mcp", config={"servers":[…]}
  → resolveRunTools (run start)
  → mcpclient.Connect  (headers credential-resolved here, host side)
  → tools/list         (one round trip per server)
  → mcp__<server>__<tool>, … added to the run's ToolSet + allow-list
```

Adding a capability this way touches no Go. It is a config row.

### Configuration

```json
{
  "servers": [
    {
      "name": "billing",
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer {{cred:BILLING_MCP_TOKEN}}" },
      "allow_tools": ["lookup_invoice"],
      "timeout_seconds": 30,
      "required": false
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `name` | Namespaces the tools this server contributes. Required, unique. |
| `url` | The Streamable HTTP endpoint. `https` unless `allow_http` is set. |
| `headers` | Sent on every request. `{{cred:NAME}}` is resolved from the agent's vault. |
| `allow_tools` | Narrows what the agent sees. It can only subtract from what the server offers. |
| `required` | `false` (default): an unreachable server is skipped and logged. `true`: it fails the run. |

### Rules that hold

- **Streamable HTTP only.** No stdio transport: that would mean spawning a
  tenant-named process on the API host, which is the exact boundary this design
  exists to keep. Responses may arrive as `application/json` or
  `text/event-stream`; both are handled.
- **The SSRF backstop applies.** MCP connections use `httptool.NewGuardedClient`,
  so an operator-supplied URL resolving to loopback, a private range, or the
  cloud-metadata address is refused at dial, and redirects are never followed —
  the same guard that makes `http_request` safe.
- **Secrets resolve on the host, at build time.** An `Authorization` header is
  operator configuration, not a model-supplied argument, so it never passes
  through the tool loop. `ToolBuildContext.Credentials` carries the vault;
  an unresolvable `{{cred:NAME}}` fails the run closed rather than shipping a
  literal placeholder as a bearer token.
- **Remote tools are external-write by construction.** We cannot see what a
  third-party server does behind its API, so `ToolExternalWrite` returns true for
  any `mcp__` name and the unattended-publish rail strips them from
  scheduled/webhook/delegate runs below autonomy `auto`.
- **Config errors fail closed; outages do not.** A malformed server list, a bad
  URL, or an unresolvable secret aborts the run — the operator asked for a
  capability we cannot honor. A third-party server that will not answer is
  skipped with a logged note, because an unrelated question should not die with
  someone else's uptime. `required: true` opts back into failing closed.
- **Write-time validation is offline.** The control plane
  (`ValidateToolConfig`) checks shape only. A correct config saves while the
  remote server is down, and while the vault holding its token is not loaded.

### Failure semantics inside a run

A remote tool that reports failure surfaces as a Go error at the agentcore
boundary, so the loop's per-run circuit breaker counts it: a server failing three
calls in a row is disabled for the rest of that run, exactly like a local tool.

---

## Part 2 — Hooks: the first-party extension path

`agentcore.Hooks` is a set of ordered, panic-hardened interceptors on the run
loop. It is compile-time Go in this repo. It is not a plugin system and must not
grow into one: it has no discovery, no loading, and no registration by name.

### The contract

| Hook | Fires | May |
|---|---|---|
| `BeforeAgentStart` | once, on the assembled system prompt + seed messages, before turn 1 | **mutate** system prompt and seed messages |
| `TurnStart` | top of every turn, before compaction/steering/the provider call | observe |
| `Context` | before every provider request, on the message view | **mutate** the outgoing view (not persisted history) |
| `BeforeProviderRequest` | on the assembled `ChatRequest` | **mutate** the request |
| `AfterProviderResponse` | on each successful provider response, before usage accumulation | observe |
| `MessageEnd` | when an assistant message is final | observe |
| `Before` (tool call) | after args validate, before execution | **block** (`Decision`) |
| `After` (tool call) | after execution | **rewrite** the result, **terminate** the run |
| `BeforeCompact` | when the transcript trips its context budget | **skip** compaction, or **replace** the transcript |
| `TurnEnd` | once per started turn, on every path out of it | observe |
| `AgentEnd` | as the run returns, on every exit path | observe |

Reducer conventions, consistent across the mutating hooks:

- `Context`: returning `nil` keeps the input.
- `BeforeProviderRequest`: returning a request with no messages means "no change".
- `BeforeAgentStart`: per field — an empty `System` keeps the assembled prompt,
  `nil` `Messages` keeps the seeds. A hook that edits one need not restate the other.
- `BeforeCompact`: the first hook returning a decisive answer (`Skip`, or non-nil
  `Messages`) wins; the rest are not consulted.

### Error policy

`Hooks.ErrorPolicy` is `HookContinue` (default) or `HookThrow`. Every handler runs
inside panic recovery; failures are attributed to a stable source string
(`before_compact[0]`, `turn_start[1]`) and reported through `Hooks.OnError`
regardless of policy. Under `HookContinue` a broken hook loses its say and the run
proceeds. The fallthrough direction is always the safe one — a panicking
`BeforeCompact` cannot suppress compaction, a panicking `Before` under `HookThrow`
blocks the call rather than letting it through.

### Things that are already true and are easy to miss

- **Turn hooks are not stream events.** `StreamTurnStart`/`StreamTurnEnd` only
  reach an attached viewer; a scheduled run has no sink and emits nothing. Turn
  hooks fire on every run, which is why metering and audit belong here rather
  than on the sink.
- **`TurnEnd` fires exactly once per started turn**, including a turn aborted by a
  provider error or a hook failure. Counting turns from `TurnEnd` is safe.
- **`BeforeAgentStart` message edits are persisted only on a fresh run.** On a
  resumed run the durable log is authoritative and the history is already
  written, so injected messages apply to that attempt only. To persist a mid-run
  injection, use the steering queue.
- **Tool override already works.** `ToolSet.Add` replaces by name, so a host can
  swap a built-in for its own implementation without a hook.
- **Argument migration already works.** A tool implementing `ArgPreparer`
  (`PrepareArguments(raw string) string`) normalizes the model's JSON before
  validation — the shim that keeps a resumed session replaying a tool whose
  schema has since changed.

### Other seams that are not hooks

These predate `Hooks` and stay separate because each has one owner, not a list:
`Config.PrepareNextTurn` (per-turn model/tools/system save-point),
`Config.StepGate` (Lab explain-mode pause), `Config.BudgetGate`,
`Config.FinishGuard` (verify-on-stop), `Config.Goal`,
`Config.GetSteeringMessages` / `GetFollowUpMessages`.

---

## Deciding which one you need

- The capability lives in someone else's system → **MCP server**.
- The capability is domain knowledge, not an API → **skill**.
- The capability is one more call against our own data → **an opcore operation**
  (see [AGENT-GOVERNANCE.md](AGENT-GOVERNANCE.md) § Extending capabilities).
- The behavior is a cross-cutting rule about how *every* run works — metering,
  redaction, a guard rail → **a hook**.
- The behavior is specific to one agent → it is config. It is not a hook, and it
  is definitely not Go.
