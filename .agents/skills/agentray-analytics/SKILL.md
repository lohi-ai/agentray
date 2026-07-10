---
name: agentray-analytics
description: "Answer product questions from real AgentRay event data — activity, funnels, retention, persons — and pin dashboards, via the AgentRay MCP server."
---

# AgentRay Analytics

## Goal

Turn a product question ("are readers coming back?", "where do signups drop
off?", "what broke last night?") into an answer grounded in real event data from
an AgentRay project, using the AgentRay MCP tools. Optionally pin the view to a
dashboard so the team sees it without re-asking.

## Setup

Connect your agent to the project's MCP server once. Authenticate with the
project API key (Settings → project → API key) via a request header:

```sh
claude mcp add --transport http --header "X-API-Key: <project-key>" \
  agentray https://agentray.lohi2.com/mcp
```

Self-hosted: swap the host for your instance. The same key scopes every call to
one project — there is no separate login step.

## AgentRay MCP tools

Read first, then build:

- `activity_summary`: event volume, errors, latency, and cost over a recent
  window (`hours`, default 24). Start here for "how are things?" and incident
  triage.
- `recent_events`: the most recent raw events (`limit` 1-200). Use to eyeball
  what is actually being captured before trusting a roll-up.
- `explore_events`: event-name breakdown and property coverage. Use to find the
  exact event names for a funnel, and to spot data-quality gaps.
- `persons`: identified + anonymous person counts over a window. Use for audience
  sizing.
- `run_insight`: the analytical workhorse — `timeseries`, `funnel`, or
  `retention`. Prefer this over raw SQL for those three shapes; the result renders
  as a chart.
- `run_sql`: arbitrary **SELECT-only** ClickHouse query against the `events`
  table for anything `run_insight` does not cover. Extract JSON props with
  `JSONExtractString(properties, 'key')`.
- `list_dashboards`: see existing boards before creating a new one.
- `create_dashboard` / `create_chart`: pin a worthwhile view. Create the
  dashboard first if none fits, then add charts to it.
- `submit_recommendation`: file a growth/marketing recommendation with the
  evidence behind it.
- `remember`: persist a durable finding for next time.

## Workflow

1. Identify the question's shape: monitoring (`activity_summary` /
   `recent_events`), audience (`persons`), data-quality (`explore_events`), or
   analysis (`run_insight` / `run_sql`).
2. For funnels and retention, first confirm the real event names with
   `explore_events`, then run `run_insight` with the right type and steps.
3. For one-off questions, `run_sql` against `events` (SELECT-only). Keep the
   window tight; widen only if the data is thin.
4. Answer with the number first and a one-line interpretation. Name the single
   biggest driver or drop-off, not five shallow observations.
5. If a view is worth keeping, ask the user, then `create_chart` it onto a
   relevant dashboard (`list_dashboards` first; `create_dashboard` if none fits).
6. When you spot an opportunity, end with `submit_recommendation` carrying the
   evidence, and `remember` durable findings.

## Output format

Lead with the highest-signal result:

- The headline number (and the window it covers).
- The trend vs. the prior period, if known.
- The single biggest driver / drop-off / anomaly.
- Suggested next action (pin a chart, file a recommendation, dig deeper).

## Guardrails

- **Do not invent metrics.** Every number must come from a tool call you made
  this turn. If a query returns nothing, write `unknown` / `no data` — never
  guess or recall a figure from memory.
- **Verify before you pin.** Before `create_chart`, run the exact query that will
  back it (`run_insight` or `run_sql`) and confirm it returns data. Never pin a
  chart from an unverified, erroring, or empty query.
- **SELECT-only.** `run_sql` is read-only; never attempt a write.
- **Confirm before side effects.** `create_dashboard`, `create_chart`,
  `submit_recommendation`, and `remember` are durable. State the exact action and
  its evidence, and get an explicit go-ahead before calling them.
- **One project per key.** Every tool call is scoped to the API key's project; to
  analyze another project, reconnect with that project's key.
