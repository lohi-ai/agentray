---
name: add-frontend-event
description: >
  Guideline for adding, renaming, or removing an analytics event on the
  frontend of an app instrumented with AgentRay. Use this skill whenever the
  user asks to track something in the UI — "track this button", "add an event
  when…", "measure how many people see/click/open X", "instrument this page",
  "log when the user does Y" — even if they never say the word "event" or
  "analytics". Also use it when reviewing a diff that touches analytics
  emitters or the tracking plan.
---

# Add a frontend analytics event

AgentRay-instrumented frontends follow one contract: **autocapture handles the
generic signals for free; typed emitters handle everything with semantics.**
Adding an event the wrong way creates charts that silently break, split person
timelines, or leak PII — so walk the steps in order.

## Step 0 — Maybe you don't need an event at all

The autocapture SDK (`sdk/browser/autocapture.ts`, installed once per app)
already emits:

| Event | Properties | Answers |
|---|---|---|
| `user.pageview` | `path`, `$referrer`, `title` | "How many people visit page X?" — on load and every SPA navigation |
| `$autocapture` | `tag`, `label`, `href`, `path` | "Do people click this link/button?" |
| `element_viewed` | `label`, `path` | "Does this section ever get seen?" — fires once at ≥50% visibility for `[data-track-view="label"]` |

Decision rule — use **autocapture** (no code beyond maybe a markup attribute)
when the question is "was this page viewed / element clicked / element seen"
and nothing more. Add a **typed event** when any of these hold:

- The event needs **structured properties** (amounts, ids, counts, enum
  states) — autocapture can't carry them, and you can't reconstruct
  `{ amountVnd: 50000 }` from a button label.
- The event feeds a **funnel, revenue, or conversion** question. Autocapture
  labels come from rendered UI text; rewording a button silently splits the
  chart. Typed event names are a stable contract.
- The event needs **group attribution** (e.g. which novel/product/workspace
  it belongs to).
- The event distinguishes **intent states** (`added`/`removed`,
  `accepted`/`rejected`, source surface) that a raw click can't.

If autocapture covers it, stop here. Optionally improve the signal with
markup: `data-track="label"` (explicit click label), `data-track-view="label"`
(visibility), `data-track-ignore` (mute a subtree — wrap PII-bearing UI in
this). Do **not** add a typed event that duplicates a pageview or click with
zero extra properties.

## Step 1 — Register it in the tracking plan

The tracking plan JSON (in web: `web/.analytics-events.json`) is the
contract. Add the event in the **same commit** as the emitter, matching the
file's existing compact style:

```json
"donate_clicked": {
  "description": "Reader tapped a donate amount on the novel page. Intent event — the API emits the authoritative donation_completed.",
  "emitters": ["web/lib/analytics/events.ts"],
  "properties": { "novelSlug": "string", "amountVnd": "number" },
  "groups": ["novel"]
}
```

Renaming or removing an event updates this file in the same commit too — a
stale registry is worse than none.

## Step 2 — Emit from the right side

- **Browser** emits *intent*: button clicked, sheet opened, form submitted.
- **API** emits *authoritative outcomes*: money captured, order settled,
  subscription renewed. Never emit a revenue event from the client — payment
  IPNs/webhooks are the source of truth.

If the event is server-authoritative, this skill doesn't apply — add it to the
API emitter module instead. A common pattern is a pair: client
`donate_clicked` (intent) + server `donation_completed` (outcome), which gives
funnels a conversion rate.

## Step 3 — Write the typed emitter

All capture calls live in **one file** (in web:
`web/lib/analytics/events.ts`). Never call the analytics client's
`capture` from a component, hook, or route — one emitter function per event:

```ts
export function trackDonateClicked(p: {
  novelSlug: string;
  amountVnd: number;
}) {
  capture('donate_clicked', { ...p, ...novelGroup(p.novelSlug) });
}
```

Rules, and why:

- **snake_case name, past-tense verb** (`donate_clicked`, not `clickDonate`) —
  consistency makes the event explorer scannable.
- **No PII in property values.** Emails, phone numbers, raw form input are
  out; ids and amounts are in. Property values land in ClickHouse unredacted.
- **Group-scoped events attach `$groups`.** In web every novel-scoped
  event passes `novelSlug` through the `novelGroup(slug)` helper — forget it
  and the event disappears from per-novel analytics.
- **Emitters no-op when analytics isn't initialized** — guard through the
  shared `capture()` wrapper so local dev without a token works.

## Step 4 — Call it from exactly one place

Two call sites is a smell: it means the same user action is modeled twice and
the counts will drift. Lift state up or emit from a shared hook instead.
`useEffect(() => { trackFooViewed(...) }, [])` for view events; the action
handler for click events.

## Removing an event

When autocapture (or a better event) makes an old one redundant, remove all
three pieces in one commit: the emitter function, its call site(s), and the
tracking-plan entry. Leave the historical rows alone — ClickHouse data is
append-only; old charts just stop receiving points.

## Checklist (verify before declaring done)

1. Autocapture genuinely can't answer the question (Step 0 reasoning stated).
2. Tracking-plan entry added/updated in the same commit, JSON still valid.
3. Emitter in the single emitter file; no direct `capture` call anywhere else
   (`grep` for the client's capture outside that file should only hit the
   emitter file and the autocapture sink).
4. Exactly one call site.
5. Group attached if the event is group-scoped; no PII in properties.
6. Typecheck passes.
