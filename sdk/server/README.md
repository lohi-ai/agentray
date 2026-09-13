# @agentray/server

The server SDK for [AgentRay](https://agentray.lohi2.com) — the sanctioned path
for **revenue truth**. The browser cannot be trusted to report money, so
payment, subscription, and refund events come from your backend through this
client. Works on Node ≥ 18 and Bun (global `fetch`); zero dependencies.

## Install

Releases are published to GitHub first — the tarball below is the exact artefact
CI built, tested and installed into a clean project before publishing:

```bash
npm install https://github.com/lohi-ai/agentray/releases/download/server-v0.1.0/agentray-server-0.1.0.tgz
```

Pick the version you want from [Releases](https://github.com/lohi-ai/agentray/releases?q=server); the tags are
`server-v<semver>`.

Once the `@agentray` npm scope is published, the shorter form works and is the
one to prefer:

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
  SELECT arg_max(coalesce(try_cast(json_extract_string(properties, '$.amount') AS DOUBLE), 0), "timestamp") AS amount
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

## Which app an event came from

Every event carries `platform: "server"`, which keeps backend-sent events —
payments above all — out of the "unknown" bucket in Traffic's platform split and
the per-platform funnel.

It has to be stated. Node's global `fetch` identifies itself as the bare word
`node`, which matches no client the server can recognise, so an untagged revenue
event is attributed to nothing at exactly the point money is counted.

When you are relaying on a client's behalf and know its real platform, say so:

```ts
new AgentRayServerClient({ apiUrl, apiKey, platform: 'ios' });
```
