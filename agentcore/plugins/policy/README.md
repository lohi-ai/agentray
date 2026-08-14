# policy

**Adapter.** This package carries an `agentcore.Policy` to
`Registry.UsePolicy` and does nothing else — `Policy`, `Decision`, `DenyAll`
and `NewAllowList` are all kernel types, and `UsePolicy` (in the kernel's
`compose.go`) is what claims the seam *and* installs the `PriorityGate` hook in
one call, so a policy can never be registered without the gate that consults it.

**Deleting this package removes no governance.** `newRegistry` seeds
`policy: DenyAll{}`, so an agent composed with no policy plugin denies every
tool call — asserted by `preset_test.go`'s `TestPolicyDefaultsToDenyAll`. A
composition that forgets governance is not ungoverned; it is inert.

The rest of this file describes the **gate itself**, which is the kernel's
(`permission.go`), not this folder's.

## Model Experience

### A denied call

#### What the model sees

```
blocked: <reason>
```

as the tool result. The call is never executed and the tool never sees the
arguments.

#### Token effect

**Fixed** and small, but it recurs: a model that keeps calling a blocked tool
pays for every refusal. That interaction is exactly what `repeatguard` is
counting.

#### KV cache effect

**Append-only.**

### An allowed call

#### What the model sees

Nothing from this plugin.

#### Token effect

**Zero-direct.**

## Impact on the agent

- The gate is installed as a hook at **`PriorityGate`**, so it is consulted
  before any consumer hook regardless of where the policy plugin sits in the
  list. That is the whole point of prioritized hooks: the governance guarantee
  became a property of the composition rather than a side effect of construction
  order.
- Policy is **default-deny** (`NewAllowList`).
- Credential resolution happens **after** the gate, so a blocked call never
  resolves a secret.
- Two built-in tools bypass the gate — `read_skill` (core) and anything an
  extension declares `SelfGated` — because none can reach outside the run. That
  claim lives next to the capability that makes it true, and is auditable.

## Known limitations and deferred work

- **Name-based only.** A decision cannot depend on arguments, on the caller's
  history, or on a resource identity. Protocol facets are on the fleet roadmap.
- **No per-call rate limiting.** The gate says yes or no, not "not this often".
