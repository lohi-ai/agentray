# agentcore plugins

One folder per capability — plus three [adapters](#adapters), which are not
capabilities and are listed separately so they cannot be mistaken for one. Each
folder has a `README.md` explaining what it does to the agent — what the model
sees, what it costs in tokens, what it does to the provider's prefix cache, and
what it cannot do.

## The rule

> **The loop names no plugin.** Delete every package in this directory and
> `agentcore` still compiles and runs — it just does less.

Everything below the loop reaches it through the generic interfaces in
[`agentcore/extension.go`](../extension.go). The loop dispatches to
`ToolInterceptor`, `BatchInterceptor`, `StepInterceptor`, `StopInterceptor`,
`RunObserver`, `LogObserver`, `ToolContributor`, `PromptContributor`,
`ContextContributor`, `RunCloser`, `SelfGated`, `BookkeepingTool` — never to
`spill`, `jobs`, `todo`, or `subagent`.

Plugins do not name each other either. Where one capability needs to know
something about another — the repeat guard must not count a plan tool's
legitimate repeats — the question is asked generically (`RunInfo.Bookkeeping`),
never by importing the other package.

Adding a capability is **a new folder**. It is never an edit to `agentcore`, and
never an edit to another plugin.

## Three kinds of plugin

They are not the same thing, and calling all three "plugin" without saying which
is how a plugin system quietly becomes a monolith with folders. The kind is
decided by which registry call the plugin makes, so you can read it off the
`Register` body in one line.

### Seams — configure something the loop always does

`r.SetX(...)`. Keyed.

A seam has **exactly one provider**. A second claim is a build error naming both
plugins, never a silent overwrite. These are not ejectable in any meaningful
sense: an agent with no provider cannot reason. What they buy is
**replaceability**.

(The driver is a seam too, but has no folder: `Build` installs
`agentcore.DefaultDriver` when nothing claims it, and a composition that wants
different control flow registers a plugin whose `Register` calls
`r.SetDriver` — keeping governance, durability, and observation unchanged.)

| Folder | Seam | Ejecting it means |
|---|---|---|
| [`model`](model/) | provider, ladder, retry, caching | no reasoning |
| [`definition`](definition/) | persona, skills, limits, env | no identity |
| [`memory`](memory/) | cross-run recall | forgets between runs |
| [`session`](session/) | durable log, resume | a crash loses the run |
| [`compaction`](compaction/) | context summarization | long runs hit the ceiling |
| [`budget`](budget/) | spend ceiling, step gate | unbounded spend |
| [`steering`](steering/) | steer / follow-up / save-point | no mid-run correction |
| [`sandbox`](sandbox/) | isolation substrate + argument guard, secret vault | no untrusted code, no secrets |

The permission gate is a seam too, and the most important one — but the folder
that fills it holds no policy logic, so it is listed under
[Adapters](#adapters) instead. Governance itself is the kernel's:
`permission.go` owns `Policy`, `Decision`, `DenyAll` and `NewAllowList`, and
`newRegistry` seeds `policy: DenyAll{}`.

Two are hybrids, and both for the same reason — a recorded decision and its
enforcement have to be installed together or the composition lies about itself:

- [`goal`](goal/) claims the seam (the loop persists the condition to the durable
  log and recovers it on resume) **and** adds the gate as an extension. The state
  is core's because only the loop may write the log; the policy — contract,
  sentinel, nudge, stall breaker — is entirely in the plugin.
- [`sandbox`](sandbox/) claims the substrate seam **and** installs the guard that
  reads what is sent into it, at `PriorityGate`. A backend wired with nothing
  inspecting its arguments is a shell with no one watching, and the forgetting
  would be silent.

`sandbox` and `credentials` are two seams inside one `Env`, folded in at build
time rather than merged at registration — which is what lets `definition`
install a whole `Env` and `sandbox` install one capability of it without either
clobbering the other, whichever was listed first.

`compaction` is a third shape: the seam is not ejectable, but its *strategy* is
replaceable through `agentcore.Compactor`, so a pruner that makes no model call
is a sibling package rather than an edit to the loop.

A contribution is not always a tool or a hook. `WrapProvider` contributes a
decorator around every model call the run can make — the one shape that can
*bracket* a call, and therefore the only way to measure its duration or see it
fail. `observe.Monitor` uses it; a rate limiter or a response cache would too.

### Contributions — add to the composition

`r.AddTools(...)` / `r.AddHooks(...)` / `r.WrapProvider(...)`. Additive,
unkeyed, and resolved **once at compose time** — they carry no per-run state, so
they need no lifecycle. This is the cheapest kind of plugin, and the right one
whenever a capability is fully expressed by "here is a tool", "here is a
listener", or "here is a decorator around every model call".

| Folder | Contributes | Ejecting it means |
|---|---|---|
| [`todo`](todo/) | `update_plan` + the pinned live plan | a long run drifts off task |
| [`observe`](observe/) `Hooks` | telemetry at `PriorityLate` | the run is unobservable |
| [`observe`](observe/) `Monitor` | per-call trace + cost, on every rung | spend is unattributable |

### Extensions — add something the loop does not do

`r.AddExtension(...)`. Additive and **unkeyed**: several may intercept the same
point, and they compose in registration order. Unlike a contribution an
extension is **run-scoped** — the loop instantiates it per run via `BeginRun`,
which is what lets it hold state (a repeat chain, a nudge budget) with no
locking and no cross-run leakage, and lets it decline a run outright. A
capability is not a slot — two interceptors bounding a tool result is a
waterfall, not a conflict.

These are the ejectable ones. The loop holds them behind
`[]agentcore.ExtensionFactory` and discovers what each can do by **type
assertion** at run start, so the set of things a plugin may do is open and adding
a new kind does not touch core.

| Folder | Adds | Ejecting it means |
|---|---|---|
| [`spill`](spill/) | lossless bounding + `read_spill` | oversized results are truncated for good |
| [`jobs`](jobs/) | async tools + `job_*` | every tool blocks the run |
| [`sessionquery`](sessionquery/) | `session_query` over the log | compaction is lossy in practice |
| [`repeatguard`](repeatguard/) | loop-detection reminder | a repeat loop burns the turn budget |
| [`finishguard`](finishguard/) | verify-on-stop | the first answer is the answer |
| [`goal`](goal/) | completion contract (`STATUS: DONE`) | runs stop when they like |
| [`subagent`](subagent/) | `spawn_subagent` | the agent is solo |
| [`observe`](observe/) `LogInvariant` | proves model-visible ⊆ logged | resume corruption goes unnoticed |

## Adapters

Three folders are **not capabilities**. They hold no logic, own no state, and
make no decision: each carries a value the caller already has to a setter the
kernel already has.

| Folder | Carries | `Register` body |
|---|---|---|
| [`tools`](tools/) | the caller's `agentcore.Tool`s | `r.AddTools(p.Tools...)` |
| [`hooks`](hooks/) | the caller's `agentcore.Hooks` | `r.AddHooks(p.Priority, p.Hooks)` |
| [`policy`](policy/) | the caller's `agentcore.Policy` | `r.UsePolicy(p.Policy)` |

They exist because `agentcore.Build(plugins...)` is the only composition entry
point, so every exported setter needs *some* `Plugin` to call it. Deleting one
costs a caller a three-line inline plugin — which is what `agentcore`'s own
`plugin_test.go` writes — never a capability.

**Do not give them an "ejecting it means" row.** Every symbol they touch is the
kernel's, so removing the *package* removes nothing; only passing an empty value
does, and that is a configuration choice, not a composition one.
[`extension.go`](../extension.go) names this distinction as the thing the whole
mechanism rests on — *"that is the difference between a plugin and a
configuration flag"* — so the line has to be drawn here or it is not drawn
anywhere.

Two consequences worth stating plainly, because the earlier version of this file
got both wrong:

- **Ejecting `policy` does not mean default-allow.** The kernel defaults to
  `DenyAll` (`newRegistry`, `plugin.go`), proven by
  `preset_test.go`'s `TestPolicyDefaultsToDenyAll`: an agent built with no policy
  plugin still describes as `DenyAll`. A composition that forgets governance is
  not ungoverned.
- **The gate hook is installed by the kernel, not by the plugin.**
  `Registry.UsePolicy` ([`compose.go`](../compose.go)) claims the seam *and*
  contributes the `PriorityGate` hook in one call. `plugins/policy` forwards to
  it. The "a policy nobody consults is not a gate" guarantee is real, but it is
  enforced by the setter, which is why no plugin can register a policy without
  it.

`tools` and `hooks` carry a `Label` field precisely because a composition has
**several** of each, naming themselves `tools:catalog`, `hooks:audit` — the
other tell that they are not "one folder per capability". (`policy` has no
`Label`: it forwards to a keyed seam, so a second one is a build error naming
both plugins.)

## preset

[`preset`](preset/) is none of the above: it composes the rest back into
agentcore's default agent. `preset.Plugins(cfg)` is pinned to
`agentcore.New(cfg)` parity;
`preset.Full(cfg, opts)` is that list plus the capabilities `Config` has no field
for — `spill`, `jobs`, `repeatguard`, `sessionquery`, and both `observe`
plugins — and is what a deployment actually composes.

## Four things the mechanism guarantees

**Replaceability.** A seam has one provider and the plugin that registers it says
so. `Registry.Describe()` prints which plugin owns which seam; `Agent.Describe()`
prints what an agent actually ended up configured with — for "what is *actually*
running?".

**Reversibility.** Registrations are effects owned by the plugin that made them,
so `Registry.Unload(name)` unwinds one completely: seam released, hooks
withdrawn, extension removed, previous provider restored.

**Ordering is explicit, not positional.** Hooks carry a `Priority`
(`PriorityGate` < `PriorityDefault` < `PriorityLate`), so the permission gate is
consulted before any consumer hook regardless of where `policy` sits in the list.
The governance guarantee is a property of the composition, not a side effect of
construction order.

**The core owns persistence.** An extension returns `AdditionalContexts`; the
**loop** appends them (after every tool result in the batch, never interleaved —
that would break tool-call/result adjacency) and writes them to the durable log.
No plugin touches `appendEntry`. That is what makes "model-visible means logged"
structurally true rather than a convention every new injector has to remember.

**One interface, uniformly.** Every plugin in every table above is a value with
the same two methods (`Name`, `Register`); every extension adds the same
`BeginRun`. `preset_test.go`'s `TestEveryPluginSharesOneInterface` holds them all
in one `[]agentcore.Plugin` and drives each through `BeginRun`, so a capability
that ever needed a bespoke entry point could not be listed there.
`TestSeamsAreNotExtensions` guards the reverse — a seam plugin must not also
pretend to be ejectable.

## Declining a run is not an error

`BeginRun` returning a nil `Extension` means "not this run" — delegation already
at max depth, no store to spill into, no durable log to query. The tool is then
never advertised, which is deliberate: a tool that is offered and can only ever
refuse is something the model must read, reason about, and work around.

## Writing one

```go
package mycap

type Plugin struct{ /* config */ }

func (Plugin) Name() string { return "my_cap" }

func (p Plugin) Register(r *agentcore.Registry) error { r.AddExtension(p); return nil }

func (p Plugin) BeginRun(ctx context.Context, info agentcore.RunInfo) (agentcore.Extension, error) {
	if !p.canServe(info) {
		return nil, nil // decline; not an error
	}
	return &capRun{ /* per-run state, no locking needed */ }, nil
}
```

Then implement whichever optional interfaces apply. Write the README in the
format the others use — model experience, token effect, KV cache effect, known
limitations — because the honest section is the last one.

## What is deliberately not copied from deepseek-harness / Cordis

The dependency-injection framework. No reflection, no service container, no
lifecycle graph. A plugin is a value with a `Register` method, a seam is a typed
field, an extension point is a Go interface, and composition is a function call.
