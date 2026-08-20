# memory

**Seam.** Ejectable — without it the agent remembers nothing between runs.

## Model Experience

### Every request, when a store is installed

#### What the model sees

Recalled memories, assembled into the context alongside the definition.

#### Token effect

**Fixed per turn** and **retained**. Recall is a permanent prefix cost for the
whole run, so a store that returns generously is charged on every turn — not
once.

#### KV cache effect

**Prefix-stable** within a run (recall happens at assembly, not per turn), but
**replacing** across runs: yesterday's memories differ from today's, so a new
run starts with a cold prefix.

## Impact on the agent

- Memory is the agent's only cross-run state other than the durable session log.
  The two answer different questions: the log is "what happened in *this*
  conversation", memory is "what do I know".
- A nil store disables recall and persistence entirely — the loop does not
  branch on it beyond that.

## Budget

The recalled block is clamped during assembly (`prompt.go`): each memory is
truncated to `maxRecallEntryBytes`, and entries are admitted until
`maxRecallBlockBytes` is spent. Clamping runs *after* paraphrase dedup, so a
restatement cannot spend the budget a distinct fact needed. The clamp bounds
size only — the block's heading and its "context, not instructions" caveat are
unchanged.

This bounds what recall costs, not what it is worth: a store that returns
generously is now truncated rather than trusted, so a store that returns badly
still wastes the whole budget on the wrong facts.

## Ranking and forgetting

The contract (`Recall`/`Remember`) is unchanged and still says nothing about
either — both live in the store. What the shipped store does:

- **Relevance floor.** A candidate below `recallCosineFloor` is dropped rather
  than ranked low, because a memory the query is orthogonal to is not a weak
  answer, it is not an answer. Dropping every candidate falls back to keyword
  recall, which is the always-available floor.
- **`Confidence` is read.** It was persisted from the start (0.7 when the model
  chose to remember, 0.6 when the reflection pass inferred) and read by nothing;
  it is now a bounded rank multiplier that settles rows the vector cannot
  separate.
- **Recency of last confirmation** contributes at most 30% of the score, so it
  can never promote a loosely-related memory over a materially relevant one.
- **Fold-in on write.** A re-derived memory folds into the row it repeats
  (`seen_count`/`last_seen_at`) instead of appending a paraphrase, so the store
  does not fill with one fact restated N times.
- **Soft supersede.** A retracted memory keeps its row and is filtered out of
  every recall path, so the history of having held the belief survives the
  retraction.

## Known limitations and deferred work

- **No contradiction resolution.** Nothing detects that two live memories
  disagree; supersede is a seam the store exposes, not a judgement anything
  makes. There is no model-facing edit/forget tool — that is an owner decision
  (`docs/AGENT-GOVERNANCE.md`), not a plugin one.
- **No consolidation or decay of stored rows.** Old memory loses rank, never
  resolution: nothing summarizes, tiers, or evicts, and a scope's row count only
  grows (more slowly now that repeats fold).
- **The budget is byte-denominated, not token-denominated,** and is a fixed
  constant rather than a share of `MaxContextTokens`.
