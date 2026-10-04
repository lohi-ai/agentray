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

Connect your agent to the project's MCP server once. Authenticate with a
scoped, revocable management credential (`agm_…`), never the browser/capture
project key. An investigation needs `analytics:read` plus `sources:read`; add
`dashboards:write` only when the agent should author boards:

```sh
claude mcp add --transport http --header "Authorization: Bearer <agm_…>" \
  agentray https://agentray.lohi2.com/mcp
```

Self-hosted: swap the host for your instance. The credential scopes every call
to one project. Keep source operation (`sources:manage`), finding
(`plans:write`), and memory/notification (`growth:write`) credentials separate
unless the task actually needs those side effects.

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
- `run_sql`: one **SELECT-only** DuckDB query against `events` and/or
  `external_rows` for anything `run_insight` does not cover. Extract event JSON
  with `json_extract_string(properties, '$.key')`. Every external-data CTE must
  filter both `connector_id` and `table_name`; `row_key` is unique only inside
  that pair. Use `canonical_id` for people and aggregate one-to-many facts before
  joining them to people.
- `list_dashboards`: see existing boards before creating a new one.
- `list_metrics` / `read_metric`: the metric catalog — what a board tile may
  reference, in what unit, with which definition and required instrumentation —
  and the read that computes one over a range. Use `read_metric` instead of
  hand-writing SQL for a number the catalog already defines.
- `get_board` / `save_board`: read a board's declared content (sections and
  tiles, with the revision) and declare it back in one write. `save_board`
  replaces the board's content, so read it first. A metric tile's `target`
  object declares the metric's target (direction + value + period, e.g.
  `{direction:"gte", value:40, period:"7d"}`); `set_metric_target` does the
  same directly and is the only way to clear one (`clear: true`).
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
3. For one-off questions, use `run_sql` (SELECT-only). Keep the window tight;
   widen only if the data is thin. Return `date`, `series`, `value`, `unit`,
   `sample_size`, `state`, and `reason` when the query will feed a reusable
   chart or evidence packet.
4. Answer with the number first and a one-line interpretation. Name the single
   biggest driver or drop-off, not five shallow observations.
5. If a view is worth keeping, ask the user, then either declare it onto a board
   (`list_metrics` for the metric, `get_board` for the current revision and
   `save_board` with the whole document) or pin an ad-hoc chart
   (`create_chart` onto a dashboard from `list_dashboards`, creating one with
   `create_dashboard` if none fits).
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
- **Preserve exact numbers.** HUGEINT/DECIMAL values may arrive as strings.
  Keep them exact and carry their unit; do not coerce unsafe integers, nulls,
  NaN, infinity, or unparseable text to zero.
- **Bind saved-chart dates explicitly.** SQL charts on the dashboard must use
  both quoted UTC tokens — `timestamp >= '{{from}}' AND timestamp < '{{to}}'`.
  The end is exclusive. `{{hours}}` is optional but is not a substitute for the
  two absolute bounds; unbound or partial SQL is refused rather than shown as
  though the dashboard filter applied.
- **Confirm before side effects.** `create_dashboard`, `create_chart`,
  `save_board`, `submit_recommendation`, and `remember` are durable. State the
  exact action and its evidence, and get an explicit go-ahead before calling
  them.
- **Declare only what the catalog defines.** A `save_board` tile references a
  metric key from `list_metrics` or a chart that already belongs to the board;
  anything else is refused. Carry the current `revision` from `get_board` — a
  declaration without one is a create and conflicts against an existing board.
- **One project per credential.** Every tool call is server-scoped to the
  management credential's project; SQL cannot select another project. Reconnect
  with a credential belonging to the other project when access is authorized.
- For the full query, chart, access, error, and capacity contract, read
  [`docs/QUERY-ACCESS.md`](../../../docs/QUERY-ACCESS.md).
