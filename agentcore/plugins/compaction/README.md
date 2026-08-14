# compaction

**Seam, with a replaceable strategy.** Not ejectable — the loop always considers
compaction, because a run that silently blew past the context window would fail
in the provider rather than in the agent. What IS replaceable is *what*
compaction does.

Two decisions, split:

| Decision | Owner |
|---|---|
| WHEN to consider it, and the durable bracket around it | the loop |
| WHICH span is old enough to lose, what replaces it, what it costs | `agentcore.Compactor` |

`agentcore.DefaultCompactor` is the built-in strategy: summarize the older span
into a structured checkpoint with a model call, degrading to a deterministic
elide when the call fails. Installing a different one is a sibling package and a
one-line composition change:

```go
compaction.Using(myPruner{})            // drops stale tool results, no model call
compaction.Plugin{Settings: &s, Strategy: myPruner{}}
```

This is deepseek-harness's Service Definition / Service Provider split
(`dsh-compaction` + `dsh-compaction-basic` + `dsh-compaction-tool-result-pruner`).
The reason the default lives in `agentcore/compaction.go` rather than here is
Go's import graph: `Config` must produce a working agent without importing a
plugin package, so the default provider ships with the definition — the same
arrangement as `Driver` / `DefaultDriver`.

`plugins/compaction/compaction_test.go` builds a second strategy in the test file
and proves it runs with no core edit, and that two strategies in one composition
is a build error naming both.

## Model Experience

### The transcript approaches the context limit

#### What the model sees

The older span of the conversation replaced by a summary, with the most recent
`KeepRecentTokens` worth of messages intact.

#### Token effect

**Replaced**, and that is the point: an unbounded history becomes a bounded one.
The cost is a summary call on a possibly-cheaper rung.

#### KV cache effect

**Replacing** — this is the single most cache-destructive event in a run. The
prefix is rewritten, so every provider cache entry after the compaction point is
invalidated and the next turn pays full price. Compacting *often* is expensive
for reasons that have nothing to do with the summary call.

## Impact on the agent

- Compaction is **lossy on purpose**. The detail is gone from context but still
  intact in the durable log — which is exactly what `sessionquery` exists to
  read back. Composing the two is what makes aggressive compaction safe;
  compaction alone is not.
- The bracket is written to the durable log, so a resumed run reproduces the same
  rewrite and `observe.LogInvariant` treats it as a legitimate rebase rather than
  a divergence.
- `CompactionProvider` + `CompactionModel`, when both set, pin the summary call
  to a dedicated (cheap) rung instead of borrowing whichever rung the run has
  escalated to.

## What a replacement must preserve

- **Provider validity.** Every tool result stays adjacent to the assistant turn
  that called it; the loop hands the output straight to the next request.
- **Progress.** A strategy whose `ShouldCompact` keeps firing while `Compact`
  shrinks nothing turns the run into an infinite compaction loop. The built-in
  guards this with a tail-elide fallback.
- **Nothing else.** Failure is allowed: returning an error leaves the transcript
  alone for this turn rather than failing the run, and the durable bracket still
  closes so recovery sees a finished attempt rather than a dangling one.

## Known limitations and deferred work

- **One summarization strategy.** There is no per-message importance scoring and
  no way to pin a message as never-summarizable.
- **A summary of a summary degrades.** Nothing bounds how many times a span can
  be re-compacted.
