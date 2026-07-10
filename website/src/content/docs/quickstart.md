---
title: Quickstart
description: Clone to first event and first agent answer in under 15 minutes.
---

This page mirrors the canonical [`docs/QUICKSTART.md`](https://github.com/lohi-ai/agentray/blob/main/docs/QUICKSTART.md).
Keep the two in sync; the repo file is the source of truth.

## 1. Run the stack

```bash
git clone <repo> && cd agentray
docker compose up
```

First boot seeds a default project plus ~2 days of demo events, so dashboards
render populated. Open <http://localhost:3200> (API on `:8088`, key
`lohi_dev_project_token`).

## 2. Send your first event

```bash
curl -X POST http://localhost:8088/capture \
  -H 'Content-Type: application/json' \
  -d '{"api_key":"lohi_dev_project_token","event":"hello_agentray","distinct_id":"you"}'
```

Or instrument an app with `@agentray/browser` / `agentray` (Python).

## 3. Ask the agent

Open **Chat** and ask the seeded Growth Lead: *"What's our week-1 retention, and
where is the funnel leaking?"* — it answers with a chart and offers to pin it.

## 4. Connect over MCP (optional)

```bash
claude mcp add agentray --transport http http://localhost:8088/mcp \
  --header "X-API-Key: lohi_dev_project_token"
```
