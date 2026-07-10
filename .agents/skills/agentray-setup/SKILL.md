---
name: agentray-setup
description: "Integrate an app with AgentRay the correct way, end to end: project + API key, SDK install (browser/server/python), event instrumentation that follows the tracking contract, dashboards, and agents (in-app and MCP). Use whenever a user wants to 'add AgentRay', 'set up analytics', instrument a new app, or wire up agents on their data."
---

# AgentRay Setup

## Goal

Take an app from zero to a working AgentRay integration, in the order that
prevents rework: **project → SDK → events → dashboards → agents**. Each stage
ends with a verification step; do not advance past a stage that doesn't verify.
Doing these out of order (dashboards before events exist, funnels before event
names are stable) produces confident, wrong charts.

## Stage 0 — Project and API key

- **Local/self-hosted:** `docker compose up` starts API (`:8088`), web
  (`:3200`), ClickHouse, Postgres, Redis, NATS. First boot seeds a default
  project with key `lohi_dev_project_token` and (when `AGENTRAY_SEED_DEMO=true`)
  ~2 days of synthetic events. Self-hosting for real: unset
  `AGENTRAY_SEED_DEMO`.
- **Existing instance:** get the project API key from Settings → project →
  API key. One key = one project; every SDK call and MCP call is scoped by it.
- **Headless / agent path (no web app):** the `agentray` CLI is self-serve
  (`make cli` in the repo, or `go install github.com/lohi-ai/agentray/cmd/cli@latest`):

  ```bash
  agentray --url <host> signup --email you@example.com   # or `login` on an existing account
  export AGENTRAY_API_KEY=$(agentray key)                # bare project key on stdout
  ```

  Password comes from `--password`, `AGENTRAY_PASSWORD`, or a hidden prompt.
  The session and default project key persist in `~/.agentray/config.json`, so
  subsequent CLI operations (`agentray activity_summary '{"hours":24}'`, …)
  need no flags. `agentray key --rotate` rotates a leaked key; `agentray
  projects` lists projects; `--project <name-or-id>` selects one.
- **Verify:** `curl <host>/healthz`, then send one event:

```bash
curl -X POST <host>/capture -H 'Content-Type: application/json' \
  -d '{"api_key":"<key>","event":"hello_agentray","distinct_id":"setup-check","properties":{"source":"setup"}}'
```

It must appear in the web app's Events tab (or `GET /api/events?api_key=...`).

## Stage 1 — SDK install

Pick per surface; an app commonly needs browser **and** server.

- **Browser** (`@agentray/browser`): one `init()` at app entry wires identity,
  batching, retries, and `sendBeacon` flush. Turn autocapture on — it is the
  free tier of instrumentation:

  ```ts
  import { init } from '@agentray/browser';
  const ar = init({ host: '<host>', apiKey: '<key>', autocapture: true });
  ```

  `init()` returns the client, but app code should **not** call `ar.capture`
  from components — wrap it once in a typed emitter (Stage 2, rule 4) and call
  `ar.identify(userId, props)` at login so anonymous and identified activity
  merge into one person timeline.

  **Vendored install (no npm dependency).** Some apps — including kiem-lai
  `web/` — do not add `@agentray/browser` as a dependency; they **copy** the
  SDK source in and keep it current with a sync script. If the app already has
  a `lib/analytics/` (or similar) that a script like `bun run sync:sdk` refreshes
  from `agentray/sdk/browser/`, extend that instead of installing the package —
  looking for a `@agentray/browser` dep that isn't there is the common misstep
  on an already-integrated app. The wrapper typically also routes events through
  a **same-origin ingest proxy** (e.g. a `/ingest/*` rewrite) rather than posting
  to `<host>/capture` directly, so the host stays out of client code and events
  survive ad blockers. Match the existing convention; don't introduce a second one.
- **Server** (Node, `sdk/server/`): the sanctioned path for **revenue truth**
  — payments, subscriptions, refunds. Calls are awaitable and throw (a dropped
  payment event must be retryable), identity is explicit, and every revenue
  event carries an `idempotencyKey` (the payment provider's event id, sent as
  `$insert_id`) because webhooks retry. API key lives in a server env var,
  never in client code.
- **Python** (`agentray`): non-blocking server-side capture with a background
  batch thread; `Client(host=..., api_key=...).capture(...)`, `flush()` on
  shutdown.

All three speak the same PostHog-compatible `capture`/`batch`/`identify`
payloads — migrating existing PostHog instrumentation means changing only the
host.

**Verify:** one event from each installed SDK visible in the Events tab.

## Stage 2 — Instrument events on the app

This is where integrations go wrong. Deciding **which** events to capture, why
each earns its place, and which chart/ask-AI question/SQL query will consume it
is its own skill — `agentray-instrument`; use it to draft the tracking plan
before writing emitters. The mechanical contract (in full in the
`add-frontend-event` skill, if the repo carries it):

1. **Autocapture first.** `user.pageview`, `$autocapture` clicks, and
   `element_viewed` (via `data-track-view="label"`) already answer "was this
   page viewed / element clicked / section seen". Improve signal with markup
   only: `data-track="label"`, `data-track-ignore` (wrap PII-bearing UI in
   this). Do not add a typed event that duplicates a click with zero extra
   properties.
2. **Typed events for semantics.** Add one when the event needs structured
   properties (amounts, ids, enum states), feeds a funnel/revenue/conversion
   question (autocapture labels break when UI copy changes), needs group
   attribution, or distinguishes intent states.
3. **Naming:** `snake_case`, past-tense verb — `donate_clicked`, not
   `clickDonate`.
4. **One emitter file per app** (e.g. `lib/analytics/events.ts`) — one exported
   function per event; never call `capture` from components or routes
   directly. Exactly one call site per event.
5. **Browser emits intent, server emits outcomes.** `donate_clicked` from the
   client; `donation_completed` from the payment webhook. Never emit revenue
   from the browser.
6. **No PII in properties** (ids and amounts in; emails, phones, raw form
   input out — property values land in ClickHouse unredacted). Attach
   `$groups` on group-scoped events or they vanish from per-group analytics.
7. **Tracking plan.** If the app keeps an event registry (e.g.
   `.analytics-events.json`), register every add/rename/remove in the same
   commit as the emitter.

Instrument the app's **activation funnel first** (the 3–5 steps from landing
to first value), then revenue outcomes, then breadth.

**Verify:** exercise the flow once; confirm each new event arrives with the
expected properties via the Events tab or `explore_events` (MCP). Typecheck
passes; `grep` shows no stray `capture` calls outside the emitter file.

## Stage 3 — Dashboards

- Every new project is auto-seeded with a **Product overview** dashboard
  (event trend, top events, sessions, agent cost) — don't rebuild it.
- Add the app's own views once real events flow: the activation funnel and a
  retention curve are the two that matter first. Build via the web app's
  insight builder or templates, or via MCP (`run_insight` → `create_chart`).
- **Verify before you pin:** run the exact backing query (`run_insight` /
  `run_sql`) and confirm it returns data before `create_chart`. Confirm real
  event names with `explore_events` first — a funnel on a misspelled event
  name renders a confident 0%.

## Stage 4 — Agents

Two integration surfaces; set up the one the user needs (often both):

- **External agent over MCP** (Claude Code, Codex): connect once —

  ```bash
  claude mcp add --transport http --header "X-API-Key: <key>" agentray <host>/mcp
  ```

  then install the companion skills so the agent drives the tools correctly:
  copy `.agents/skills/*` into `~/.claude/skills/` (or `~/.codex/skills/`).
  Start with `agentray-analytics`; `agentray-growth-loop`,
  `agentray-funnel-retention`, and `agentray-incident-triage` cover the
  recurring jobs.
- **In-app agents (AgentGarden):** a new agent is a **config change, not a
  backend PR** — persona (`SOUL.md` + `AGENTS.md`), approved skills, enabled
  tools/scopes, write-only secrets referenced as `{{cred:NAME}}`, and
  triggers (chat / manual / schedule / webhook). Every workspace seeds a
  **Growth Lead** agent; test it in the Chat tab against real events before
  creating custom agents. To run the growth loop unattended, add a schedule
  trigger to the Growth Lead; cap spend with a budget on the agent's setup
  page.

**Verify:** ask the connected agent one real question ("what's our week-1
retention?") and confirm the answer cites numbers from a tool call, not from
memory.

## Output format

Report per stage: what was installed/configured, the verification evidence
(event names seen, dashboard/chart created, agent answer), and what remains.
If a stage fails verification, stop there and say what's blocking — don't
continue building on an unverified layer.

## Guardrails

- **Never advance past a failed verification.** Dashboards on missing events
  and agents on empty projects waste every later step.
- **Server key stays server-side.** Only the browser SDK's project key ships
  to clients; revenue events come only from the backend with idempotency keys.
- **Don't over-instrument.** Autocapture plus a handful of typed
  funnel/outcome events beats fifty ad-hoc events nobody charts. Every typed
  event needs a question it answers.
- **Confirm before durable side effects.** Creating dashboards/charts and
  enabling scheduled agents are durable — state the action and get a
  go-ahead (or follow the configured autonomy policy when unattended).
- **One project per key.** To set up a second app/project, get its key and
  reconnect; never mix events from two products into one project.
