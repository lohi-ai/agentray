# tools

**Adapter.** `tools.Of(...)` / `tools.FromSet(cfg.Tools)` carries the caller's
own `agentcore.Tool`s to `Registry.AddTools` — additive and unkeyed, so a
composition may register several (`tools:catalog`, `tools:http`).

It contributes no tool of its own. Deleting this package removes no capability:
`AddTools` is exported on the kernel's `Registry`, so a caller can reach it from
any three-line plugin. An agent that "can reason and answer but not act" is what
you get from passing **no tools**, which is a configuration choice — not from
the absence of this folder. See [Adapters](../README.md#adapters).

## Model Experience

### Every request

#### What the model sees

The tool schemas: name, description, and JSON-Schema parameters for every tool
the policy allows.

#### Token effect

**Fixed** and **retained** — schemas ride in every provider call. A large tool
surface is a permanent tax on every turn, and the single most common cause of an
agent whose prefix is mostly boilerplate. Granting fewer tools is usually
cheaper *and* more accurate than granting more.

#### KV cache effect

**Prefix-stable**, provided the set does not change mid-run. `PrepareNextTurn`
swapping the tool set invalidates the cache from that point.

### Description text is model-visible

A tool's `Description` is prose the model reads and acts on. For a **remote**
(MCP) tool, that prose is authored by whoever operates the server — which makes
it a prompt-injection surface. See `docs/AGENT-GOVERNANCE.md`.

## Impact on the agent

- Tool **order** is the order the model is shown, so it is never sorted.
- A tool declaring `Parallel()` opts its calls into concurrent batch execution.
- A tool declaring `ArgPreparer` normalizes arguments **before** validation and
  before the gate sees them — the gated form is what gets traced.
- A tool declaring `RetrySafeCall` controls whether a dangling call is replayed
  after a crash-resume, per call rather than per tool.

## Known limitations and deferred work

- **No schema linting.** A malformed or hostile description reaches the model
  unreviewed; the deferred MCP scanner is the planned answer.
- **No per-tool token budget.** A tool whose schema is enormous costs every turn
  and nothing warns.
