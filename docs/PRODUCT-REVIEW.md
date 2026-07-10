# AgentRay — Product Review

**Date:** 2026-07-02 · **Status:** internal review
**Scope:** product idea, feature inventory, and market-fit assessment. The
companion improvement plan lives in [`../ROADMAP.md`](../ROADMAP.md).

---

## 1. The product idea

AgentRay today is **two products fused on one data plane**:

1. **An open-source, AI-native product analytics engine.** PostHog-compatible
   ingestion (`/capture`, `/batch`, `/identify` + `/e/` aliases), Go + ClickHouse
   + Postgres + Redis + NATS, small enough to self-host on one VM. The event
   model treats agent runs, tool calls, token cost, and latency as first-class
   rather than bolted on.
2. **A config-only agent platform (AgentGarden).** One generic, hardened agent
   runtime — default-deny permissions, encrypted secrets vault, Docker sandbox
   (`run_shell` / persistent `computer_use` / real `browser_use`), compaction
   with goal pinning, sub-agents, skills, triggers (chat/manual/schedule/
   webhook), full tracing, and a Lab test bench. Every new agent is data, not a
   backend PR.

The synthesis — stated in the Signal design doc — is the actual product thesis:

> **category:** data + agent operating system for SaaS growth teams
> **promise:** turn product signals into agent-assisted decisions and actions

That is, AgentRay is not "analytics with a chatbot." It closes the loop
`measure → diagnose → hypothesize → act → learn` with agents that live *on* the
event store (Growth Autopilot design), can act through governed `http_request`
tools, and remember what they learned between cycles.

**Verdict on the idea: strong and differentiated, but currently under-stated.**
The README still pitches "PostHog-lite migration path," which is the weakest
framing of the three available. The defensible idea is the closed loop; the
analytics engine is the substrate that makes agents trustworthy (they query
real events, not vibes), and the harness quality is the moat that makes the
agents actually work.

## 2. Feature review

### What's shipped and strong

| Area | State | Assessment |
|---|---|---|
| Ingestion & storage | PostHog-compat endpoints, Redis rate limit → NATS → ClickHouse, sessions MV | Solid, boring in the good way. Real migration on-ramp. |
| Analytics workflows | Insight builder (trend/funnel/retention), dashboards + templates, web analytics, event explorer, persons, cohorts (weekly retention triangle), SQL editor with saved queries, event-name autocomplete | Covers ~the top 80% of PostHog daily usage for a small team. Identity stitching (`canonical_id`) reaches raw SQL and charts. |
| Agent observability | Per-LLM-call traces (tokens, cost, latency), per-tool traces, agent session replay | This is Langfuse-territory functionality living next to product analytics — a real combo advantage. |
| Agent harness | Verified at Claude-Code parity across a 12-capability matrix (tool use, computer use, browser use, compaction, steering, plans, skills, sub-agents, reflection), faux + real tests | The single most technically impressive asset. Very few "agent platforms" have this rigor. |
| AgentGarden authoring | Agents-as-data (SOUL/AGENTS.md, skills, tools, secrets, triggers, model tiers), Lab with test/explain/steer/replay + LLM-judge verdicts, marketplace presets, config-only proof (novel-mod) | The zero-backend-code-per-agent governance rule is a genuine product discipline, not just an aspiration. |
| Agent-facing UX | Chat with orchestrator routing, ECharts everywhere incl. in-chat chart fences, agent-narrated daily home (DailyReadout), MCP server + installable skills for Claude Code/Codex | The "agent conversation UX standard" is ahead of most analytics tools' AI features. |
| Security posture | Default-deny policy, SSRF guard + host allowlists, write-only secrets resolved at trust boundary, hard-isolated sandboxes | Above-bar for the stage. Known open item: least-privilege ClickHouse role for `run_sql` table-function bypass. |

### What's weak or missing

- **Packaging & adoption surface.** No published JS/Python SDK packages; the
  autocapture module ships as a copy-in TS file. No docs site, no hosted cloud,
  no one-line `docker run` quickstart aimed at a stranger. The OSS claim ("MIT,
  designed for extraction") is not yet exercised — AgentRay has **one customer:
  kiem-lai/lohi**.
- **The act-path is thin.** Growth Autopilot is a design doc; scheduled agents
  can run insights and `http_request`, but there are no built-in outbound
  channels (Slack/email digest, alert on anomaly), no feature flags, and only a
  partial experiment loop. "Turn signals into actions" currently requires the
  target product to expose its own `/agent/*` API (as novel did).
- **No alerting/anomaly detection.** A growth team's first ask of any always-on
  analytics+agent product is "tell me when something breaks or spikes." Nothing
  proactively pushes today except a scheduled agent someone configured.
- **Governance gaps that block untrusted tenants:** per-agent budget/quota,
  skill-authoring hardening, retrieved-data screening, sandbox egress
  allowlist, argument-level policy facets — all consciously deferred, all
  prerequisites for anyone-but-us usage.
- **Web session replay** (DOM replay) doesn't exist — only agent replay. Fine
  for the wedge, but it's the #1 PostHog feature people actually miss.
- **Design system migration is mid-flight** (Tailwind/shadcn → Astryx "Signal"),
  so the UI is currently two visual generations at once.

## 3. Market-fit assessment

### The landscape

- **Product analytics:** PostHog, Amplitude, Mixpanel. PostHog is the direct
  giant: OSS, generous free tier, and now ships "Max AI" plus LLM analytics.
  Competing head-on as "lighter PostHog" is a losing wedge on its own — the
  moat there is breadth, and they have it.
- **LLM/agent observability:** Langfuse, LangSmith, Helicone, Braintrust. They
  observe AI apps but **don't own the product-analytics side and don't act** —
  no agents that do work on your behalf.
- **Agent platforms / AI teammates:** Lindy, Dust, Relevance, Zapier Agents,
  n8n+AI. They act, but they **don't own your event data** — their agents are
  blind to product truth and mostly glue APIs together.

Nobody credibly occupies the intersection: *agents with governed hands that
stand on your own event warehouse*. That intersection is exactly what AgentRay
has built. This is real whitespace, and the harness-parity work means the agent
half isn't a demo.

### Who it's for (sharpest ICP)

**Solo founders and 2–10-person product teams who cannot afford a data analyst
or a growth hire.** The pitch: *install one snippet, and a governed AI growth
analyst watches acquisition/activation/retention on your own infra, tells you
the weakest link every week, and — within limits you set — acts on it.* The
kiem-lai deployment is a live proof of exactly this shape (default Growth
Analyst seeded per workspace, marketplace dashboards, daily agent-narrated
readout).

Secondary ICP: **teams building AI products** who want product analytics and
agent/LLM observability in one self-hosted stack instead of PostHog + Langfuse.

### Honest risks

1. **Single-tenant validation.** Every "verified live" claim is verified on the
   founder's own product. That's the right way to start (dogfooding is deep
   here), but there is zero external signal yet — no stranger has installed it,
   hit the onboarding wall, or churned. Market fit is currently *hypothesized,
   not measured*.
2. **Two-audience positioning drag.** "PostHog alternative" buyers evaluate on
   analytics breadth (replay, flags, experiments) and will find gaps.
   "AI teammate" buyers don't care about ClickHouse. The messaging must commit
   to the loop story or it will lose both comparisons.
3. **PostHog convergence.** They are adding agentic features fast. AgentRay's
   durable edges are (a) genuinely self-hostable small-footprint stack,
   (b) harness quality + governance (config-only agents with real sandboxes),
   (c) speed on the act-loop. Edge (c) decays fastest; ship it.
4. **Scope gravity.** The repo shows a pattern of building excellent
   infrastructure (harness rounds, Lab, design languages) ahead of distribution.
   The next marginal week of engineering almost certainly buys less than the
   next marginal week of packaging + a second design-partner deployment.

### Verdict

| Dimension | Grade | Note |
|---|---|---|
| Product idea | **A−** | Closed measure→act loop on owned event data is real whitespace; needs to be *the* stated identity. |
| Feature depth (for the wedge) | **B+** | Analytics core + harness are above-bar; act-path, alerting, and packaging are the gaps that matter. |
| Market fit today | **C / unproven** | One (deeply integrated) internal customer; strong internal PMF signal, zero external signal. |
| Market fit potential | **B+ → A** | Contingent on picking the growth-analyst wedge, shipping alerting + act-path, and getting 3–5 external design partners. |

**Bottom line:** the engineering has out-run the product's market exposure. The
highest-leverage moves are not new capabilities — they are (1) repositioning
around the loop, (2) closing the act-path (alerts, outbound channels, budgets),
and (3) making a stranger's first 30 minutes work. The roadmap orders work
accordingly.
