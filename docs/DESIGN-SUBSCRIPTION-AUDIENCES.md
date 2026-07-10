# Subscription-aware audiences — a careful model for paid/premium cohorts

**Status:** design. **Scope:** how AgentRay should model the *paid / subscription*
signal so cohort audiences are correct across products with different billing
shapes, and so subscription lifecycle (start, **end date**, **renewal**, cancel,
trial) is first-class. Builds on the shipped Cohort Analysis feature
(`internal/storage/store.go` `Store.Cohorts`, `modules/cohorts/`).

## 1. The problem with what shipped (v1)

The first cut models paid/premium as a **static, all-time, monotonic** person
attribute, derived in the `firsts` CTE of the cohort rollup:

```sql
max(paid_event)                         AS is_paid   -- ever fired revenue, amount>0
argMaxIf(plan_value, timestamp, …)      AS plan      -- latest plan property seen
```

That is cheap and fine for coarse splits, but it is the wrong **primitive** for a
subscription product, in three independent ways:

1. **"Paid" is a point-in-time state, not a forever flag.** A person who
   subscribes in January and cancels in February stays `is_paid = 1` forever.
   For *retention and churn* — the entire reason a cohort triangle exists — that
   is backwards. The question is "was this person an **active paying subscriber
   during week N**," which an all-time aggregate cannot express.

2. **The signal genuinely differs per product.** AgentRay hosts many products:
   - *One-time purchase* (coin pack, unlock a novel) → a `revenue` event, no
     subscription at all. "Ever paid" is the right model here.
   - *Recurring subscription* → a lifecycle: `trial → active → past_due →
     cancelled → expired`.
   - *Usage-based* → continuous charges, no discrete "plan".
   - And **where the truth lives** varies: some products emit
     `subscription_started/renewed/cancelled` events; some emit only a payment
     event; for many the authoritative state is in Stripe / RevenueCat / SePay,
     never in an in-app event.

3. **A subscription is a timeline, not a boolean.** `started_at`,
   **`current_period_end`**, **renewal**, `cancelled_at`, and a `trial` flag are
   first-class. Without them you cannot answer the questions that matter:
   active-subscribers-over-time, churn cohorts, renewal rate, MRR / net-revenue
   retention.

## 2. The split: two kinds of audience

The v1 code conflates two distinct concepts. Name them and keep both:

| | **Static-trait audience** (v1, keep) | **Subscription-state audience** (this doc) |
|---|---|---|
| Backed by | all-time event aggregate | per-person subscription *timeline* |
| Answers | "ever paid", "latest plan = X" | "active subscriber **at week N**", "churned", "trialing", "plan **at t**" |
| Cost | one scan, already built | a normalized projection + point-in-time join |
| Good for | one-time-purchase products, coarse splits | recurring-subscription products, churn/renewal |

Static-trait audiences are the existing `paid` / `plan` kinds (see
`ProjectAudience.compilePredicate`). They stay. This doc adds the second tier.

## 3. The core: a normalized subscription projection

Introduce one internal abstraction — a per-person **subscription timeline**:

```
subscription(canonical_id, plan, status, started_at,
             current_period_end, cancelled_at, is_trial)
```

stored as **intervals** so it answers point-in-time questions:

```
status_at(t)   -- active | trialing | past_due | cancelled | expired | none
plan_at(t)     -- the plan in effect at instant t
is_active(t)   -- status_at(t) ∈ {active, trialing}  (or active-only, configurable)
```

Everything else — audiences, churn, renewal rate, MRR retention — is a query over
this one projection. The projection is the contract; its **source is pluggable**.

### Source A — event-mapped (in-app, ships first)

This is also how "the signal differs per product" becomes **config, not code**.
Per project, a mapping says which events are lifecycle transitions and which
properties carry the fields:

```jsonc
{
  "start_event":      "subscription_started",   // or "purchase", "revenue"
  "renew_event":      "subscription_renewed",
  "cancel_event":     "subscription_cancelled",
  "plan_prop":        "plan",                    // property holding the plan id
  "amount_prop":      "amount",
  "period_end_prop":  "current_period_end",      // ISO ts in the event payload
  "trial_prop":       "is_trial",
  "active_grace_days": 0                          // tolerance past period_end
}
```

The projection is then derived per `canonical_id` by folding that product's events
in timestamp order: `start`/`renew` open or extend an interval to
`current_period_end`; `cancel` closes it; trial events flag `is_trial`. A product
with no lifecycle events but a recurring `revenue` event can synthesize periods
from payment cadence + `active_grace_days`.

This reuses the existing identity stitching (`identityResolver`, canonical id) and
the per-project config pattern already used by `cohort_audiences`.

### Source B — external billing DB (authoritative, future)

A synced table — `person_subscriptions(canonical_id, plan, status, started_at,
current_period_end, cancelled_at, is_trial, source)` — populated from
Stripe / RevenueCat / SePay by a connector (or pushed via the ingestion API).
This is the **authoritative** source and *natively* carries end-date, renewal, and
status, so it does not depend on the product remembering to emit clean events.

**Precedence:** when both exist for a person, external wins (it is ground truth);
event-mapped fills gaps. The projection is a `coalesce` over sources keyed by
canonical id — the same "external-DB seam" already noted in the v1 code, now made
concrete at the subscription layer instead of bolted onto a single column.

## 4. Audiences over the projection

`ProjectAudience.kind` grows beyond `paid | plan`. New kinds compile to predicates
over the projection (still **structured rules, never raw SQL** — the
`compilePredicate` safety property is preserved; plan values stay escaped):

| kind | meaning | point-in-time? |
|---|---|---|
| `paid` *(v1)* | ever paid | no |
| `plan` *(v1)* | latest plan ∈ set | no |
| `active_subscriber` | `is_active(t)` | **yes** |
| `trialing` | `status_at(t) = trialing` | **yes** |
| `churned` | was active, now `cancelled`/`expired` | **yes** |
| `plan_active` | `plan_at(t) ∈ set` | **yes** |

The crucial change for cohorts: a point-in-time audience is evaluated **per cell**
(`t` = the week-N window), not once over all history. "Of the cohort acquired in
week W, how many were **still actively subscribed** in week N" is then a real
subscription-retention triangle — distinct from the activity-retention triangle
v1 draws. Both are worth offering; they answer different questions.

## 5. New questions this unlocks

Once the projection exists, these are thin queries, not new subsystems:

- **Subscription retention / churn triangle** — `is_active(week N)` per cohort.
- **Renewal rate** — `renew` within `current_period_end + grace` over due subs.
- **Trial → paid conversion** — `trialing` cohort that reaches `active`.
- **MRR / net-revenue retention** — sum `amount` of active subs per cohort week.
- **Plan migration** — `plan_at` transitions (upgrade/downgrade) over time.

## 6. Phasing

- **v1 — shipped.** Event-derived static `paid` / `plan` audiences + activity
  retention. Keep as the cheap tier for one-time-purchase products.
- **v2 — this doc, event-mapped.** Per-project subscription mapping config →
  normalized projection (event source) → point-in-time audience kinds →
  subscription-retention/churn cells. No external dependency. This is where
  end-date / renewal / churn become correct.
- **v3 — external connector.** `person_subscriptions` table + Stripe/RevenueCat/
  SePay sync, coalesced ahead of the event source. Same projection contract, so
  audiences and cohorts need no change.

## 7. Open decisions (resolve before v2 code)

1. **Active = `active` only, or `active ∪ trialing`?** Default `active ∪ trialing`
   with a per-audience toggle; trial-heavy products will want both views.
2. **Synthesizing periods from bare `revenue` events** — cadence inference is
   fuzzy. Safer to require an explicit `period_end_prop` and treat cadence-only
   products as static-trait (v1) until they emit period ends.
3. **Projection materialization** — compute inline in the cohort CTE (simple, but
   re-folds events every query) vs a maintained `subscription_state` table
   (faster, but a refresh path to own). Start inline; promote to materialized only
   if it shows up in query latency.
4. **Grace window** — `active_grace_days` default (proposal: 1–2 days) so a
   late renewal webhook does not flap a subscriber to churned for one day.

## 8. Non-goals

- Not building a billing system or invoicing — AgentRay reads subscription state,
  it is not the system of record.
- Not a new agent or BE per product — subscription mapping is **config**
  (per-project, like `cohort_audiences`), consistent with the governance rule
  that products extend the platform by configuration, not bespoke code.
