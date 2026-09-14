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
await ar.revenue('user-123', { amount: 1900, currency: 'USD', plan: 'pro', kind: 'subscription' }, {
  idempotencyKey: webhook.id, // required — the provider's event id, see Idempotency
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

## Money taxonomy

AgentRay owns two stored event names, and every sender publishes them: this
client, `@agentray/browser`, and a raw `POST /capture` from your billing
provider all write the same payload, so a booking is one row read one way.
`paid_user` is never emitted: paid status is derived from the ledger, from the
earliest deduplicated positive booking on the payer's canonical id.

| Export | Value | Meaning |
| --- | --- | --- |
| `REVENUE_EVENT` | `'revenue'` | one settled money booking |
| `REVENUE_REVERSED_EVENT` | `'revenue_reversed'` | money that came back: a refund, chargeback, or clawback |
| `REFUND_KIND` | `'refund'` | the documented `kind` for a reversal row |
| `STANDARD_REVENUE_KINDS` | `'payment'`, `'in_app_purchase'`, `'subscription'`, `'wallet_topup'`, `'donation'` | the documented booking kinds |

`revenue(distinctId, facts, options)` records a booking under `REVENUE_EVENT`,
and `revenueReversed(distinctId, facts, options)` records money returned under
`REVENUE_REVERSED_EVENT`. Both take `RevenueOptions`, which is `CaptureOptions`
with `idempotencyKey` made mandatory: on a money write a random key is the
defect the key exists to prevent.

```ts
import { AgentRayServerClient } from '@agentray/server';

const ar = new AgentRayServerClient({ apiUrl, apiKey });

await ar.revenue('user-123', {
  amount: 1900,        // smallest unit: $19.00
  currency: 'USD',     // uppercase ISO 4217
  kind: 'subscription',
  plan: 'pro',
}, {
  idempotencyKey: webhook.id,   // required
});

await ar.revenueReversed('user-123', {
  amount: 1900,        // what was actually returned, positive
  currency: 'USD',
  // kind defaults to 'refund'
}, {
  idempotencyKey: `refund:${refund.id}`,  // its own key, never the booking's
});
```

`RevenueFacts` is the booking input: `amount`, `currency` and `kind` required,
plus the optional flat, non-PII dimensions `provider`, `transaction_id`,
`product_id` and `plan`. `RevenueReversalFacts` is the same shape with `kind`
optional. `RevenueProperties` is what lands on the wire — the facts plus the
`$insert_id` stamped from `idempotencyKey` — and it is the exact type the
browser SDK exports.

### `amount` is in the smallest unit

`amount` is an integer in the smallest unit of the currency the sender declared:
`1900` USD is **$19.00**, `50000` VND is 50,000 ₫. Not a decimal, not a float,
and never a converted value. The SDK throws rather than write a row the read
would book as zero — on a non-integer `amount`, an empty `currency`, an empty
`kind`, an empty `idempotencyKey`, and on a `revenueReversed()` whose `amount`
is not positive.

There is no FX and no cross-currency total, ever: each declared currency is
reported separately and signed, so two currencies in one window are two numbers,
not one.

### `kind`

| `kind` | Use |
| --- | --- |
| `payment` | the remaining one-time settlement |
| `in_app_purchase` | an App Store / Play item that is neither a subscription nor a top-up |
| `subscription` | a recurring-entitlement charge |
| `wallet_topup` | a stored-value purchase |
| `donation` | a contribution |

The documented reversal kind is `refund`, which `revenueReversed()` stamps when
`kind` is omitted. `kind` is documentation, not a filter: the read never
inspects it for a booking, so an existing taxonomy — `one_time`, `renewal`,
anything else — still books normally.

### Migrating from 0.1.0

0.2.0 is a compile-time break for money callers. The compiler names every call
site:

- `revenue()` now takes the facts and `RevenueOptions`, and `idempotencyKey` is
  required instead of defaulting to a random UUID.
- `amount` is the smallest unit of `currency` now. A 0.1.0 caller that sent
  dollars must multiply by 100: `amount: 19` becomes `amount: 1900`.
- `facts.kind` is required.
- The `RevenueEvent` type is gone: bookings take `RevenueFacts`, reversals take
  `RevenueReversalFacts`, and the wire shape is `RevenueProperties`.

What did not change: a `revenue` row whose `kind` is `refund`, or whose `amount`
is negative, is still read as a reversal, and a custom `kind` still books.

## Idempotency

Pass the payment provider's event id as `idempotencyKey`; it is sent as
`$insert_id` and stored on the event's `insert_id` column. Every read of money
de-duplicates on it, once: rows are grouped by
`coalesce(nullif(insert_id, ''), event_id)` with last write wins, reversals are
netted against bookings, and the result is reported per declared currency with
no FX.

The Overview **Net revenue** tile and the SQL recipe in
[`docs/ANALYTICS.md`](https://github.com/lohi-ai/agentray/blob/main/docs/ANALYTICS.md#reading-money-with-sql)
implement that same contract, so a hand-written query and the dashboard agree —
do not write a second de-duplication of your own.

The conventional event name is `revenue`; AgentRay's Growth Lead and Data
Analyst agents read MRR/LTV/conversion from it without extra configuration.

## API

| Method | Purpose |
| --- | --- |
| `capture(distinctId, event, props?, opts?)` | Send one event; resolves on success, throws on failure. |
| `revenue(distinctId, facts, opts)` | Book money under `revenue`; `RevenueOptions.idempotencyKey` is required. |
| `revenueReversed(distinctId, facts, opts)` | Record money that came back under `revenue_reversed`; `kind` defaults to `refund`. |
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
