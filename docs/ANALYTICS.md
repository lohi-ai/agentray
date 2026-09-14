# AgentRay Analytics

What the events you send are called, and what AgentRay reads them as. Read this
before adding, renaming, or removing an event: a name is a contract between
every SDK, the raw HTTP ingest and every chart, agent and SQL query that already
reads it.

This file is AgentRay's own contract. A consuming product (LoHi, or your app)
may keep its own analytics notes; those describe its emitters, not this
taxonomy.

---

## The money taxonomy

Financial analysis — sales, in-app purchase, subscriptions, top-ups, donations,
cohorts of people who paid, the Overview **Net revenue** tile — works for every
customer without that customer inventing an event name. AgentRay owns exactly
two stored names:

| Event | Meaning |
| --- | --- |
| `revenue` | one settled money booking |
| `revenue_reversed` | money that came back: a refund, chargeback, or clawback |

They are ordinary events: same envelope, same ingest path, same permissive
acceptance as `user.pageview`. Nothing is special-cased server-side, which is why
a billing provider can emit them with a plain HTTP POST.

### Payload

```json
{
  "api_key": "<project capture key>",
  "event": "revenue",
  "distinct_id": "<canonical payer id>",
  "session_id": "<optional>",
  "timestamp": "<optional RFC3339 occurrence time>",
  "properties": {
    "amount": 1900,
    "currency": "USD",
    "kind": "in_app_purchase",
    "$insert_id": "receipt-8812",
    "provider": "app_store",
    "product_id": "coins.500",
    "plan": "pro",
    "platform": "ios"
  }
}
```

| Property | Required | Meaning |
| --- | --- | --- |
| `amount` | yes | Integer in the smallest unit of `currency` — `1900` USD is $19.00, `50000` VND is 50,000 ₫. Not a decimal, not a float. |
| `currency` | yes | Uppercase ISO 4217 code, e.g. `USD`, `VND`, `JPY`. |
| `kind` | yes | What the sale was — see the vocabulary below. |
| `$insert_id` | yes | Stable, row-specific idempotency key. This is the key the read de-duplicates on. |
| `provider` | no | `stripe`, `sepay`, `app_store`, … |
| `transaction_id` | no | The provider's transaction id, when it differs from `$insert_id`. |
| `product_id` | no | The purchased product or SKU. |
| `plan` | no | The plan id for a subscription purchase. |

The optional keys are flat, non-PII dimensions. Traits (`email`, `name`,
anything personal) do **not** belong on a money row: send them through
`identify()` / `POST /identify`, so a financial event can never rewrite who a
person is. `$set` and `$set_once` are not money properties.

`distinct_id` is the payer, the same canonical id every other event uses, and
`platform` is stamped by the sender's SDK so a purchase made in the app and one
made on the web can be told apart.

### Booking kinds

| `kind` | Use |
| --- | --- |
| `payment` | the remaining one-time settlement |
| `in_app_purchase` | an App Store / Play item that is not a subscription and not a top-up |
| `subscription` | a recurring-entitlement charge |
| `wallet_topup` | a stored-value purchase |
| `donation` | a contribution |

The reversal kind is always `refund`.

**These are documentation, not a filter.** The read never inspects `kind` when
deciding what to include, so an existing customer taxonomy — `one_time`,
`renewal`, `topup`, anything else — still books normally. The vocabulary exists
so that a project starting today has one obvious answer, and so a query can
compare "subscription vs. top-up vs. one-off" later.

### Two rules that make the totals trustworthy

1. **The unit is the sender's.** There is no FX and no cross-currency total,
   ever. A row declares `currency`; the read reports each declared currency
   separately, signed. AgentRay does not convert, does not filter against the
   ISO registry, and does not sum currencies together.
2. **A correction re-uses the key; a refund does not.** Both `revenue` and
   `revenue_reversed` are de-duplicated on `coalesce(nullif(insert_id, ''),
   event_id)`, keeping the **last write** (greatest timestamp). So a retried
   webhook carrying the same `$insert_id` is one booking, and a correction sent
   later under the booking's own key replaces the value it fixes. A reversal is
   a *separate fact*: give it its own key, or it will replace the booking
   instead of netting against it.

A `revenue` row whose `kind` is `refund`, or whose `amount` is negative, is read
as a reversal — the shape the server SDK documented before `revenue_reversed`
existed. It still works; prefer the explicit name for new emitters.

### What the Overview tile reports

The **Net revenue** tile on `/overview` reads exactly these two names over the
selected range and platform, and reports:

- a headline in the **dominant currency** — the one with the highest
  deduplicated gross in the window, ties broken by currency code;
- `gross`, `reversed` and `net` **per currency**, never combined across
  currencies;
- an **unclamped signed** net: a window with more reversals than bookings reports
  a negative number, because that is what happened;
- `ok` when at least one deduplicated money row carries a currency, and `no_data`
  when none does — a zero-amount booking with a currency is money and reads
  `0`, never "No data";
- the exclusions: rows that declared no currency, and rows that declared `LT`.

### `LT` is not money

`LT` is a platform credit, not a currency. Rows declaring `LT` are excluded from
every total and named as excluded (`excluded_currencies`) rather than silently
reinterpreted, so a credit balance can never be mixed into a revenue figure.
Pre-cutover `LT` history stays readable through `run_sql`; it just never lands
in a money total.

### `paid_user` is derived, not emitted

There is deliberately **no `paid_user` event**. A person's paid status is
derived from the ledger: the earliest deduplicated positive booking for their
canonical id. A second event that says "this person is paid" can disagree with
the money truth, and the one that disagrees is always the one somebody charts.
Subscription *lifecycle* is separate and unchanged
(`subscription_started` / `renewed` / `cancelled`): a settled subscription
charge also emits `revenue` with `kind: 'subscription'`.

---

## Reading money with SQL

The Overview **Net revenue** tile implements the contract below, and this is the
same contract hand-written (`run_sql`, a saved chart, an agent, a cohort). Both
go through one de-duplication — the tile's is the Go read in
`internal/dataplane/store/money.go`, and the recipe below is the SQL form of it,
pinned to that code by a test. Do not write a third one.

The grid first: one row per write, last write wins.

```sql
WITH money_raw AS (
  SELECT
    canonical_distinct_id AS person_id,
    event_id,
    event_name,
    "timestamp" AS occurred_at,
    upper(trim(coalesce(json_extract_string(properties, '$.currency'), ''))) AS currency,
    coalesce(try_cast(json_extract_string(properties, '$.amount') AS BIGINT), 0) AS amount,
    lower(trim(coalesce(json_extract_string(properties, '$.kind'), ''))) AS kind,
    coalesce(nullif(insert_id, ''), CAST(event_id AS VARCHAR)) AS row_key
  FROM resolved_events
  WHERE project_id = '<project id>'
    AND "timestamp" >= TIMESTAMPTZ '2026-09-01T00:00:00Z'
    AND "timestamp" <  TIMESTAMPTZ '2026-10-01T00:00:00Z'
    AND event_name IN ('revenue', 'revenue_reversed')
),
money_rows AS (
  SELECT
    person_id, event_id, event_name, occurred_at, currency, amount, kind,
    row_number() OVER (PARTITION BY row_key ORDER BY occurred_at DESC, event_id DESC) AS write_rank
  FROM money_raw
)
```

Money the project actually earned, per currency, signed and unclamped:

```sql
SELECT
  currency,
  sum(CASE WHEN event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0 THEN 0 ELSE amount END) AS gross,
  sum(CASE WHEN event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0 THEN abs(amount) ELSE 0 END) AS reversed,
  sum(CASE WHEN event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0 THEN -abs(amount) ELSE amount END) AS net,
  count(*) AS rows
FROM money_rows
WHERE write_rank = 1
GROUP BY currency
ORDER BY gross DESC, currency ASC
```

Rows whose `currency` is empty (`''`) and rows declaring `LT` come back in this
result; the tile keeps them out of the totals and reports them as
`excluded_rows` / `excluded_currencies`. Filter them the same way in your own
query (`WHERE currency NOT IN ('', 'LT')`) — a credit balance is not revenue.

Finally, the derived milestone — when each person became a paying customer:

```sql
SELECT person_id, min(occurred_at) AS paid_at
FROM money_rows
WHERE write_rank = 1
  AND event_name = 'revenue'
  AND NOT (event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0)
  AND amount > 0
  AND currency <> ''
  AND currency <> 'LT'
GROUP BY person_id
ORDER BY paid_at ASC, person_id ASC
```

That is the whole definition: the earliest booking that survived de-duplication,
carried a currency, and was not money coming back. A refund does not erase the
milestone, a correction replaces the row it corrects (so the corrected write's
timestamp is the milestone), and two ids for one person are one person
(`canonical_distinct_id`, the same stitching every other person read uses).

---

## Emitting money

### Server (`@agentray/server` ≥ 0.2.0)

The sanctioned path for money, because the server is where billing truth already
lives:

```ts
import { AgentRayServerClient } from '@agentray/server';

const ar = new AgentRayServerClient({ apiUrl, apiKey: process.env.AGENTRAY_API_KEY! });

await ar.revenue('user-123', { amount: 1900, currency: 'USD', kind: 'subscription', plan: 'pro' }, {
  idempotencyKey: webhook.id,          // required
});

await ar.revenueReversed('user-123', { amount: 1900, currency: 'USD' }, {
  idempotencyKey: `refund:${refund.id}`, // its own key
});
```

`idempotencyKey` is required on both, `amount` must be an integer, `currency`
must be non-empty, and a reversal's `amount` must be positive; the SDK throws
rather than write a row the read would book as zero.

### Browser (`@agentray/browser` ≥ 0.2.0)

Money created *in the browser* (a client-side purchase flow the server cannot
see) goes through the ordinary identity-aware capture — there is no privileged
browser money helper, because a browser is forgeable and its claims about money
are worth exactly as much as its other claims:

```ts
import { init, REVENUE_EVENT } from '@agentray/browser';

const ar = init({ host, apiKey });

ar.capture(REVENUE_EVENT, {
  amount: 1900,
  currency: 'USD',
  kind: 'in_app_purchase',
  $insert_id: receipt.transactionId,   // the caller owns this key
});
```

The SDK supplies `distinct_id`, `timestamp` and `platform`; the caller supplies
`$insert_id`, and it must be stable — the read de-duplicates on it.

### Raw HTTP

Any language, any provider webhook:

```bash
curl -X POST "$AGENTRAY_URL/capture" -H 'Content-Type: application/json' -d '{
  "api_key": "'"$AGENTRAY_API_KEY"'",
  "event": "revenue",
  "distinct_id": "user-123",
  "properties": {"amount": 50000, "currency": "VND", "kind": "wallet_topup", "$insert_id": "topup:9"}
}'
```

`POST /batch` takes an array of the same objects with the API key once. Ingest is
deliberately permissive: it requires a `distinct_id` and stores whatever name and
properties you send. It does not validate money, and it does not de-duplicate —
the read does that, once, so every consumer sees the same number.

Python (`capture(...)`) and Swift (`AgentRay.shared.capture(...)`) emit the same
contract through their ordinary capture call; neither has a money helper yet.
