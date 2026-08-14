# hooks

**Adapter.** `hooks.Of(h)` carries the caller's own `agentcore.Hooks` to
`Registry.AddHooks` — additive and unkeyed, so a composition may register
several (`hooks:audit`, `hooks:terminate`).

The listener types themselves and their dispatch are the kernel's
([`hooks.go`](../../hooks.go)): before/after tool call, context assembly, turn
boundaries, message end, provider response, agent end. This package writes no
listener, and deleting it removes none — `AddHooks` is exported on the
`Registry`. See [Adapters](../README.md#adapters).

Hooks are the **first-party** interception point — for AgentRay maintainers, not
tenants. AgentRay never loads tenant-authored code in-process; the tenant
extension boundary is a network boundary (MCP), not a plugin loader. See
`docs/ARCHITECT-EXTENSIONS.md`.

## Model Experience

### A `Context` hook contributes text

#### What the model sees

Whatever the hook returns, added to the assembled context.

#### Token effect

**Fixed per turn** and **retained** — a context hook that returns a paragraph
every turn is paying for it every turn.

#### KV cache effect

**Prefix-stable** if the hook's output is stable; **replacing** if it changes
each turn (a timestamp, a live counter), which quietly destroys prefix caching
for the whole run.

### An `After` hook rewrites a tool result

#### What the model sees

The rewritten result.

#### Token effect

**Replaced.**

## Impact on the agent

- Priority ordering is explicit: `PriorityGate` < `PriorityDefault` <
  `PriorityLate`. Ties break by registration order, so behavior never depends on
  how the plugin list happened to be sorted.
- A `Before` hook can **deny** a call — it runs in the same chain as the
  permission gate, at a lower priority.
- Hooks are panic-hardened: a panicking hook degrades to "no opinion" and is
  attributed through `OnError` rather than taking the run down.
- An `After` hook can **terminate** the run (a terminal tool).

## Known limitations and deferred work

- **A hook cannot inject a message.** Only an extension can (`AdditionalContexts`),
  because the loop owns persisting it — that is what keeps "model-visible means
  logged" enforceable.
- **No hook sees another hook's decision**, so two hooks with overlapping
  concerns can only be ordered, not composed.
