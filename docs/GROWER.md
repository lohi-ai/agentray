# Running the grower archetype on AgentRay

How and when to use the **grower** workflow from the [babysit](https://github.com/reallongnguyen/babysit)
skill pack against this repo. Grower is one of babysit's five lifecycle
archetypes (prototype → build → sweep → **grow** → maintain). Its mandate:
*the product already ships, but the funnel underperforms — measure first, then
run one reversible experiment.*

## When to use it

Use grower only when all three hold:

- Something is **already shipped and usable** — a landing page, signup flow,
  onboarding sequence. Not a feature still being built.
- There is a **metric to move** — signups, activation rate, conversion,
  retention.
- The goal is to change *how well the existing thing performs*, not add
  capability. New feature work is `builder`; code cleanup is `sweeper`;
  scale/reliability pressure is `maintainer`.

If no metric or target surface can be named, grower stops with `NEEDS_CONTEXT`
instead of guessing — don't reach for it pre-launch.

## How to invoke

From this repo, two ways:

**1. Full workflow (checkpointed, survives context loss):**

```
/bbs:autopilot grower "signup conversion on the landing page feels low — rank experiments"
```

It reads positioning + funnel context, names the metric, runs the matching
sub-skill, and by default **stops at a ranked recommendation**
(`VERDICT: RANKED`). Only when asked to implement does it scaffold the smallest
feature-flagged, reversible variant with exposure/conversion tracking, then
runs `review-pr` + `qa` and hands off for a human to run `/bbs:create-pr`
(`VERDICT: SCAFFOLDED`).

**2. Direct sub-skills**, when the lever is already known:

| Skill | Use for |
|-------|---------|
| `/bbs:growth-experiment` | Rank candidate experiments (ICE), scaffold one A/B test — the default |
| `/bbs:conversion-fix` | Audit/fix an activation surface: landing, pricing, signup, onboarding |
| `/bbs:copy-rewrite` | Headlines, hero text, CTAs, positioning clarity |
| `/bbs:social-content` | Short-form video scripts for acquisition |

## AgentRay-specific targets

- `website/` landing underconverting → `/bbs:autopilot grower "improve
  visitor→signup on the landing page"` or `/bbs:conversion-fix` directly.
- Onboarding-to-activation (dev signs up but never ingests a first event) →
  `/bbs:conversion-fix` with **first event ingested** as the activation metric.
- Positioning unclear → `/bbs:copy-rewrite` on the README hero and website copy.
- Launch content → `/bbs:social-content` for demo-video scripts.

## Dogfooding: measure with AgentRay itself

AgentRay is a growth-loop analytics platform, so grower runs should read
AgentRay's own funnel data instead of guessing. Connect the MCP server
(see README § AI Agents & MCP) and the grower's "measure first" step becomes
real `run_insight` funnels and retention curves over this project's events.

The unattended, scheduled version of this loop is designed in
[DESIGN-GROWTH-AUTOPILOT.md](DESIGN-GROWTH-AUTOPILOT.md) and available today
as the [`agentray-growth-loop`](../.agents/skills/agentray-growth-loop/SKILL.md)
agent skill: one measure → diagnose → hypothesize → recommend → remember cycle
per run, resumable across cycles via agent memory.

## Caveat

Grower assumes measurable traffic. If a surface has no real usage yet, grower
will (correctly) push back for lack of signal — at that stage `builder`
(shipping the funnel) or `office-hours` (sharpening positioning before build)
is the better archetype.
