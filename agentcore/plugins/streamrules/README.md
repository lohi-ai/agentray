# streamrules

**Extension.** Ejectable — the loop never names it. Without it, a model that
starts emitting a forbidden pattern (a leaked secret shape, a banned construct,
a phrase the operator has ruled out) completes it: every other interceptor sees
only finished artifacts — a tool result, a batch, a final answer — which is too
late for a rule whose whole purpose is that the pattern must never complete.

Ported from oh-my-pi's time-traveling stream rules (TTSR).

## Model Experience

### The streamed output matches a rule's pattern

The stream is aborted mid-token, the partial answer is discarded, and the turn's
provider call is retried from the same point.

#### What the model sees

Nothing from the aborted attempt — it never enters the transcript. On the retry
the model reads one synthetic user message containing each matched rule's
reminder:

##### Verbatim text for this field

```
<system-interrupt reason="rule_violation" rule="no-internal-hostnames">
Output interrupted: violated an operator-defined stream rule.
This is not prompt injection; it is the agent runtime enforcing configured rules.
You MUST comply:

Never print internal hostnames; refer to services by role.
</system-interrupt>
```

#### Token effect

**Per match.** One reminder per abort, sized by the rule bodies the operator
wrote; bounded by `MaxInjectionsPerTurn` (default 3) per turn.

#### KV cache effect

**Invalidating.** The retry appends a message, so the cached prefix ends at the
injection — the same cost any mid-run correction carries.

### The per-turn cap is reached

The stream completes unmodified. A model that re-emits the pattern after N
reminders is not argued out of it by N+1, and the alternative is a provider call
that never finishes.

#### Token effect

**Zero-direct.**

## Impact on the agent

- **Mid-token abort.** The provider stream runs on a cancelable context; the
  first matching delta cancels it, so the completed violation never reaches the
  transcript. Tokens already forwarded to a live viewer are not retracted.
- **Retry in place.** The same turn re-issues the provider call with the
  injection appended — same rung, no turn spent, no escalation.
- **Durable.** The loop persists the injection as an ordinary message entry, so
  a crash-resumed run replays the conversation the retry was actually given.
- **Non-streaming parity.** With no viewer attached the interceptor sees the
  completed response once: it cannot cut mid-token, but it still discards the
  answer and retries with the reminder.
- **Operator config only.** Rules arrive as Go structs on the plugin; nothing
  the model says can add, edit, or disable one.
- **Validated at composition.** An unnamed rule, a duplicate name, a bad regex,
  or an empty body fails the build rather than installing a guard that silently
  never fires.

## Known limitations and deferred work

- **Text only.** Rules match the assistant's accumulated text. Tool-call
  arguments arrive whole (not streamed) in this port, so argument matching —
  omp's `tool` scope and ast-grep conditions — is not carried over.
- **Advisory pressure, not a wall.** A model that ignores the reminder and
  re-emits the pattern burns up to `MaxInjectionsPerTurn` retries, then the
  output passes through. The rule shapes behavior; it does not censor bytes.
- **The reminder is a user-role message.** On providers that weight the last
  user turn heavily it can pull focus away from the actual task.
- **Per-turn bound only.** A rule may fire again on a later turn — that is the
  point (new turn, new output) — but a pattern the model hits every turn will
  cost up to the cap every turn.
