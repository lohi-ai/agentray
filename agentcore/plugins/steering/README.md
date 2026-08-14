# steering

**Seam.** Ejectable — without it a run cannot be corrected mid-flight and cannot
continue after it would stop.

Installs three things: the steering queue (drained at the top of every turn), the
follow-up queue (drained when the run would end), and `PrepareNextTurn` (the
per-turn save-point).

## Model Experience

### A steer arrives

#### What the model sees

The user's message, threaded into the conversation **before** the model reasons,
so the correction is honored on the very next turn rather than after the current
plan completes.

#### Token effect

**Fixed** per steer.

#### KV cache effect

**Append-only.**

### A follow-up arrives at the end

#### What the model sees

The new message, and the run **restarts** instead of returning — so a
conversation continues inside one bounded run.

#### Token effect

**Retained.** The whole prior conversation stays in the prefix, so a long
follow-up chain is progressively more expensive per turn.

#### KV cache effect

**Prefix-stable** — this is the cheap way to continue a conversation, since the
prefix is never rebuilt.

### `PrepareNextTurn` changes the model or tools

#### Token effect

**Replaced** — swapping the tool set or system prompt rewrites the request
prefix.

#### KV cache effect

**Replacing.** Worth knowing before wiring a hook that changes tools every turn.

## Impact on the agent

- Both drained messages are persisted to the durable log, so a resumed run
  replays the conversation the model actually saw.
- Both are reported as external input, so a repeat detector resets its chain: the
  model has been given information it did not have.
- `MaxTurns` still bounds a follow-up-extended loop; it is not an escape from the
  run's limits.
- `PrepareNextTurn` applies to the **next** turn, never the in-flight one, and
  empty returned fields keep the current value — so a careless hook cannot blank
  the run.

## Known limitations and deferred work

- **Drain-only.** agentcore polls the callbacks; it cannot be pushed to, so a
  steer is honored at turn granularity, not immediately.
- **No steer during a long tool call.** A turn already inside a 5-minute tool
  execution will not see a correction until it returns.
