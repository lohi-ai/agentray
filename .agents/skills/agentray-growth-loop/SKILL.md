---
name: agentray-growth-loop
description: "Run one full growth-loop cycle on AgentRay event data — measure the funnel, find the single weakest link, design the smallest reversible test, file it as an evidence-backed recommendation, and remember the baseline for the next cycle."
---

# AgentRay Growth Loop

## Goal

Run one cycle of the loop AgentRay exists to close:

```
measure ──► diagnose ──► hypothesize ──► recommend ──► learn
   ▲                                                    │
   └────────────────────────────────────────────────────┘
```

Each invocation produces exactly one output: the **single weakest link** in the
product's funnel, one **smallest reversible test** to lift it, filed via
`submit_recommendation` with the numbers as evidence, and a `remember` entry so
the next cycle starts from this baseline instead of re-deriving it. Requires
the AgentRay MCP connection — see the `agentray-analytics` skill for setup.

## AgentRay MCP tools

- `explore_events`: confirm real event names and volume before building any
  insight. Never assume an event name exists.
- `activity_summary`: overall volume/error/latency context for the window.
- `run_insight`: the workhorse — `funnel` for acquisition/activation steps,
  `retention` for return cohorts, `timeseries` for trend vs. prior period.
- `run_sql` (SELECT-only): anything the three insight shapes don't cover, e.g.
  source mix or per-property breakdowns.
- `list_dashboards` / `create_dashboard` / `create_chart`: pin the metric the
  experiment targets, so the next cycle reads it in one call.
- `submit_recommendation`: file the experiment (category `growth`) with
  evidence.
- `remember`: persist the baseline numbers, the hypothesis, and where the loop
  left off.

## Workflow

1. **Resume.** Check conversation/agent memory for a prior cycle's baseline and
   any running experiment. If one exists, start by re-measuring *its* target
   metric and reporting the delta before anything else.
2. **Measure.** Establish the three signal families for a recent window
   (default 7 days, widen if data is thin):
   - *Acquisition*: new distinct persons per day (`persons`, `run_sql`), and
     the top-of-funnel step volume.
   - *Activation*: the signup → first-value funnel (`explore_events` to lock
     event names, then `run_insight` `type: funnel`).
   - *Retention*: `run_insight` `type: retention` with the return-defining
     event.
3. **Diagnose.** Name the **one weakest link**: the funnel step with the worst
   conversion, the steepest retention drop, or the flattest acquisition trend —
   whichever caps growth most right now. One link per cycle; list runners-up in
   one line each, unexplored.
4. **Hypothesize.** Design the smallest reversible test that could lift that
   link: what changes, the expected direction and rough size of the metric
   delta, and how exposure/conversion will be measured. Prefer tests the team
   can flag off in one commit.
5. **Recommend.** After a go-ahead, `submit_recommendation` (category `growth`)
   carrying: the baseline number, the weakest link, the proposed test, and the
   decision metric. Pin the target metric with `create_chart` onto a "Growth
   loop" dashboard (`list_dashboards` first) so the next cycle reads it
   directly.
6. **Learn.** `remember` the baseline values, the hypothesis, and the open
   experiment, so the next invocation resumes at step 1 instead of starting
   cold.

## Output format

- The delta on any previously running experiment (or "no prior cycle").
- The three headline numbers: acquisition trend, activation conversion, week-1
  retention — each with its window.
- The single weakest link, named explicitly, with the number that condemns it.
- The proposed test: change, expected delta, decision metric, reversal path.
- What was filed and remembered.

## Guardrails

- **One weakest link per cycle.** Resist fixing three things; the loop's value
  is compounding focus, not coverage.
- **Confirm event names first.** A funnel built on a wrong event name produces
  a confident, wrong diagnosis — the worst outcome.
- **Do not invent metrics.** Every number must come from a tool call made this
  cycle; write `unknown` / `no data` rather than guessing. If activation events
  are missing entirely, the recommendation becomes "fix instrumentation", not a
  growth test.
- **Verify before you pin.** Run the exact backing query and confirm it returns
  data before `create_chart`.
- **Confirm before side effects.** `submit_recommendation`, `create_dashboard`,
  `create_chart`, and `remember` are durable — state the action and evidence,
  and get an explicit go-ahead first. When running unattended under a
  configured autonomy policy, that policy is the go-ahead; log what was filed.
- **SELECT-only.** `run_sql` is read-only; the loop acts on the product only
  through recommendations, never by writing data.
