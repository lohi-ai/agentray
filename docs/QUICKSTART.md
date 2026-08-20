# AgentRay Quickstart

Clone → **first event visible + first agent answer** in under 15 minutes. This is
the canonical path a newcomer follows, and the source content for the docs site
when it is built (IMPLEMENTATION-PLAN Phase 3c).

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

Instrument a real app instead. Event names that match `user.pageview` /
`user.signup` / `user.conversion` light the Product funnel without a custom query.
`@agentray/browser` and PyPI `agentray` are **not published yet** — do not
`npm install` / `pip install`. Paste this before `</body>` (same snippet as
`web/modules/start/components/instrument-snippet.tsx`):

```html
<script>
(function () {
  var HOST = 'http://localhost:8088';
  var KEY  = 'lohi_dev_project_token';
  var id = localStorage.getItem('ar_id');
  if (!id) { id = 'a-' + Math.random().toString(36).slice(2) + Date.now().toString(36); localStorage.setItem('ar_id', id); }
  function send(event, props) {
    fetch(HOST + '/capture', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ api_key: KEY, event: event, distinct_id: id, properties: props || {} }),
      keepalive: true
    });
  }
  send('user.pageview', { path: location.pathname });
})();
</script>
```

With a build step, copy `sdk/browser/` or `sdk/python/` into the product repo
(`npm install @agentray/browser` / `pip install agentray` once those packages
are published):

```ts
import { init } from '@agentray/browser';
const ar = init({ host: 'http://localhost:8088', apiKey: 'lohi_dev_project_token', autocapture: true });
ar.capture('user.pageview', { path: location.pathname });
```

```python
# copy sdk/python/ — pip install agentray is not published
from agentray import Client
Client(host="http://localhost:8088", api_key="lohi_dev_project_token").capture(
    "user.signup", distinct_id="you", properties={"plan": "free"})
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
