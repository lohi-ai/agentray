# AgentRay Quickstart

Clone → **first event visible + first agent answer** in under 15 minutes. This is
the canonical path a newcomer follows; it is also the source content for the docs
site (`website/`).

## 1. Run the stack (≈3 min)

```bash
git clone <repo> && cd agentray
docker compose up
```

`docker compose up` starts the API, web app, ClickHouse, Postgres, Redis, and
NATS. On first boot it seeds a default project **and ~2 days of synthetic events**
(`AGENTRAY_SEED_DEMO=true` in compose), so the dashboards render populated instead
of empty. Open <http://localhost:3200>.

- Web: <http://localhost:3200>
- API: <http://localhost:8088>
- Default project API key: `lohi_dev_project_token`

> Self-hosting for real? Unset `AGENTRAY_SEED_DEMO` so no demo data is written.

Prefer your own account over the seeded default (or you're an agent working
headless)? The CLI is self-serve:

```bash
make cli                                        # builds ./agentray
./agentray signup --email you@example.com       # account + workspace + project
export AGENTRAY_API_KEY=$(./agentray key)       # project API key, no web app needed
```

## 2. See your first real event (≈2 min)

Send one from the terminal:

```bash
curl -X POST http://localhost:8088/capture \
  -H 'Content-Type: application/json' \
  -d '{"api_key":"lohi_dev_project_token","event":"hello_agentray","distinct_id":"you","properties":{"source":"quickstart"}}'
```

Refresh **Events / Web analytics** in the web app — your `hello_agentray` event
appears alongside the seeded demo funnel.

Instrument a real app instead:

```ts
// Browser — npm install @agentray/browser
import { init } from '@agentray/browser';
const ar = init({ host: 'http://localhost:8088', apiKey: 'lohi_dev_project_token', autocapture: true });
ar.capture('signup', { plan: 'free' });
```

```python
# Python — pip install agentray
from agentray import Client
Client(host="http://localhost:8088", api_key="lohi_dev_project_token").capture(
    "signup", distinct_id="you", properties={"plan": "free"})
```

## 3. Get your first agent answer (≈3 min)

A new workspace is seeded with a **Growth Lead** agent. Open the **Chat** tab and
ask a question about the seeded data:

> "What's our week-1 retention, and where is the funnel leaking?"

The agent runs a retention insight and a funnel over your events and answers with
a chart. Ask it to pin the chart and it builds a dashboard.

## 4. Connect an external agent over MCP (optional, ≈4 min)

AgentRay exposes its analytics operations over MCP at `POST /mcp`. From Claude
Code:

```bash
claude mcp add agentray --transport http http://localhost:8088/mcp \
  --header "X-API-Key: lohi_dev_project_token"
```

Now Claude Code can run funnels/retention/SQL and pin dashboards directly. See
[README.md](../README.md#ai-agents--mcp) for the full operation list.

## Where to go next

- **Autopilot** — add a weekly schedule trigger to the Growth Lead and it runs
  the PMF loop unattended (see [DESIGN-GROWTH-AUTOPILOT.md](DESIGN-GROWTH-AUTOPILOT.md)).
- **Alerts** — set a rule that notifies Slack when a metric breaks (Alerts tab).
- **Budgets** — cap an agent's daily spend on its setup page.
- **Migrating from PostHog** — the `capture`/`batch`/`identify` payloads match;
  change only the host.
