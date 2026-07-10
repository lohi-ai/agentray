---
name: agentray-instrument
description: "Design an app's AgentRay tracking plan: decide WHICH events to capture, WHY each one earns its place, and plan the consumer for every event — the chart, dashboard, ask-AI question, SQL query, or alert that will read it. Use when someone asks 'what should we track?', 'instrument this app', or an integration has events nobody charts. Mechanics of adding one event live in add-frontend-event; end-to-end integration order lives in agentray-setup."
---

# AgentRay Instrumentation Plan

## Goal

Produce a tracking plan where **every event has a named consumer** — the chart,
agent question, SQL query, or alert that will read it. An event nobody consumes
is noise that costs storage and drowns the agent; a consumer nobody instrumented
is a confident empty chart. Design both sides together, then instrument.

The one-line test for any proposed event: *"which question does this answer,
and where will the answer be looked at?"* No answer → don't capture it.

## Which events to catch, and why

Work down this ladder in order. Autocapture is layer 0 and is already on
(pageviews, clicks, `data-track-view` visibility) — never hand-write an event
that duplicates it with zero extra properties.

1. **Activation funnel steps** (3–5 events, first). The path from landing to
   first value — e.g. `signup_completed`, `first_chapter_read`,
   `subscription_started`. *Why:* conversion between adjacent steps is the
   single highest-leverage number a product has; the Growth Lead diagnoses the
   weakest link from exactly these events. Browser emits the intent steps,
   server emits the completed ones.
2. **The core action** (one event). The thing a user does when the product
   works for them — `chapter_read`, `message_sent`, `report_generated`. *Why:*
   retention is meaningless without a return-defining event; every retention
   curve and "active user" count keys off this one name. Choose it once and
   never rename it casually.
3. **Revenue outcomes** (server-side only). `revenue` with `amount`,
   `currency`, `plan`, `kind`, and an `idempotencyKey` from the payment
   provider's event id. *Why:* MRR/LTV/conversion all read from the
   conventional `revenue` event; webhooks retry, so the idempotency key is what
   keeps money from being counted twice. Never emit money from the browser.
4. **Decision-bound feature events.** One event per keep/kill/invest decision
   pending — `tts_played`, `dark_mode_enabled`. *Why:* usage is the evidence
   for the decision; when the decision is made, the event can be retired.
5. **Quality signals.** Errors, timeouts, and — for agent/LLM products —
   latency, token cost, and model per call. *Why:* incident triage
   (`agentray-incident-triage`) slices spikes by these properties; without
   them an alert can say "errors up" but never "errors up on model X".

Rules that keep the plan consumable (full contract in `add-frontend-event`):
`snake_case` past-tense names; structured properties (ids, amounts, enum
states — no PII, values land in ClickHouse unredacted); browser = intent,
server = outcome; one emitter module per app; stable `distinct_id` with
`identify()` at login so anonymous and identified activity stitch into one
person.

## Plan the consumers (AgentRay web app)

For each event in the plan, write down its consumer **before** instrumenting.
The four consumer surfaces, and what each demands of the event:

- **Graphs / dashboards** (Insights builder; or MCP `run_insight` →
  `create_chart` → pin). Funnels need the step events to share a
  `distinct_id`; retention needs the core action; timeseries need nothing
  extra. Plan: activation funnel chart + retention curve pinned to the
  project dashboard — these two views justify most of the plan by themselves.
  The auto-seeded Product overview dashboard already covers event trend / top
  events / sessions / agent cost; don't rebuild it.
- **Ask AI** (Chat tab — the seeded Growth Lead; or an external agent over
  MCP). The agent reads event *names and properties* to reason, so
  self-describing names are the interface: `checkout_abandoned` answers
  questions `evt_17` never will. Plan: after events flow, ask "where is the
  funnel leaking?" and "what's week-1 retention?" — if the agent can't answer
  from the plan's events, the plan is missing a step, not the agent.
- **SQL** (SQL page in the web app; `run_sql` over MCP; SELECT-only). Ad-hoc
  slicing via `JSONExtractString(properties, 'plan')` etc. — which is why
  properties must be flat, typed values, not prose. Revenue reads de-duplicate
  by `insert_id`:

  ```sql
  SELECT sum(amount) FROM (
    SELECT argMax(JSONExtractFloat(properties, 'amount'), timestamp) AS amount
    FROM events WHERE event_name = 'revenue' GROUP BY insert_id
  )
  ```

- **Alerts** (Alerts tab). A threshold rule on a metric that should page
  someone — error rate, revenue drop to zero, funnel-step volume collapse.
  Only alert on events with a clear "someone acts on this" answer.

Web analytics (pageviews, sessions) is fed by autocapture for free — it is
never a reason to add typed events.

## Worked example (novel-reading app)

| Event | Layer | Why | Consumer |
|---|---|---|---|
| `signup_completed` | funnel | activation step 2 | funnel chart; ask-AI |
| `first_chapter_read` | funnel + core | first value & return anchor | funnel + retention charts |
| `listen_started` | decision | invest-in-TTS decision pending | trend chart; SQL by `voice` |
| `revenue` (server) | revenue | MRR/LTV; webhook-safe | dashboard; SQL dedup; alert on 0 |
| `tts_error` (server) | quality | triage by `model`, `voice` | alert on rate; incident SQL |

## Workflow

1. Name the product's first-value moment and core action; draft the ladder
   above as a table like the example — event, layer, why, consumer.
2. Confirm what autocapture already answers (`explore_events` on a live
   project); delete any planned event it covers.
3. Get the plan agreed, then instrument via `add-frontend-event` (browser) and
   the server SDK (outcomes/revenue), registering events in the app's tracking
   plan file if it keeps one.
4. Exercise each flow once; verify with `explore_events` that every planned
   event arrives with its properties, then build the planned consumers —
   funnel + retention first (`agentray-funnel-retention`).
5. Close the loop in Ask AI: the two questions from the consumer plan must be
   answerable with tool-call numbers, not memory.

## Guardrails

- Every event needs a consumer named at design time; every consumer needs its
  events named. Refuse "track everything, we'll decide later".
- Start with ≤ 10 typed events. Autocapture plus a sharp funnel beats fifty
  ad-hoc events nobody charts.
- Renaming a live event orphans its history in every chart, retention curve,
  and saved query — treat names as API. If a rename is unavoidable, update the
  consumers in the same change.
- No PII in properties; ids and amounts in, emails and raw input out.
- Revenue only from the server, only with provider idempotency keys.
