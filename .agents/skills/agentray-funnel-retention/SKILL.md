---
name: agentray-funnel-retention
description: "Build a conversion funnel or a retention readout from AgentRay event data and pin it to a dashboard."
---

# AgentRay Funnel & Retention

## Goal

Answer "where do users drop off?" (funnel) or "are users coming back?"
(retention) with a real chart from an AgentRay project, then optionally pin it so
the team tracks it. Requires the AgentRay MCP connection — see the
`agentray-analytics` skill for setup.

## Required inputs

- A connected AgentRay MCP server (project API key).
- For a funnel: the ordered steps (e.g. landing → read chapter → subscribe). If
  unclear, derive candidate event names from `explore_events`.
- For retention: the window (default a sensible recent range) and the
  return-defining event.

## AgentRay MCP tools

- `explore_events`: confirm the **real** event names and their volume before
  building anything. Never assume an event name exists.
- `run_insight`: the workhorse.
  - `type: funnel` with ordered `steps` → per-step conversion %.
  - `type: retention` → a cohort retention curve.
- `list_dashboards` / `create_dashboard` / `create_chart`: pin the result.
- `submit_recommendation`: file a fix for the weakest step / steepest drop.

## Workflow

### Funnel

1. `explore_events` to lock the exact ordered event names and confirm each has
   volume. If a step has near-zero events, flag the instrumentation gap instead
   of reporting a misleading 0% conversion.
2. `run_insight` `type: funnel` with the ordered steps.
3. Report each step's conversion %, then name the **single weakest step** and one
   concrete idea to lift it.
4. If worth keeping, ask the user, then `create_chart` onto a "Funnels"
   dashboard (`list_dashboards` first; `create_dashboard` if none fits).
5. Optionally `submit_recommendation` (category `growth`) for the weakest step,
   carrying the conversion numbers as evidence.

### Retention

1. `run_insight` `type: retention` over a sensible window.
2. Summarize in one line: the week-1 number, the trend vs. the prior cohort, and
   the single biggest drop-off point.
3. If worth keeping, ask, then `create_chart` onto a "Retention" dashboard.

## Output format

- The headline number first (overall conversion, or week-1 retention).
- The weakest step / steepest drop, named explicitly.
- The next action (pin, recommend, or investigate an instrumentation gap).

## Guardrails

- **Confirm event names first.** A funnel built on a wrong event name produces a
  confident, wrong chart — the worst outcome.
- **Do not invent metrics.** Report only what `run_insight` returned; write
  `unknown` when a value is missing.
- **Verify before you pin.** The `run_insight` call you just made *is* the chart's
  backing query — confirm it returned data before `create_chart`. Never pin an
  unverified, erroring, or empty funnel/retention chart.
- **Confirm before pinning.** `create_dashboard`, `create_chart`, and
  `submit_recommendation` are durable — state the action and get a go-ahead.
