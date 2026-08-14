# budget

**Seam.** Ejectable — without it a run is bounded only by turns and tool calls,
not by spend.

Installs the budget gate (consulted with running usage at the top of each turn)
and the step gate (blocks before each turn).

## Model Experience

### The budget gate trips

#### What the model sees

A final "budget exhausted — summarize and stop" user turn, **with tools
stripped**, so it can only write a wrap-up.

#### Token effect

**Fixed**, one extra turn. Deliberately spent: an agent that vanishes at its
ceiling returns nothing, while one that spends a last cheap turn returns what it
learned.

#### KV cache effect

**Append-only** for the message; **replacing** for the tool schemas, since
removing them changes the request prefix.

### The step gate pauses

#### What the model sees

Nothing — it is not running. The gate blocks before any work (compaction,
steering, reasoning) happens.

#### Token effect

**Zero-direct.**

## Impact on the agent

- The consumer owns the ceiling and the spend lookup; agentcore only sees the
  boolean verdict against running `Usage` — which **includes** what sub-agents
  spent (`peekChildUsage`).
- A budget wrap-up is a stop the run did not choose, so no stop interceptor and
  no goal gate may re-open it. `StopReason` is `budget_exhausted`.
- The step gate is how the Lab's explain mode pauses a live run before each step
  without changing any other behavior: gating, secret resolution, budgets, and
  escalation all still run after it releases. A nil gate never pauses, so
  production is unaffected.

## Known limitations and deferred work

- **Checked per turn, not per call.** A single turn with many parallel expensive
  tool calls can overshoot the ceiling before the next check.
- **No soft warning.** The gate is binary; there is no "you are at 80%" signal
  the model could plan against.
