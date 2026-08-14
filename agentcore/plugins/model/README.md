# model

**Seam.** Not ejectable — an agent with no provider cannot reason.

Installs the provider, the model, the escalation ladder, retry policy, per-turn
key refresh, output cap, reasoning effort, structured output, and prompt caching.

## Model Experience

### Always

#### What the model sees

Nothing added to the conversation. What changes is *which* model reads it and
under what constraints.

#### Token effect

**Capped**, via `MaxTokens` — the model's output tokens per turn. Left at 0 the
provider applies its own default, which can truncate a large artifact with
`stop_reason: "length"`. Agents that emit big outputs (long documents, full HTML
pages) need this set generously.

#### KV cache effect

**Prefix-stable** when `PromptCacheKey` is set: every provider call in the run
opts into caching (OpenAI `prompt_cache_key`; Anthropic `cache_control` on the
system prefix). Empty leaves caching off, so compat servers that do not support
it are unaffected.

**Replacing** on escalation: falling to a different rung changes the model, and
the new model's cache is cold. The ladder is a cost decision with a latency
tail, not a free retry.

## Impact on the agent

- `Escalation` is walked only on a **retryable** provider failure, never on a
  cancellation.
- `Retry` bounds same-model backoff before escalation, so a brief 429/5xx blip
  does not jump straight to a pricier rung.
- `RefreshKey` re-resolves the API key before each turn, so an expiring BYO token
  does not kill a long run. It is applied only if the provider implements
  `KeyUpdater`.
- `OutputSchema` constrains every text answer at the provider. Verdict-shaped
  agents only — it makes prose impossible.

## Known limitations and deferred work

- **The ladder is ordered, not adaptive.** There is no learning about which rung
  is currently healthy.
- **`ReasoningEffort` is passed through blind.** Providers without the knob
  ignore it; there is no capability check.
