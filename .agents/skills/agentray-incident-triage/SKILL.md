---
name: agentray-incident-triage
description: "Investigate an error spike, latency, or cost regression in an AgentRay project from activity and raw events."
---

# AgentRay Incident Triage

## Goal

Given a vague alarm — "errors are up", "it feels slow", "cost spiked" — find the
what, when, and likely why from real AgentRay event data, fast. Requires the
AgentRay MCP connection — see the `agentray-analytics` skill for setup.

## AgentRay MCP tools

- `activity_summary`: start here. Event volume, **error rate**, latency, and cost
  over a window (`hours`, default 24). Compare a short recent window against a
  longer baseline to confirm a regression is real.
- `recent_events`: pull the most recent raw events (`limit` up to 200) and read
  the actual error payloads — do not theorize before you have seen one.
- `explore_events`: see which event names carry the errors and whether a specific
  property (model, route, agent) concentrates them.
- `run_sql` (SELECT-only): slice the spike — group errors by
  `JSONExtractString(properties, 'key')` (e.g. `error`, `model_name`, `agent_id`,
  `route`) over the `events` table to localize the cause.

## Workflow

1. `activity_summary` over a tight recent window (e.g. `hours: 2`), then again
   over a baseline (e.g. `hours: 168`). Confirm the error/latency/cost delta is
   real, not noise.
2. `recent_events` with `error_only` intent — read 5–10 actual failing events to
   see the real message, not a guess.
3. Localize with `run_sql`: `GROUP BY` the most likely dimension (model, route,
   agent, version) and `ORDER BY count() DESC` to find where the spike
   concentrates. Keep it SELECT-only.
4. Report: **what** changed (the metric + magnitude), **when** it started,
   **where** it concentrates (the top dimension), and the **most likely cause**
   from the raw payloads. If the data is inconclusive, say so — do not invent a
   root cause.
5. If a durable signal, `remember` it; if it needs a fix, `submit_recommendation`
   with the evidence (after confirming with the user).

## Output format

Lead with the verdict:

- The metric and magnitude (e.g. "error rate 2% → 19% since 03:10 UTC").
- The dimension it concentrates in (e.g. "94% on model X / route Y").
- The most likely cause, quoted from a real event payload.
- Suggested next step (rollback, fix, keep watching).

## Guardrails

- **See a real error before naming a cause.** Pull `recent_events` payloads;
  never infer a root cause from aggregates alone.
- **Do not invent metrics or causes.** If the data does not localize the spike,
  report "inconclusive" with what you ruled out.
- **SELECT-only**, and confirm before any `submit_recommendation` / `remember`.
