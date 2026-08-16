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

### The checkpoint has a ceiling too

`CompactionSettings` sizes **both** halves of what a compaction leaves behind:
`KeepRecentTokens` for the verbatim tail, `MaxSummaryTokens` for the checkpoint
that stands for everything older. `effectiveCompaction` clamps the tail to half
the run's budget and the checkpoint to a quarter, so a compaction lands near
three quarters of the ceiling and buys real headroom before the next one.

Bounding the checkpoint is not symmetry for its own sake. The update prompt asks
the summarizer to preserve what it already captured and fold in what is new —
a monotonic instruction, so a model that simply obeys returns a longer checkpoint
every time. Unbounded, it ratchets until it fills the budget alone, at which
point a compaction can no longer bring the transcript back under the ceiling and
re-fires on the next turn. Measured on a 1500-turn run with a 4000-token budget
and a summarizer that folds honestly (`scale_test.go`): a 60 KB checkpoint
against a 4 KB share, half the run spent over budget, and **one summarization
call per 1.6 turns of actual work**. With the ceiling: 4 KB, no turn over budget,
one call per 6.8 turns.

The ceiling is enforced twice — as the summarization call's `MaxTokens`, and as a
clamp on what comes back, because `MaxTokens` is a request and a self-hosted or
OpenAI-compatible endpoint may ignore it. An over-long checkpoint is not one
turn's problem: it is handed back as the next fold's *previous summary*, so a
single unbounded reply becomes the permanent floor of every window after it.

On a normal window the clamp never binds (a quarter of 190k is far above the
2048-token default), so this only shapes behaviour where it was broken — the
small windows a cheap model gives you, which is where the long cheap runs happen.

### The clamp drops whole facts, never half of one

A checkpoint that overshoots has to lose something, and *what* it loses is a
correctness question rather than a formatting one. The clamp used to take a
byte-exact bite out of the middle, which cuts through whatever line it lands on:

```
- [x] shard 3 inspected, code FINDING-3=SHARD003OK
- [x] shard 4 inspected, code FINDING-4=SHARD00      ← ends 004OK
…[981 bytes truncated]…
NDING-23=SHARD023OK                                  ← dangling fragment
```

`SHARD00` is not a missing fact. It is a **wrong one that reads as complete**,
and nothing downstream can tell: this checkpoint is handed to the next fold as
the previous summary with instructions to carry it forward, so the mutilated
identifier propagates for the rest of the run and lands in the final answer
beside the correct ones. Measured end to end on a 320-turn run under a 480-byte
ceiling (`recall_test.go`), the run answered `FINDING-11=SHARD011O` — one wrong
code among fifteen right ones, with nothing anywhere marking it damaged.

The cut now lands on line boundaries and states what it removed:

```
- [x] shard 3 inspected, code FINDING-3=SHARD003OK
…[20 earlier lines dropped to fit the checkpoint budget]…
- [x] shard 24 inspected, code FINDING-24=SHARD024OK
```

Same ceiling, same nine findings lost — every survivor intact, and the loss is a
number the next fold can read and act on. Dropping a fact is honest and
recoverable: the run can look the shard up again. Half a fact presented as whole
is a fabrication the run will defend. A reply with no line structure at all still
falls back to the byte cut, because an unbounded checkpoint becoming the
permanent floor of every later window is the worse failure.

## An oversized tool result is cut, not deleted

When one turn's tool results are bulkier than the whole recent-tail budget, the
cut point cannot fall inside that turn (a tool result must stay adjacent to the
call that issued it), so the tail guard shrinks the results in place instead.

It used to shrink them to nothing — a placeholder reading *"re-run the tool if
you need the detail"*. That is the wrong half of the content to keep and the
wrong advice to give. A tool result puts its **conclusion at the end**: the
finding, the verdict, the error. And "re-run it" assumes the call is cheap and
repeatable, which is false for exactly the results most worth keeping — a
`spawn_subagent` answer is an entire child run that has already been paid for.

Measured on a parent that fans out to 8 sub-agents in one turn (6 KB each, a
4000-token window): **1 of 8 findings survived to the turn the parent answered
on.** The run bought 8 child runs and a summarization call and then reasoned over
a paraphrase. Each oversized result is now cut to 1 KB with `truncateMiddle`,
keeping both ends and marking the gap: **8 of 8**, window 15.6 KB against a 16 KB
budget. Re-truncation is idempotent, so a long run does not re-cut what it
already cut.

## What a replacement must preserve

- **Provider validity.** Every tool result stays adjacent to the assistant turn
  that called it; the loop hands the output straight to the next request.
- **Progress.** A strategy whose `ShouldCompact` keeps firing while `Compact`
  shrinks nothing turns the run into an infinite compaction loop. The built-in
  guards this with a tail-elide fallback and a checkpoint ceiling — a summary
  that may grow without limit is the *slow* version of the same wedge.
- **Nothing else.** Failure is allowed: returning an error leaves the transcript
  alone for this turn rather than failing the run, and the durable bracket still
  closes so recovery sees a finished attempt rather than a dangling one.

## Known limitations and deferred work

- **One summarization strategy.** There is no per-message importance scoring and
  no way to pin a message as never-summarizable.
- **A summary of a summary degrades.** Nothing bounds how many times a span can
  be re-compacted. `MaxSummaryTokens` bounds how *large* the checkpoint gets, not
  how much fidelity it loses across hundreds of folds; the goal pin
  (`goalMarker`) is what keeps the run's objective exempt from that decay.
- **A checkpoint under its ceiling forgets silently.** The clamp is honest about
  *quantity* — it says how many lines went — but the run's final answer is not.
  Measured on a 320-turn run under a 480-byte ceiling: 15 of 24 findings, and the
  answer presents those 15 as the complete set. The facts that survive are all
  correct, which is the fix above; that the answer does not know it is partial is
  not addressed. Preserving the count past the fold that drops it, and having the
  answer say "15 of 24", would need the checkpoint to carry a tally the model
  cannot silently omit.
