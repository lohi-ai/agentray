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

## Known limitations and deferred work

- **No relevance budget.** The store decides what to return; nothing bounds it
  against `MaxContextTokens` before assembly.
- **No forgetting.** There is no eviction, decay, or contradiction resolution in
  the contract; that is the store's problem.
