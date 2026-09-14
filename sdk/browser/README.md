# @agentray/browser

The browser SDK for [AgentRay](https://agentray.lohi2.com). One `init()` wires up
identity (anonymous ↔ identified), batched delivery, retries, and `sendBeacon`
flush on page unload.

## Install

Releases are published to GitHub first — the tarball below is the exact artefact
CI built, tested and installed into a clean project before publishing:

```bash
npm install https://github.com/lohi-ai/agentray/releases/download/browser-v0.1.0/agentray-browser-0.1.0.tgz
```

Pick the version you want from [Releases](https://github.com/lohi-ai/agentray/releases?q=browser); the tags are
`browser-v<semver>`.

Once the `@agentray` npm scope is published, the shorter form works and is the
one to prefer:

```bash
npm install @agentray/browser
```

No bundler? The same bundle loads from a `<script>` tag and exposes
`window.AgentRay`. Download `agentray-browser-<version>.min.js` from the release
and serve it yourself, or once the package is on npm, from unpkg:

```html
<script src="https://unpkg.com/@agentray/browser@0/dist/index.global.js"></script>
<script>
  AgentRay.init({ host: 'https://agentray.example.com', apiKey: 'phc_your_project_key', autocapture: true });
</script>
```

Pin the major version in that URL. An analytics tag that silently upgrades on
someone else's marketing site is a liability.

## Quick start

```ts
import { init } from '@agentray/browser';

const ar = init({
  host: 'https://agentray.example.com',
  apiKey: 'phc_your_project_key',
  autocapture: true, // delegated click + pageview capture
  respectDoNotTrack: true, // opt in: honor DNT/GPC (default false) — see below
});

// Manual events
ar.capture('checkout_started', { plan: 'pro' });

// On login — links the prior anonymous session to the user
ar.identify('user-123', { email: 'alice@example.com' });

// On logout
ar.reset();
```

## API

| Method | Purpose |
| --- | --- |
| `init(opts)` | Create the client. `opts`: `host`, `apiKey`, optional `autocapture`, `captureConfig`, `respectDoNotTrack`, `batching`, `platform`. |
| `capture(event, props?)` | Queue an event (flushed in batches). |
| `identify(userId, traits?)` | Switch to an identified user; aliases the anonymous history. |
| `alias(anon, canonical)` | Manually link two IDs (advanced). |
| `reset()` | Start a fresh anonymous session. |
| `flush()` | Force-send buffered events and pending identity work now. |
| `autocapture(opts?, config?)` | Turn on delegated capture; returns an uninstall fn. `config` overrides the `captureConfig` from `init()`. |

`captureConfig` constrains *how* autocapture collects without changing *what*:
`clickAllowlist` narrows click capture to a selector, `internalHosts` normalizes
referrers from your own domains so they don't land in the referrer table.

## Money events

Revenue is a shared contract, not a per-project convention: this SDK,
`@agentray/server` and a raw `POST /capture` all publish the same two event
names and the same payload, so a booking sent from any of them is one row, read
one way.

| Export | Value | Meaning |
| --- | --- | --- |
| `REVENUE_EVENT` | `'revenue'` | one settled money booking |
| `REVENUE_REVERSED_EVENT` | `'revenue_reversed'` | money that came back: a refund, chargeback, or clawback |
| `REFUND_KIND` | `'refund'` | the documented `kind` for a reversal row |
| `STANDARD_REVENUE_KINDS` | `'payment'`, `'in_app_purchase'`, `'subscription'`, `'wallet_topup'`, `'donation'` | the documented booking kinds |

`RevenueProperties` is the payload (`RevenueKind` and `StandardRevenueKind` are
the matching types):

| Property | Required | Meaning |
| --- | --- | --- |
| `amount` | yes | Integer in the smallest unit of `currency` — `1900` USD is $19.00, `50000` VND is 50,000 ₫. |
| `currency` | yes | Uppercase ISO 4217 code, e.g. `USD`, `VND`. |
| `kind` | yes | What the sale was: one of `STANDARD_REVENUE_KINDS`, or a customer kind that already exists. |
| `$insert_id` | yes | Stable, row-specific idempotency key. This is the key the read de-duplicates on. |
| `provider` | no | `stripe`, `sepay`, `app_store`, … |
| `transaction_id` | no | The provider's transaction id, when it differs from `$insert_id`. |
| `product_id` | no | The purchased product or SKU. |
| `plan` | no | The plan id for a subscription purchase. |

There is deliberately **no** `revenue()` helper on this SDK. A browser is a
forgeable environment, so it carries no privileged money path: these constants
go through the ordinary identity-aware `capture()` like any other event, and the
browser's claims about money are worth exactly as much as its other claims.
Server-truthful bookings and reversals belong to `@agentray/server`, or to a raw
HTTP POST from your billing provider.

```ts
import { init, REVENUE_EVENT, REVENUE_REVERSED_EVENT } from '@agentray/browser';

const ar = init({ host, apiKey });

// A purchase the client completed itself, e.g. an App Store receipt.
ar.capture(REVENUE_EVENT, {
  amount: 1900,              // smallest unit: $19.00
  currency: 'USD',           // uppercase ISO 4217
  kind: 'in_app_purchase',
  $insert_id: receipt.transactionId,  // stable; a retried send de-dups
});

// Money the client already saw come back.
ar.capture(REVENUE_REVERSED_EVENT, {
  amount: 1900,
  currency: 'USD',
  kind: 'refund',
  $insert_id: `refund:${refund.id}`,  // its own key, never the booking's
});
```

The SDK supplies `distinct_id`, `timestamp` and `platform`; the caller supplies
`$insert_id`. That split is the whole contract, and the key has to be stable:
the read de-duplicates on it, so a random one silently double-counts a retried
send, while a reversal that reuses the booking's key replaces the booking
instead of netting against it.

`kind` is documentation, not a filter — the read never inspects it for a
booking, so an existing taxonomy (`one_time`, `renewal`, anything else) still
books normally.

`amount` and `currency` are declared by the sender and never converted. There is
no FX and no cross-currency total: each declared currency is reported separately
and signed. Traits have no place on a money row — `$set`/`$set_once` are not
money properties; send them through `identify()`.

## Privacy: `respectDoNotTrack`

**Off by default.** No snippet that imports this SDK starts dropping events
because of a browser header nobody at the site asked it to read. Turn it on
deliberately — it is a promise to that visitor, and turning it on means keeping
it:

```ts
const ar = init({ host, apiKey, respectDoNotTrack: true });
```

With the option on, `init()` checks the browser's signal **first**, before it
constructs anything:

| signal | result |
| --- | --- |
| `navigator.globalPrivacyControl === true` | suppressed — GPC wins whatever DNT says |
| `navigator.doNotTrack === '1'` or `'yes'` | suppressed |
| `null`, `undefined`, `'0'`, `'no'`, anything else | collects normally — no preference asserted |
| `navigator` unavailable (SSR, prerender, a worker) | collects normally |

Suppressed `init()` returns an inert facade. No anonymous ID is minted or read,
no unload/autocapture/history/observer listener is installed, no timer starts,
nothing is written to `localStorage`, and no request — `fetch` or `sendBeacon`,
then or on page hide — is ever made. `getDistinctId()` answers `''` rather than
inventing an identity, `flush()` resolves immediately, and every method is safe
to call on a page that expects analytics to be running.

Nothing can opt back in afterwards. If the preference changes, call `init()`
again — a different `host`/`apiKey` is a different client, and the suppressed one
never had one.

## Delivery semantics

Events are buffered and sent to `POST /batch` when the buffer reaches
`batchSize` (default 20) or after `flushIntervalMs` (default 3000). Transient
5xx/network failures retry with exponential backoff (1 s, doubling, capped at
8 s) until `retryBudgetMs` (default 60000) is spent; 4xx responses are not
retried, because resending a bad key cannot fix it. On
`visibilitychange→hidden` and `pagehide` the buffer is flushed via
`navigator.sendBeacon` so the tail of a session is not lost when the tab closes.

**A dropped batch is reported, never silent.** An event batch has no durable
copy — unlike an alias, which waits out a page load in `localStorage` — so once
the budget is spent those events are gone. Every abandonment therefore fires
three signals: the `onBatchDropped` callback (wire it to your own error
reporting), a `console.warn`, and an `agentray:batch_dropped` window event whose
`detail` is `{ events, attempts, reason }`. A 4xx is reported the same way, with
`attempts: 1`.

```ts
init({
  host,
  apiKey,
  batching: {
    // A restart, not a deploy: the deploy's own healthcheck expects the API to
    // answer within its 30 s start_period, and a blue/green roll keeps the old
    // colour serving. Raise it if a longer window is worth holding a batch in
    // memory for.
    retryBudgetMs: 60_000,
    onBatchDropped: (drop) => reportToSentry('agentray batch dropped', drop),
  },
});
```

The budget is a deadline rather than an attempt count because what it has to
outlast is a *window*: with a capped backoff curve, "3 attempts" silently means
a different amount of time on every failure pattern. Deliveries are also
serialized — a batch that is still retrying is not joined by a second loop for
the events captured meanwhile, so an outage produces one retry loop rather than
one per flush interval.

What the budget does **not** cover is a page that navigates mid-outage: the
in-flight batch lives in memory, and the unload beacon cannot save it because
`/batch` is not idempotent (beaconing a batch the retry loop may also deliver
would double-count its events). Closing that gap needs a durable queue, which
would put event payloads — URLs, referrers, caller-authored properties — in
`localStorage`, a privacy surface this SDK deliberately keeps to ID pairs only.
It is not implemented; treat it as a separate decision.

`identify()` and `alias()` are not events and do not travel in a batch. They go
out on their own ordered lane — `POST /alias` first, then `POST /identify` —
with the same retry and beacon-on-unload policy, in that order, so the traits
never land on a person who does not yet own the anonymous history. They also
never ride `/batch`: a batch item has no operation discriminator and the batch
handler does not merge a top-level `$set`, so identity sent that way is accepted
with a `200` and changes nothing.

Two consequences worth knowing:

- `flush()` settles identity work before it settles events, so a flush that
  returns means the alias behind those events has been answered.
- An alias that never got a `2xx` is remembered *as an ID pair only* — no
  traits, which may be personal data — and replayed on the next page load. The
  anonymous ID is kept until the server confirms, because deleting it early is
  what orphans a reader's history. Confirmation clears it only if it is still
  current, so a logout `reset()` that raced ahead keeps the next visitor's ID.

`reset()` is purely local: it mints a fresh anonymous ID and cancels no pending
identity work.

## Build & test

```bash
npm run typecheck
npm test           # vitest, jsdom
npm run build      # tsup → dist/ (ESM + CJS + <script> global + d.ts)
```

## Which app an event came from

Every event carries `platform: "web"`. That property is what lets Traffic's
platform split, the per-platform funnel, and the filter bar tell your website's
audience from your app's, in a product that has both — instead of reporting one
blended number that describes neither.

It is stated rather than left to a user-agent guess on the server: the guess is
right for a plain tab and wrong the moment the bundle runs somewhere else — an
Electron renderer, a prerender bot, a jsdom test — where it lands in "unknown".

Set it once when this bundle genuinely is not the website. A Capacitor or Cordova
build shipped inside the iOS app should say so, or its users are counted as web
traffic:

```ts
init({ host, apiKey, platform: 'ios' });
```

Per-call properties cannot override it — the platform is applied after your own
keys, so a stray `platform` in a `capture()` call can't mislabel a surface.
