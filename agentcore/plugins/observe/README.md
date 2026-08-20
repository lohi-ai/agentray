# observe

**Extensions, hooks, and a provider decorator.** Ejectable — the loop never
names any of them. Without them the run behaves identically; you just cannot
see it, cannot price it, and cannot prove it is resumable.

Three things live here, and the split matters:

- **`Hooks`** watches the run's *outward* events — turns, provider calls,
  executed tool calls, the finished run.
- **`LogInvariant`** watches the run's *inward* consistency — whether everything
  the model was shown actually reached the durable log.
- **`Monitor`** watches the *provider seam* — one `TraceRecord` per LLM call
  carrying what was sent, what came back, how long it took, whether it failed,
  and what it cost. Without it `Usage.CostUSD` stays zero and nothing can say
  which call burned the budget.

## Why Monitor is a decorator and not a hook

`Hooks.BeforeProviderRequest` and `Hooks.AfterProviderResponse` already exist,
and they are the wrong shape for accounting:

- A hook fires on **one side** of the call, so it cannot measure duration.
- `AfterProviderResponse` does not fire when the call **errors**, and a failed
  call is exactly the one worth tracing.
- Streaming has no single "response" moment; usage arrives on the `Done` delta.

Monitoring has to *bracket* the call. `agentcore` exposes that bracket as
`Registry.WrapProvider` — an additive, unkeyed contribution resolved at compose
time, applied over whatever the model seam holds. `Monitor` is its first
consumer; a rate limiter or a response cache is the same shape. It decorates
**every rung** the composition can reach: the primary provider, each escalation
rung, and the compaction rung. A run that escalates past a rate limit must not
silently stop being accounted for — that is the exact case where the spend is
surprising.

## Model Experience

### Always

#### What the model sees

**Nothing.** This is the defining property of the package: an observer that
changes what the model sees is not an observer. `LogInvariant` reports and
returns; `Hooks` receives and returns; `Monitor` hands the provider a request
byte-identical to the one the loop built.

#### Token effect

**Zero-direct**, in every configuration.

#### KV cache effect

**Independent.**

## Impact on the agent

### Hooks

- Registered at `PriorityLate`, so they see the **final** decision, after every
  gate and rewrite has had its say. An observer that ran first would faithfully
  report calls that were subsequently blocked — worse than no telemetry, because
  it looks correct.
- `OnTurnStart` / `OnTurnEnd` fire on every run, streamed or not, so metering and
  audit never depend on a viewer being attached.
- `OnAgentEnd` fires on every exit path: final answer, budget stop, abort, max
  turns, error.

### LogInvariant

agentcore's durable resume is only as good as the log's completeness: a resumed
run rebuilds its history from the append-only log, so **any** message the model
saw that never reached the log makes the resumed conversation different from the
one that actually ran. The model is then asked to continue reasoning it never
did.

That failure is silent by construction. Nothing breaks at write time; the
divergence shows up much later, in a resumed run, as a model that has
inexplicably forgotten a correction it was given. It is also easy to introduce —
every feature that injects a synthetic message has to remember to persist it,
and the compiler cannot help.

- The loop reports **every** durable append (`ObserveLogged`) and **every**
  history the model is about to be shown (`ObserveMessages`). The plugin cannot
  be told about a write that did not happen, and cannot miss one that did.
- Deliberate, log-reproducible rewrites — compaction, which is bracketed in the
  log — arrive as `PhaseRebase` and reset the tracker. Without that the
  invariant would fire on exactly the transform it is supposed to permit.
- It is a **multiset** of fingerprints: a message may legitimately appear twice
  (the same tool result, the same nudge) and both occurrences must be logged.
  Role, tool name, and tool-call id are part of the identity, so two tool results
  with identical text but different call ids cannot hide behind each other.
- At most one violation is reported per check, so a systemic divergence does not
  flood the error sink.
- It **declines** a non-durable run: there is no log to diverge from, and
  reporting every message as unlogged would be noise.

Cheap enough to leave on in production — one hash per message per turn.

Adopted from deepseek-harness, which asserts the same property at runtime
("Model-visible means logged", `docs/architecture.md`).

### Monitor

- **Cost is filled in, always.** `Pricing.Cost` runs even with a nil `Sink`, so
  `Result.Usage.CostUSD` is honest whether or not anyone is collecting traces.
  Dropping the sink drops the record, not the accounting.
- **Cache tokens are priced separately.** A cache read bills at a fraction of
  fresh input and a cache write at a small premium (derived from `InputPerM`
  unless the table states them), so a long cached session does not read as if it
  paid full price for the same prefix on every turn.
- **Unknown models price at zero**, not at a guess. `Pricing` looks up exactly
  first, then by longest matching prefix, so a dated variant
  (`gpt-4o-2024-08-06`) prices off its family entry without a table edit.
- **Correlation is the consumer's.** `WithTraceID(ctx, id)` stamps an opaque id
  onto every record produced under that context. This package never learns what
  the id means — the consumer maps it back to a run.
- **A sink that panics does not kill the run.** `FileSink` swallows write
  errors, because losing a trace line is strictly better than losing the run
  that produced it.

### RunSummary

`runsummary.go` is a pure fold, not a plugin: it registers nothing, persists
nothing, and touches no run state. `FoldRun(res, records)` turns the two values
a finished run already produced — the `agentcore.RunResult` an `AgentEndHook`
receives and the `TraceRecord` stream `Monitor` emitted — into a `RunSummary`
and a `RunCoverage`. Mirrors `LogInvariant`: it reports, it does not act.

Adopted from omp's `packages/agent/src/run-collector.ts`
(`AgentRunSummary` / `AgentRunCoverage`).

- **A blocked call is counted.** The rollup folds `RunResult.Tools`, where
  `ToolTrace.Allowed`/`Reason` have carried the denial all along — so
  `ToolCounters` splits `ok / error / blocked / aborted` as a closed vocabulary,
  per run and per tool name, and `Total` is every call the MODEL ASKED FOR
  whether or not it executed. This is what closes the `After`-hook limitation
  listed below.
- **Coverage is the prompt-surface audit.** `ToolsAvailable` (union of
  `TraceRecord.Tools`) minus `ToolsInvoked` (union of `RunResult.Tools[].Tool`)
  is `ToolsUnused` — the tools a persona pays schema tokens to advertise on
  every request and never calls. A name in `ToolsInvoked` that is absent from
  `ToolsAvailable` is the other direction worth catching: the model called
  something that was never offered.
- **Delegated calls are filtered out.** A parent and its spawned children share
  one provider and one `Sink`, so `FoldRun` keeps only `Depth == 0` records.
  Without that a subagent's advertisements land in the parent's coverage.
- **Coverage is a PER-INVOCATION claim.** A resumed run's records start at the
  resume point, so its `ToolsUnused` is not the session's. Use
  `AggregateRunCoverage` to span invocations.
- **No tokens, no cost — on purpose.** See "No aggregation" below.
- **`AggregateRunSummaries` / `AggregateRunCoverage`** fold N runs into the same
  shape, so a harness repeating a task has somewhere to put the repetitions and
  a consumer needs no second rendering path.
- **`IsGhostRun`** classifies a run with a failed provider call, no tool the
  model asked for, and no billable tokens as infrastructure noise rather than a
  model failure — omp's `isGhostRun`, whose point is to keep a flaky provider
  out of the score denominator. `RunSummary.GhostRuns` carries the count, so a
  scoreable denominator is `Runs - GhostRuns`.
- **Derived and discarded.** The value is deliberately not persisted and not a
  second source of truth. Making it durable would be a separate governance
  decision, and it should be a materialized view over `agent_llm_calls`, not a
  new write on the run path.

## Composition

As a plugin, over a whole composition:

```go
agentcore.Build(append(
    preset.Plugins(cfg),
    observe.Monitor{Sink: sink},              // default price table
    // observe.Monitor{Pricing: myRates, Sink: sink}  // negotiated rates
)...)
```

As a bare decorator, for a provider built by hand — a connectivity probe, a
one-shot classifier, anything that is not an `Agent`:

```go
prov = observe.Wrap(prov, observe.DefaultPricing(), sink)
```

Both paths run the same implementation, so a call priced through the registry
and a call priced by hand produce identical records.

Sinks fan out with `MultiSink{fileSink, dbSink}`; `SinkFunc` adapts a plain
function.

## Known limitations and deferred work

- **`LogInvariant` detects, it does not repair.** A divergence is reported after
  the model has already been shown the unlogged message; the run continues.
- **Fingerprints ignore ordering.** A history whose messages are all logged but
  in a different order passes.
- **No hook for a *blocked* tool call** distinct from an executed one — the
  `After` hook runs post-gate, so a consumer wanting to meter denials must read
  the trace instead. **Closed at the rollup level:** `RunSummary` folds
  `RunResult.Tools`, not the hook stream, so denials are counted (`ToolBlocked`)
  even though no hook fires for them. The gap that remains is only the live
  per-call *event* — there is still nothing to subscribe to mid-run.
- **`Hooks` cannot observe extension activity.** An additional context injected
  by another plugin arrives as an ordinary message; there is no event saying
  which plugin produced it.
- **The price table is a snapshot.** `DefaultPricing` is hand-maintained; a
  vendor price change is a code change, and a model missing from the table
  silently prices at zero rather than warning.
- **Records hold full request messages.** A trace of a long run is large, and it
  contains whatever the transcript contained — treat a trace file as sensitive
  as the conversation itself. There is no redaction hook.
- **`FileSink` is unbounded.** No rotation, no size cap; a long-lived process
  writing JSONL will fill the disk eventually.
- **Sinks are called synchronously** on the request path. A slow sink slows the
  run. Buffer inside your own `Sink` if that matters.
- **No token or cost aggregation, and this holds.** `Monitor` emits per-call
  records; rolling tokens and spend into per-run or per-agent totals is the
  consumer's job, and agentray does it in Postgres (`agent_runs.cost_usd`,
  `token_input`, `token_output` — `internal/dataplane/store/agent_runtime.go`).
  `RunSummary` folds per-call records into per-run *counts* in process, for
  tests and the bench, and deliberately carries no `Usage` or cost fields:
  duplicating them here would create a second source of truth for numbers
  Postgres already owns. `IsGhostRun` takes the usage it needs as an argument
  for exactly that reason.
