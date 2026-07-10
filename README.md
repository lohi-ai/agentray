# AgentRay

**Open-source analytics that runs your growth loop, not just your dashboards.**

Every analytics tool can tell you *what happened*. The real work — *measure →
diagnose → test → learn* — falls to a human who rarely has time to run it, and
the loop stalls at the dashboard. AgentRay ships that loop as the product: the
same event store that powers your charts also powers **agents** that read the
data, find the single weakest link in your funnel, design the smallest test,
remember the result, and pick the thread back up next cycle.

Underneath is a complete product-analytics base you can self-host with one
`docker compose up` — fast Go ingestion, cheap event storage in ClickHouse,
PostgreSQL metadata, and a **PostHog-compatible event model**, so existing
instrumentation migrates by changing only the host. On top sits what other
platforms bolt on: an MCP server and ready-made agent skills, so Claude Code or
Codex works your real event data — from one-off product questions to a
scheduled, unattended growth loop.

## What's inside

The foundation architecture (detailed in `docs/PostHog-clone.md`):

- Go ingestion API built with Echo
- ClickHouse raw event storage plus a materialized session view
- PostgreSQL project metadata, saved queries, and analyst feedback tables
- Redis-backed rate limiting for ingestion endpoints
- NATS-backed asynchronous ingestion from HTTP handlers into ClickHouse
- Next.js dashboard app in `web/` for project keys, activity, dashboards, and
  chart management
- MVP analytics workflows: insight builder, dashboard filters, templates, web
  analytics, event explorer, agent session replay, and saved SQL-lite queries
- PostHog-compatible `capture`, `batch`, and `identify` endpoints so existing
  browser and backend instrumentation can move over incrementally

## Agent runtime as a library

The agent runtime powering AgentRay's growth loop is exported as two reusable Go
packages you can import on their own:

- [`agentcore`](agentcore/) — a provider-agnostic agent loop (Anthropic or any
  OpenAI-compatible gateway), with progressive-disclosure skills, tool policies,
  budget gating, and context compaction.
- [`sandbox`](sandbox/) — read-only workspace tools (`read_file`, `grep`,
  `glob`) for grounding an agent in a repository.

```bash
go get github.com/lohi-ai/agentray@latest
```

[Swatter](https://github.com/lohi-ai/swatter), the validated PR-review bugbot,
is built entirely on these packages.

## Why AgentRay

- Built for AI-first products: agent runs, tool usage, token cost, latency, and
  failures fit the data model instead of feeling bolted on.
- Easier to self-host: Go + ClickHouse + Postgres is simpler to reason about
  than a much larger analytics platform.
- Familiar migration path: it accepts the common event payload shape teams
  already send today.
- Open-source by default: the storage layer and local workflow are readable,
  hackable, and designed for extraction into a standalone repository.

## Current Scope

The current service is the ingestion and storage base layer:

- `POST /capture`
- `POST /batch`
- `POST /identify`
- `GET /api/events`
- `GET /api/sessions`
- `GET /api/activity`
- `GET|POST /api/projects`
- `POST /api/projects/:project_id/rotate-key`
- `GET|POST|PUT|DELETE /api/dashboards`
- `GET|POST /api/dashboards/:dashboard_id/charts`
- `PUT|DELETE /api/charts/:chart_id`
- `GET /api/insights/run`
- `GET /api/templates`
- `POST /api/templates/:template_id/apply`
- `GET /api/web-analytics`
- `GET /api/persons`
- `GET /api/events/explore`
- `GET /api/sessions/:session_id/replay`
- `GET|POST /api/saved-queries`
- `POST /api/saved-queries/:query_id/run`
- `POST /api/sql/run`
- `GET /healthz`

Supported compatibility aliases:

- `POST /e/`
- `POST /e`
- `POST /i/v0/e/`
- `POST /i/v0/e`

Both `api_key` and `token` are accepted for project authentication.

Ingestion requests follow the foundation architecture from
`docs/PostHog-clone.md`:

```text
HTTP API -> Redis rate limit -> NATS queue -> ClickHouse storage
```

Every new project (signup, workspace project creation, and the default local
project) is auto-seeded with a "Product overview" dashboard holding four
predefined charts — event trend, top events, sessions, and agent cost — so the
Dashboards tab gives a readable answer before any custom chart is built.

## SDKs

**Browser — `@agentray/browser`** (`sdk/browser/`). One `init()` wires identity,
batched delivery, retries, and `sendBeacon` flush on unload:

```ts
import { init } from '@agentray/browser';

const ar = init({ host: 'https://agentray.example.com', apiKey: 'phc_...', autocapture: true });
ar.capture('checkout_started', { plan: 'pro' });
ar.identify('user-123', { email: 'alice@example.com' });
```

Autocapture is dependency-free and framework-agnostic: every page reports
pageviews (`user.pageview`, which powers the Web analytics tab), delegated clicks
(`$autocapture`), and `[data-track-view]` visibility (`element_viewed`) with no
per-button wiring. Markup conventions: `data-track="label"` opts an element into
click capture with an explicit label, `data-track-ignore` mutes a subtree, and
`data-track-view="label"` fires `element_viewed` once when the element becomes at
least half visible. See `sdk/browser/README.md`.

**Python — `agentray`** (`sdk/python/`). Non-blocking server-side capture with a
background batch thread; PostHog-compatible payloads:

```python
from agentray import Client
ar = Client(host="https://agentray.example.com", api_key="phc_...")
ar.capture("order_paid", distinct_id="user-123", properties={"amount": 29})
ar.flush()
```

**Node/Bun server — `@agentray/server`** (`sdk/server/`). Awaitable, idempotent
capture for events the browser must not be trusted to send — payments,
subscriptions, refunds:

```ts
import { AgentRayServerClient } from '@agentray/server';
const ar = new AgentRayServerClient({ apiUrl: process.env.AGENTRAY_URL!, apiKey: process.env.AGENTRAY_API_KEY! });
await ar.revenue('user-123', { amount: 19, currency: 'USD' }, { idempotencyKey: webhook.id });
```

All three SDKs speak the same `capture` / `batch` / `identify` payload, so a
PostHog integration migrates by changing only the host.

## CLI

`cmd/cli` builds the `agentray` binary (`make cli`, or
`go install github.com/lohi-ai/agentray/cmd/cli@latest`). It is self-serve from
zero — an agent can create an account and fetch a project API key without ever
opening the web app:

```sh
agentray signup --email you@example.com          # account + workspace + project
agentray login  --email you@example.com          # session saved to ~/.agentray
export AGENTRAY_API_KEY=$(agentray key)          # bare key on stdout
agentray key --rotate                            # rotate and print the new key
agentray projects                                # list projects; `whoami`, `logout`
```

Passwords come from `--password`, `AGENTRAY_PASSWORD`, or a hidden prompt. After
login, the saved server URL and project key become the defaults, so every
operation works with zero flags:

```sh
agentray ops                                     # list operations + schemas
agentray activity_summary '{"hours":24}'
agentray run_sql '{"sql":"SELECT count() FROM events"}'
```

The operation set IS the shared registry — the same definitions the server
exposes as REST, MCP tools, and in-process agent tools.

## AI Agents & MCP

AgentRay exposes its analytics operations to external AI agents (Claude Code,
Codex, any MCP client) over an **MCP server** at `POST /mcp`. The agent can read
activity, run funnels/retention/SQL over your events, and pin dashboards — the
same operations the in-app analyst and the web client use, with no second API to
learn.

### Connect from Claude Code

Authenticate with a project API key (Settings → project → API key) passed as a
request header:

```sh
claude mcp add --transport http --header "X-API-Key: <project-key>" \
  agentray https://agentray.lohi2.com/mcp
```

Codex:

```sh
codex mcp add agentray --url https://agentray.lohi2.com/mcp \
  --header "X-API-Key: <project-key>"
```

Self-hosted: swap the host for your instance (e.g. `http://localhost:8080/mcp`).
The API key scopes every call to one project — there is no separate login step.

### What the agent can do

The MCP tools are projected from the operation registry, so they stay in sync
with the in-app agent and REST API:

- `activity_summary`, `recent_events` — monitoring and incident triage
- `explore_events`, `persons` — data-quality and audience sizing
- `run_insight` (timeseries / funnel / retention), `run_sql` (SELECT-only) —
  analysis
- `list_dashboards`, `create_dashboard`, `create_chart` — pin views
- `submit_recommendation`, `remember` — capture findings

### Agent Skills

Reusable workflow guides for Claude Code / Codex live in
[`.agents/skills/`](.agents/skills). They teach an agent how to drive the MCP
above:

- `agentray-setup` — integrate an app end to end, in the right order: project +
  API key (web app or `agentray signup`/`key` CLI), SDK install, event
  instrumentation contract, dashboards, agents.
- `agentray-instrument` — design the tracking plan: which events to capture,
  why each earns its place, and the consumer (chart, ask-AI question, SQL,
  alert) planned for every event before it is instrumented.
- `agentray-analytics` — start here for questions; connect, then answer product
  questions from real event data and pin dashboards.
- `agentray-growth-loop` — run one full measure → diagnose → hypothesize →
  recommend → remember cycle: find the weakest funnel link, design the smallest
  reversible test, file it with evidence.
- `agentray-funnel-retention` — build a conversion funnel or retention readout
  and pin it.
- `agentray-incident-triage` — investigate an error spike, latency, or cost
  regression from activity and raw events.

Install them into your agent's skills folder:

```sh
# Claude Code
mkdir -p ~/.claude/skills && cp -R .agents/skills/* ~/.claude/skills/

# Codex
mkdir -p ~/.codex/skills && cp -R .agents/skills/* ~/.codex/skills/
```

## Local Development

Start the full local stack:

```bash
docker compose up --build -d
```

Services exposed on your machine:

- API: `http://localhost:8088`
- Dashboard web: `http://localhost:3200`
- ClickHouse HTTP: `http://localhost:18123`
- ClickHouse native: `localhost:19000`
- PostgreSQL: `localhost:5434`
- Redis: `localhost:6389`
- NATS: `localhost:4223`

Default local project token:

```text
lohi_dev_project_token
```

Send a smoke event:

```bash
curl -s http://localhost:8088/capture \
  -H 'Content-Type: application/json' \
  -d '{
    "api_key": "lohi_dev_project_token",
    "event": "agent.tool_call",
    "distinct_id": "local-user",
    "session_id": "session-1",
    "properties": {
      "tool_name": "search",
      "latency_ms": 120,
      "tokens_input": 42,
      "tokens_output": 11
    }
  }'
```

Inspect recent events:

```bash
curl -s 'http://localhost:8088/api/events?api_key=lohi_dev_project_token&limit=10'
```

Inspect recent sessions:

```bash
curl -s 'http://localhost:8088/api/sessions?api_key=lohi_dev_project_token&limit=10'
```

Open the dashboard:

```bash
open http://localhost:3200
```

Run the dashboard app without Docker:

```bash
cd web
bun install
NEXT_PUBLIC_AGENTRAY_API_URL=http://localhost:8088 bun run dev
```

## End-to-End Test

Run the e2e test from the host machine:

```bash
go test -tags=e2e ./internal/app -run TestAnalyticsServiceE2E -count=1 -v
```

If you run the test from inside a container that talks to the host Docker
daemon, point it back at the host-published ports:

```bash
AGENTRAY_E2E_INFRA_HOST=host.docker.internal \
  go test -tags=e2e ./internal/app -run TestAnalyticsServiceE2E -count=1 -v
```

## Schema Notes

The storage model is tuned for the roadmap without overbuilding the MVP:

- Raw events stay append-only in ClickHouse.
- A `sessions_mv` materialized view rolls session aggregates forward as events
  arrive, which keeps common session analytics cheap.
- PostgreSQL keeps relational metadata and adds indexes for the read paths that
  are already present in the design.

## Dependency Baseline

AgentRay currently targets Go `1.25` and the latest dependency set verified in
container build during this migration:

- `github.com/ClickHouse/clickhouse-go/v2 v2.46.0`
- `github.com/jackc/pgx/v5 v5.10.0`
- `github.com/labstack/echo/v4 v4.15.2`
- `github.com/nats-io/nats.go v1.52.0`
- `github.com/redis/go-redis/v9 v9.20.0`
- Next.js `16.1.6`, React `19.2.3`, and Apache ECharts `6.1.0` in `web/`

## Roadmap

The living roadmap — current priorities, acceptance criteria, and sequencing —
lives in [`ROADMAP.md`](ROADMAP.md), with the detailed engineering breakdown in
[`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md).

## Naming

`AgentRay` is the product name because it is short, easy to say, and signals
visibility into agent behavior instead of generic web analytics.

## License

MIT
