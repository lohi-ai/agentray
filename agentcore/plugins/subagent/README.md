# subagent

**Extension.** Ejectable — the loop never names it. Without it the agent is
solo: every step of every sub-task happens in its own context window.

What delegation buys the parent is **context, not compute**. A child explores in
its own isolated history and returns only its final answer, so the tool churn,
dead ends, and large intermediate results never enter the parent's window.

## Model Experience

### Delegation is available (run below `MaxDepth`)

#### What the model sees

One tool, `spawn_subagent`, with `task` and `context` parameters — plus an
`agent` parameter listing named teammates when a roster is configured.

##### Verbatim text for this field

```
Delegate one self-contained task to an ephemeral sub-agent and get back only its
final answer. The sub-agent has the same tools and permissions as you but a
fresh, isolated context — its intermediate work never enters yours. Use it for
exploration or noisy multi-step work whose details you don't need (research a
question, scan data broadly, produce an artifact), NOT for quick single-tool
lookups you can do yourself. State the task fully and self-contained: the
sub-agent sees nothing of this conversation except what you put in task and
context.
```

#### Token effect

**Replaced, and strongly net-negative.** The child's entire run — every tool
call, every intermediate result — is replaced in the parent's context by one
answer capped at `MaxOutputBytes` (default 48 KB). This is the only capability
here that reliably *reduces* total context pressure.

#### KV cache effect

**Append-only** for the parent. The child runs against its own prefix entirely.

### The run is already at `MaxDepth`

#### What the model sees

Nothing. The plugin **declines the run**, so the tool is never advertised.

Enforcing the cap by absence rather than by refusal matters: a tool that is
offered and then always refuses is something the model must read, reason about,
and work around.

#### Token effect

**Zero-direct.**

## Impact on the agent

- The tool **is** gated. Unlike `read_skill` / `read_spill` / `job_*` /
  `session_query`, spawning reaches capability the agent has not already
  exercised, so the consumer must permit `spawn_subagent` in its policy.
- A child is built by `Agent.Fork`, which lives in core precisely so scope can
  only **narrow**: the child inherits every capability-bearing field verbatim —
  provider, ladder, tools, policy (including the installed permission gate),
  memory, definition, limits, env, compaction, retry, caching, extensions — and
  drops the run-control seams (durable session by default, steering, follow-up,
  step gate, `PrepareNextTurn`). A child is one bounded task, not a
  conversation.
- Depth rides the **context**, not a field, so an A → B → A cycle is bounded by
  the same `MaxDepth` as a straight chain, even when the two agents are composed
  with different plugin sets.
- A durable parent gives the child a durable session at
  `parentSession + "/" + toolCallID`. That determinism is what makes
  `spawn_subagent` safe to declare retry-safe: a replayed spawn **reattaches** —
  a completed child returns its recorded answer without re-running (no duplicate
  spend or side effects), an interrupted one resumes from its own log.
- A **delegate**-routed spawn is not retry-safe. `Delegate.Run` is an opaque
  closure under the target agent's identity with no reattach wiring, so
  replaying it would re-run the teammate's entire task; those calls are left
  dangling for the model to decide.
- Child usage — including a **failed** child's — is folded into the parent via
  `AddChildUsage`, so the parent's budget gate accounts for what its children
  spent.
- Spawns are `Parallel`, so a fan-out turn ("spawn three, then synthesize") runs
  them concurrently.

## Known limitations and deferred work

- **The parent sees only the final answer.** There is no structured return, no
  partial results, and no way for a child to hand back an artifact reference
  instead of prose.
- **No per-child budget.** `MaxPerRun` counts spawns, not tokens or cost; one
  expensive child can consume the whole run's budget.
- **A child cannot be steered.** No steering queue, no step gate — once started
  it runs to completion or cancellation.
- **`Delegate` is fully opaque.** agentcore cannot verify that a delegate's
  scope is narrower than the caller's; that guarantee is the consumer's.
- **Depth is the only recursion bound.** There is no detection of a semantic
  cycle below `MaxDepth` (A asks B the same question A was asked).
