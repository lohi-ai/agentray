# goal

**Seam + extension.** The gate mechanism lives here, in this package: the
completion contract, the sentinel match, the nudge, and the stall breaker are all
in `goal.go`, reaching the loop through `PromptContributor` and
`StopInterceptor`. Delete this folder and a run stops whenever the model likes.

The one half that stays in core is the goal as **durable state**: the loop writes
`EntryGoal` and recovers it on resume, then hands it back as `RunInfo.Goal`. Only
the loop may write the durable log — a plugin that persisted its own gate
condition would be a second writer to the record resume depends on. This is
deepseek-harness's split: `dsh-goal` is an event-sourced service, and
continuation is a separate consumer package (`goal-round-driver`).

Claude Code's `/goal` analog.

## Composition

```go
preset.New(agentcore.Config{Goal: "all tests pass", /* … */})   // wired for you
agentcore.Build(/* … */, goal.Until("all tests pass"))          // à la carte

plugin, trail := goal.UntilRevisable("all tests pass")          // + the update_goal tool
agentcore.Build(/* … */, plugin)
trail.Revisions()                                               // the audit trail, oldest first
```

`agentcore.New(Config)` alone records the goal but does **not** enforce it —
core cannot import this package. Use `preset.New`, or pass `goal.Until(…)` in
`Config.Extensions`.

The plugin is installed even when no goal is configured, and declines the run at
`BeginRun`. That is not defensive coding: a resumed run gets its condition from
the log, so an empty `Goal` field is not proof the run is ungated.

## Model Experience

### A goal is set

#### What the model sees

The completion contract appended to the system prompt.

#### Token effect

**Fixed** and **retained** — it is in the prefix of every provider call.

#### KV cache effect

**Prefix-stable.**

### The model finishes without a sentinel

#### What the model sees

A keep-going nudge as a synthetic user message, and the run continues.

#### Token effect

**Fixed** per nudge. Uncapped in count — but the run is still bounded by
turn/tool/budget limits.

#### KV cache effect

**Append-only.**

### The model revises the goal (`UntilRevisable` only)

#### What the model sees

An `update_goal(goal, reason)` tool. Calling it replaces the completion condition
the gate enforces, from the next turn on.

#### Token effect

**Fixed** and **retained** — one schema in the prefix of every provider call.
This is why it is opt-in: an ungated run, or a run whose objective is not in
question, should not pay it.

#### KV cache effect

**Prefix-invalidating, once per accepted revision.** The condition is in the
system prompt, so changing it rewrites the whole cached prefix. That is the real
price of a revision, and it is why an identical restatement is rejected rather
than written through.

## Impact on the agent

- The run may only stop by ending its answer with `STATUS: DONE` (goal met) or
  `STATUS: BLOCKED` (+ reason). The sentinel is matched on the **closing line
  only**, so mentioning it mid-prose does not count.
- The gate must be registered **before** other stop interceptors: the first to
  re-open the run wins, and an unmet goal makes any verification pass on that
  same answer moot. `preset.Plugins` and `internal/runtime` both encode this.
- It never wedges a run: a budget wrap-up bypasses it, and two stall breakers
  stop it as `goal_stalled` rather than burning turns to `MaxTurns` — a
  verbatim-repeated answer (immediately), or three consecutive finishes with no
  tool call in between. The second exists because the first only works on a model
  deterministic enough to repeat itself; a weaker model paraphrases its way past
  a text comparison, so progress is measured by what the model **did**, not by how
  it worded itself. A model that goes back to work between nudges resets the
  count and is never capped.
- The nudge escalates. The first explains the contract; the second and later ones
  dictate the literal line to emit and rule out a trailing sign-off after it —
  the most common way a weaker model misses a contract it is trying to follow.
  Repeating identical prose to a model that already ignored it is the one
  approach known not to work.
- The goal is persisted (`EntryGoal`) by the loop, so a resumed run is still
  gated even when the resuming caller could not re-supply the condition.
- **`UntilRevisable` transfers real authority, and should be read as one.** The
  gate exists to stop a model ending a run it has not finished; a tool that
  rewrites the gate's condition can end any run by redefining success. It is
  here because the alternative is worse in the case it is for: a long autonomous
  run discovers things, and a requirement resting on an assumption the work
  disproved leaves the agent only two honest moves — grind to `MaxTurns`, or
  declare BLOCKED and hand back nothing.

  What keeps it accountable is the record, not a restriction. Every revision
  requires a `reason`, lands in the durable log as an `EntryGoal` the moment the
  loop drains it, and stays in the store's trail beside the ones before it — so
  "the agent narrowed its goal until it could pass" is something you can see
  afterwards, in order, with the model's own justification on each step. The
  pinned user requirement is **not** touched: a revision changes what finishing
  means, never what was asked for.

  The mechanism is `GoalReviser`, an optional extension interface the loop drains
  once per turn. The loop still owns the log and the system prompt; the plugin
  only offers a pending value. Same rule as everywhere else — only the loop
  writes the record.

## Known limitations and deferred work

- **The sentinel is prose.** A model that writes `STATUS: DONE` without having
  done anything satisfies the gate; this is a protocol, not a verifier. Pair it
  with a `finishguard` for evidence.
- **One goal per run.** No sub-goals, no partial completion. `UntilRevisable`
  lets the single goal *change*; it does not let a run hold two.
- **Nothing reviews a revision while the run is live.** The trail is written for
  a human reading afterwards, and the tool's description is the only thing
  arguing against a self-serving narrowing. A composition that wants a revision
  approved before it takes effect has to build that; the seam for it is
  `GoalReviser`, which the loop drains and could gate.
- **Ordering is a convention, not a constraint.** Nothing rejects a composition
  that registers a verify guard ahead of the gate; it would merely verify
  answers the gate is about to throw away.
