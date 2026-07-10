# @agentray/server

The server SDK for [AgentRay](https://agentray.lohi2.com) — the sanctioned path
for **revenue truth**. The browser cannot be trusted to report money, so
payment, subscription, and refund events come from your backend through this
client. Works on Node ≥ 18 and Bun (global `fetch`); zero dependencies.

## Install

```bash
npm install @agentray/server     # or: bun add @agentray/server
```

No AgentRay account yet? The CLI gets you a key without opening the web app:

```bash
agentray signup --email you@example.com   # or: agentray login
export AGENTRAY_API_KEY=$(agentray key)
```

## Quick start

```ts
import { AgentRayServerClient } from '@agentray/server';

const ar = new AgentRayServerClient({
  apiUrl: process.env.AGENTRAY_URL!,      // e.g. https://agentray.example.com
  apiKey: process.env.AGENTRAY_API_KEY!,  // server-side only — never ship to clients
});

// In a payment webhook handler:
await ar.revenue('user-123', { amount: 19, currency: 'USD', plan: 'pro', kind: 'subscription' }, {
  idempotencyKey: webhook.id, // the provider's event id — see Idempotency
});

// Durable user traits without an event:
await ar.identify('user-123', { plan: 'pro' });

// Any other server-truth event:
await ar.capture('user-123', 'subscription_cancelled', { plan: 'pro' }, { idempotencyKey: job.id });
```

## Why it differs from the browser SDK

1. **Identity is explicit.** The server already knows the user, so every call
   takes a `distinctId` — no anonymous lifecycle, no localStorage.
2. **Calls are awaitable and throw.** A dropped pageview is fine; a dropped
   payment event is not. Callers can await, retry, and alert.
3. **Every event carries an idempotency key** (`$insert_id`). Payment webhooks
   retry; the key lets reads de-duplicate instead of double-counting MRR.

## Idempotency

Pass the payment provider's event id as `idempotencyKey`; it is stored on the
event's `insert_id` column. De-duplicate at read time:

```sql
SELECT sum(amount) AS revenue FROM (
  SELECT argMax(JSONExtractFloat(properties, 'amount'), timestamp) AS amount
  FROM events WHERE event_name = 'revenue' GROUP BY insert_id
)
```

The conventional event name is `revenue`; AgentRay's Growth Lead and Data
Analyst agents read MRR/LTV/conversion from it without extra configuration.

## API

| Method | Purpose |
| --- | --- |
| `capture(distinctId, event, props?, opts?)` | Send one event; resolves on success, throws on failure. |
| `revenue(distinctId, event, opts?)` | Sugar for `capture(..., 'revenue', ...)` — always pass `idempotencyKey`. |
| `identify(distinctId, traits?)` | Set durable person traits (`$set`). |
| `alias(anonymousId, canonicalId)` | Link a browser anonymous id to the canonical user id server-side. |
| `batch(events)` | Send many events in one request; per-event idempotency keys. |

## Build

```bash
npm run build      # tsup → dist/ (ESM + CJS + d.ts)
npm run typecheck
```
