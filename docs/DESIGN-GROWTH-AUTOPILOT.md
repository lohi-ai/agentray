# Growth Autopilot — an autonomous PMF grower, config-only

**Status:** design. **Constraint:** zero new backend code per the
[governance rule](../../CLAUDE.md) — a grower is the *one generic agent runtime*
extended by **config only** (persona + skills + scopes + triggers + secrets +
autonomy). This doc shows that an autonomous product-market-fit grower is fully
expressible on the primitives already shipped in
[AgentGarden](ARCHITECT-AGENTGARDEN.md), [governance](AGENT-GOVERNANCE.md), and
[agent teams](ARCHITECT-AGENT-TEAM.md).

## 1. What "autonomous grower for PMF" means here

Product-market fit is not one number; it is a loop you run continuously:

```
        measure ──► diagnose ──► hypothesize ──► act ──► learn
           ▲                                              │
           └──────────────────────────────────────────────┘
```

A grower agent is an agent that *owns this loop on a schedule* instead of
waiting to be asked. PMF-specifically it watches three signal families and one
verdict:

| Signal | Concretely on kiem-lai | Tool path |
|---|---|---|
| **Acquisition** | new distinct_ids/day, source mix, landing→read | `run_insight` funnel, `run_sql` |
| **Activation** | guest → first chapter read → 3-chapter habit | `run_insight` funnel |
| **Retention** | week-1 / week-4 reader retention cohorts | `run_insight` retention |
| **PMF verdict** | Sean-Ellis "very disappointed" %, + retention plateau | `run_sql` over survey events + cohort flattening |

The grower's job each cycle: refresh those, find the one weakest link, form a
hypothesis, **act within its allowed surface**, and record what it learned so the
next cycle builds on it rather than re-deriving baselines.

## 2. Nothing new in the backend — the mapping

Every capability the loop needs already maps to an existing primitive:

| Loop need | Existing primitive | Where it comes from |
|---|---|---|
| Run on its own cadence | **schedule trigger** (cron) | `agent_triggers`, `TriggerSchedule` |
| React to a metric anomaly / external event | **webhook trigger** (token+HMAC) | `agent_triggers`, `TriggerWebhook` |
| Read events / size segments | `monitor` + `data_quality` scopes | `scopeTools` in `policy.go` |
| Build & pin tracking views | `analyze_build` scope (`create_chart`/`create_dashboard`) | `policy.go` |
| File a tracked decision for a human | `growth_suggest` scope (`submit_recommendation`) | `policy.go` |
| Remember baselines / running experiments across runs | `remember` (memory) | `growth_suggest` scope |
| **Act on the product** (toggle a promo, enqueue a push, flip a flag) | `http_request` to **audited product endpoints** + `{{cred:NAME}}` | governance §Tools |
| Heavier artifacts (reports, exports) | `computer_use` sandbox | [HARNESS-REVIEW](HARNESS-REVIEW.md) |
| Division of labor | `spawn_subagent` / team kanban | [ARCHITECT-AGENT-TEAM](ARCHITECT-AGENT-TEAM.md) (P1+) |
| Human-in-the-loop vs hands-on | `autonomy` = `suggest` \| `auto` | `agent_configs.autonomy` |

The grower is therefore a **marketplace preset** (the sanctioned config-only
blueprint path in `internal/storage/marketplace.go`) plus, optionally, a couple
of **schedule triggers** created in the AgentGarden UI. No handler, no
`*assist` helper, no new tool kind.

## 3. The act-path is the only thing that needs a partner — and it's still config

Reading data and filing recommendations are already covered by scopes. The one
genuinely new thing an *autonomous* grower wants over the existing Growth Analyst
is the ability to **act**, not just suggest. The governance-correct way to do
that without new AgentRay BE is exactly the novel-mod pattern:

- The **product** (`api.lohi2.com`) exposes a small, audited set of
  growth-control endpoints under e.g. `/novel/agent/growth/*` (idempotent,
  scoped, capability-manifested) — the same shape as the shipped
  `/novel/agent/*` moderation surface.
- The grower reaches them with `http_request`, `allow_hosts=[api.lohi2.com]`,
  auth `X-API-Key: {{cred:NOVEL_GROWTH_KEY}}`.
- AgentRay adds **nothing**; the product owns and audits what an agent may flip.

So "can a grower turn on a win-back promo by itself?" is answered entirely by
(a) does the product expose that endpoint, and (b) is the agent's autonomy set
to `auto` for that class of action. Both are config/ops decisions, not AgentRay
code. If a capability genuinely cannot be expressed as a product endpoint +
`http_request`, **stop and ask** before writing BE.

## 4. The roster (config-only)

Start as **one** grower; graduate to a small **team** once `spawn_subagent`/teams
land (P1+ in the team doc). Both are pure config.

### 4.1 Single agent: `growth-autopilot` (marketplace preset)

```
slug:    growth-autopilot
name:    Growth Autopilot
category: growth
scopes:  monitor, data_quality, analyze_build, growth_suggest   (fullAnalystScopes)
+ tools: http_request (allow_hosts=[api.lohi2.com])             (per-agent)
secrets: NOVEL_GROWTH_KEY                                        (write-only)
triggers:
  - schedule  "0 6 * * *"   daily PMF pulse
  - schedule  "0 7 * * 1"   weekly PMF readout + experiment review
  - webhook   (anomaly)     fired by an alerting metric, body = {metric, delta}
autonomy: suggest   (start here; promote specific action classes to auto later)
```

**SOUL.md** (persona) — a growth lead who owns the PMF loop, thinks in cohorts,
never states a number it didn't query, and treats every cycle as "advance the
one weakest link," not "report everything."

**AGENTS.md** (operating procedure) — the loop, made explicit so a *scheduled*
run with no human prompt still knows what to do:

```
On every scheduled run, with no human in the loop:
1. ORIENT. Recall last cycle's state from memory: baselines, the active
   hypothesis, and any experiment still running. If none, this is cycle 0 —
   establish baselines and stop after step 2.
2. MEASURE. Refresh acquisition, activation, retention, and the PMF verdict
   via run_insight / run_sql. Pin/refresh the "PMF" dashboard charts.
3. DIAGNOSE. Find the single weakest link vs baseline and vs last cycle.
   One link per cycle — do not boil the ocean.
4. DECIDE.
   - If an experiment is running: read its result, call it (ship / kill /
     extend), and submit_recommendation with the verdict + evidence.
   - Else: form ONE hypothesis for the weakest link and design the smallest
     test (segment, change, success metric, duration).
5. ACT, within autonomy:
   - autonomy=suggest  → submit_recommendation (category growth) and stop.
   - autonomy=auto AND the action is on the allow-listed product endpoint →
     http_request to /novel/agent/growth/*, then submit_recommendation as an
     audit record of what you did.
6. LEARN. remember the new baselines, the decision, and the running experiment
   so next cycle continues the thread instead of restarting it.
Guardrails apply (never invent a metric; confirm durable side effects unless
autonomy=auto explicitly covers that action class).
```

**Skills** (the PMF playbook — knowledge, not code):

- `pmf-scorecard` — the canonical query set for acquisition/activation/retention
  + the Sean-Ellis verdict, and how to read a retention curve for a plateau
  (the real PMF tell).
- `weakest-link-triage` — turn the scorecard into the one link to attack this
  cycle; tie-break rules so it's deterministic across runs.
- `experiment-design` — smallest viable test: segment from `run_sql`, one
  variable, a pre-registered success metric and duration, written as a
  `submit_recommendation`.
- `experiment-readout` — read a running test's result, decide ship/kill/extend,
  guard against peeking/under-powered calls.
- `growth-act-manifest` — *which* `/novel/agent/growth/*` endpoints exist, their
  inputs, idempotency, and which ones the agent may call under `auto`. This is
  the contract that keeps acting governed and config-only.

### 4.2 Team (deferred-primitive graduation)

When `spawn_subagent` / teams ship, the grower becomes a **lead** on a kanban,
delegating to existing config-only members instead of doing everything inline:

```
team: growth-autopilot
  lead:   Growth Autopilot   (+ injected orchestrator skill)
  members:
    - Data Analyst          (measurement / SQL)        [shipped preset]
    - Marketing Strategist  (segments + VI copy)       [shipped preset]
    - Growth Autopilot      (synthesis + decisions)
  kanban: each weak-link hypothesis becomes a card: backlog→doing→review→done
```

The lead picks a card, delegates "size this segment" to Data Analyst and "write
the win-back push" to Marketing Strategist, synthesizes, and files the decision.
Still zero BE — teams only *group* existing agents.

## 5. Autonomy & safety ladder

Promote trust gradually; each rung is a config flip, reversible:

| Rung | autonomy | Acts how | When |
|---|---|---|---|
| 0 Observe | suggest | files recommendations only; human ships | launch / unknown product |
| 1 Assist | suggest | + drafts the exact `http_request` payload in the rec | once recs are trusted |
| 2 Act-narrow | auto | may call **only** the idempotent, low-blast-radius endpoints in `growth-act-manifest` (e.g. flip a banner segment), still files an audit rec | after a few clean cycles |
| 3 Act-broad | auto | may run reversible experiments end-to-end (enqueue a push to a sized segment) | high confidence + budget caps |

Safety rails are all existing mechanisms, not new code:
- **Default-deny policy** — the agent can only touch its scoped tools +
  explicitly allow-listed `http_request` hosts.
- **Product owns the blast radius** — endpoints are idempotent, capability-
  manifested, and refuse anything not exposed; AgentRay can't widen them.
- **Every act leaves a trail** — `submit_recommendation` doubles as the audit
  record; runs/traces are inspectable in Lab.
- **Memory is the only cross-run state** — a wrong baseline is corrected by
  `remember`, not a migration.
- **Budget/quota** per-agent is the one relevant *deferred* primitive; until it
  lands, keep rung ≤2 and rely on schedule cadence to bound spend.

## 6. Cadence (triggers)

- `0 6 * * *` **daily pulse** — refresh scorecard, advance/observe the running
  experiment, file at most one rec. Cheap, keeps the thread warm.
- `0 7 * * 1` **weekly readout** — full PMF verdict, experiment portfolio
  review, next hypothesis. Pins the readout to the dashboard (`DailyReadout`
  surface already renders agent narration).
- **webhook (anomaly)** — an external/alerting metric POSTs `{metric, delta}`;
  `prompt_template` = "Metric {{body}} moved sharply — diagnose and decide
  whether to react this cycle." Event-driven without a new engine.

## 7. Why this is the right shape

- **It's the loop, not a report.** The existing Growth Analyst *answers when
  asked*; the autopilot *owns the PMF loop on a schedule and carries state
  forward* via memory — that's the only real delta, and it's all config.
- **Acting stays governed.** The grower never gets a bespoke "do growth thing"
  tool; it calls audited product endpoints exactly like the novel moderator.
- **It graduates cleanly.** Single agent today → team kanban when those
  primitives land, with no rewrite, because both are AgentGarden agents.

## 8. Status & open items

**Shipped:** `growthAutopilotPreset()` is in `internal/storage/marketplace.go`
(catalog slug `growth-autopilot`) — persona + the five PMF skills + full analyst
scopes. One-click installable; passes the catalog invariants test.

**Act-path (deferred to the dev team).** The audited product endpoints
`/novel/agent/growth/*` **do not exist yet**. The preset therefore ships in
`suggest` autonomy and the `capability-request` skill turns the gap into a
feature: when a cycle needs an action the agent can't take, it files a structured
recommendation asking the dev team to build that idempotent endpoint + manifest,
and `remember`s the request so it isn't re-filed. When those endpoints ship, the
only change is config — add `http_request` (allow_hosts=[api.lohi2.com]) + the
`NOVEL_GROWTH_KEY` secret in the UI and promote autonomy to `auto`.

**Still open:**
1. **PMF verdict source** — is there a "would you be disappointed" survey event,
   or do we proxy PMF purely from the retention plateau? (The `pmf-scorecard`
   skill already falls back to the plateau and refuses to fabricate the survey
   number.)
2. **Per-agent budget** — needed before rung 3; until then cap via cadence.
3. **Post-install wiring** — the cadence (daily + weekly `schedule` triggers) is
   set up once in AgentGarden after install; no preset field carries a trigger.
```
