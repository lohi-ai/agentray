# AgentRay Roadmap

**Updated:** 2026-07-02 · derived from
[docs/PRODUCT-REVIEW.md](docs/PRODUCT-REVIEW.md).
North star: **the closed loop** — agents that measure, diagnose, act, and learn
on your own event data. Every item below either closes that loop, makes it safe
for people who aren't us, or gets it into a stranger's hands.

Horizons: **Now** (next 4–6 weeks), **Next** (1–2 quarters), **Later**
(directional). Within a horizon, items are ordered by leverage.

---

## Now — close the act-loop and open the front door

### 1. Alerting & anomaly detection (the missing proactive layer)
- Threshold + trend-break alerts on any saved insight (trend/funnel/retention)
  and on agent-ops signals (error rate, token cost, latency).
- Delivery channels: Slack webhook, email, generic webhook. Channels are also
  agent tools (`send_notification`) so scheduled agents can push findings —
  config-only, per governance.
- Baseline anomaly detection can start simple (rolling z-score in ClickHouse);
  an agent explains the anomaly on click ("why did this spike?" → routed chat).

### 2. Growth Autopilot v1 (design → shipped)
- Implement [DESIGN-GROWTH-AUTOPILOT.md](docs/DESIGN-GROWTH-AUTOPILOT.md) as a
  marketplace preset: scheduled loop over acquisition/activation/retention +
  PMF verdict, persisted learnings via `remember`, weekly readout into
  DailyReadout and the new notification channels.
- Acceptance: runs unattended for 4 weeks on kiem-lai producing a weekly
  "weakest link + hypothesis + action taken/proposed" report a human actually
  reads.

### 3. Stranger-ready onboarding
- Publish SDKs: `@agentray/browser` (autocapture) to npm and a Python server
  SDK to PyPI; versioned, with the PostHog-compat drop-in documented.
- One-command quickstart (`docker compose up` → seeded demo project → first
  event in <5 minutes) tested by someone who didn't build it.
- Minimal docs site generated from `docs/` (install, instrument, first
  dashboard, first agent, MCP connect).
- Replace the stale README "Roadmap" section with a pointer to this file and
  reposition the README lead around the loop (per product review §3).

### 4. Per-agent budgets & quotas (governance prerequisite)
- Token/cost/run-count ceilings per agent and per workspace; hard stop +
  notification on breach. Deferred in AgentGarden, but it blocks both external
  tenants and confident always-on scheduling. Surfacing: budget bar in Lab and
  agent settings.

### 5. Security debt from known findings
- Least-privilege ClickHouse role for `run_sql` (closes the table-function
  SSRF/cross-tenant bypass) and rate limits on auth endpoints.
- Sandbox session egress allowlist (`SandboxLimits.NetworkAllow` is reserved —
  implement it; stop relying on the host firewall).

## Next — earn external users and widen the wedge

### 6. Design partners program (3–5 external deployments)
- Target: AI-product teams already sending PostHog-shaped events. Offer white-
  glove migration + a seeded Growth Analyst. Instrument AgentRay with AgentRay:
  onboarding funnel, activation (first insight, first agent run), weekly
  retention — the tool must prove PMF on itself.
- Exit criteria for the horizon: at least one team that would be "very
  disappointed" without it, for reasons we can quote.

### 7. Experiments v1 (measure the act-path)
- First-class experiment object: assignment via SDK flag evaluation, exposure
  events, and a results view (uplift + significance) on any insight.
- Agents get `create_experiment` / `read_experiment_results` tools so the
  Autopilot loop can propose → launch → measure config-only. (The measurement-
  back loop partially exists; make it a product surface.)
- Explicit non-goal: a full feature-flag platform. Flags exist only to serve
  experiments at this stage.

### 8. Multi-tenant hardening
- Untrusted skill-authoring review flow (skills as PR-like proposals with
  diff + approval), retrieved-data screening at the trust boundary, and
  argument-level policy facets for `computer_use`/`http_request` (the `Policy`
  contract already supports it — ship a consumer).
- Workspace-level audit log of every agent action with secrets redacted.

### 9. Agent teams (ARCHITECT-AGENT-TEAM P2+)
- Build on shipped `spawn_subagent` + cross-agent delegation grants: persistent
  team boards (kanban cards → runs), Lab inspection of parent/child run trees.
- Gate behind budgets (#4) — teams multiply spend.

### 10. Finish the Signal/Astryx UI migration
- Complete route pages + remaining shadcn-primitive replacements so the product
  ships one visual generation. Matters for design partners' first impression;
  sequenced after the loop features because it doesn't change what users can do.

## Later — directional bets

- **Hosted cloud.** Managed AgentRay with usage-based pricing (events + agent
  tokens). Requires #8 fully done plus billing/metering. This is the business
  model unlock; self-host stays the OSS wedge.
- **Web session replay (DOM).** The most-missed PostHog feature; consider
  rrweb-based capture only when a design partner blocks on it — it's a heavy
  subsystem and off-thesis until demanded.
- **Warehouse interop.** Export/sync to BigQuery/Snowflake and query-federation
  so bigger teams don't have to choose between AgentRay and their warehouse.
- **Group/B2B analytics.** Account-level rollups (groups) for B2B SaaS ICPs.
- **Standalone OSS extraction.** Split `agentray/` from the kiem-lai monorepo
  once external contribution demand exists; premature before design partners.
- **Marketplace as distribution.** Public gallery of installable agents +
  dashboard templates (growth analyst, incident triage, PMF grower, support
  triage) — presets become the top-of-funnel content strategy.

## Standing constraints (apply to every item)

- **Config-only agents:** no bespoke backend per agent, ever
  ([AGENT-GOVERNANCE.md](docs/AGENT-GOVERNANCE.md)). New capabilities land as
  tools/skills/presets on the one runtime.
- **Default-deny stays default:** every new tool or channel passes the policy
  gate and the credential trust boundary.
- **Harness parity is a bar, not a phase:** new capabilities ship with the
  faux + real test pattern from
  [HARNESS-REVIEW.md](docs/HARNESS-REVIEW.md). As of Round 3 the harness
  meets or exceeds pi v0.80 (`earendil-works/pi`) on every audited axis;
  regressions against that bar are release blockers.
- **Dogfood first:** nothing graduates from Now/Next without running live on
  kiem-lai.
