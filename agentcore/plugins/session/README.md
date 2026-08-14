# session

**Seam.** Ejectable — without it the run is purely in-memory: a crash loses it,
and it cannot be resumed.

Installs the `SessionStore`, the session id, resume mode, and the seed set of
disabled tools.

## Model Experience

### A fresh durable run

#### What the model sees

Nothing. Durability is invisible to the model while things go well.

#### Token effect

**Zero-direct.**

### A resumed run

#### What the model sees

The conversation rebuilt from the append-only log — including its own dangling
retry-safe tool calls replayed with their **original** call ids, and interrupted
calls closed with a note.

##### Verbatim text for an interrupted call

```
interrupted: the run stopped before this call completed
```

#### Token effect

**Retained.** The reduced history is the run's starting prefix, so a long
resumed conversation starts expensive.

#### KV cache effect

**Replacing.** A resume rebuilds the prefix from the log, so the provider's
cache for the original run does not apply.

### A completed log

Short-circuits to its recorded answer: zero provider calls, no duplicate spend,
no repeated side effects.

## Impact on the agent

- Durability is what makes `spawn_subagent` reattachable and what
  `observe.LogInvariant` checks. Without a session there is no log, so that
  plugin declines the run.
- `SeedDisabledTools` pre-populates the circuit breaker, so a tool disabled in a
  crashed run stays disabled when that run resumes.
- Idempotency keys are `(sessionID, callID)` and are replayed verbatim, so an
  external system can dedupe an effect across a crash.
- A resume whose log cannot be read **fails loudly** rather than degrading to a
  from-scratch run that would splice fresh seeds onto work already recorded.

## Known limitations and deferred work

- **No compaction of the log itself.** It grows for the life of the session;
  reduction happens at read time.
- **No log integrity check.** A tamper-evident audit chain is on the fleet
  roadmap.
