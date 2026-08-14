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

## Impact on the agent

- The run may only stop by ending its answer with `STATUS: DONE` (goal met) or
  `STATUS: BLOCKED` (+ reason). The sentinel is matched on the **closing line
  only**, so mentioning it mid-prose does not count.
- The gate must be registered **before** other stop interceptors: the first to
  re-open the run wins, and an unmet goal makes any verification pass on that
  same answer moot. `preset.Plugins` and `internal/runtime` both encode this.
- It never wedges a run: a budget wrap-up bypasses it, and a verbatim-repeated
  answer stops the run as `goal_stalled` rather than burning turns to `MaxTurns`.
- The goal is persisted (`EntryGoal`) by the loop, so a resumed run is still
  gated even when the resuming caller could not re-supply the condition.

## Known limitations and deferred work

- **The sentinel is prose.** A model that writes `STATUS: DONE` without having
  done anything satisfies the gate; this is a protocol, not a verifier. Pair it
  with a `finishguard` for evidence.
- **One goal per run.** No sub-goals, no partial completion.
- **Ordering is a convention, not a constraint.** Nothing rejects a composition
  that registers a verify guard ahead of the gate; it would merely verify
  answers the gate is about to throw away.
