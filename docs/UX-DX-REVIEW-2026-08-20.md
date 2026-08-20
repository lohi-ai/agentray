# AgentRay — UX / DX Review

**Date:** 2026-08-20 · **Method:** source review of `agentray/web`, `internal/`, `sdk/`, and docs. Local web `:3200` and API `:8088` were **not running** (`curl` exit 7); this pass did not start `make dev` / `make web` / compose. No screenshots. Live-data claims from [UX-REVIEW-2026-08.md](UX-REVIEW-2026-08.md) (2026-08-19) are reused only where current code still matches.
**Refines:** [PRODUCT-REVIEW.md](PRODUCT-REVIEW.md) (2026-07-02), [UX-REVIEW-2026-08.md](UX-REVIEW-2026-08.md) (2026-08-19).
**Constraint:** no product-code edits in this pass. Agents stay config-only marketplace presets + skills; no new Go agent.

Recent commits already closed the 2026-08-19 trust defects (`83b9254`). Those are **not** re-listed as bugs. §5 of that review is re-verified against current code; several items still hold, and a first-run **event-name split** now outranks them.

---

## Already closed — do not re-open

From UX-REVIEW-2026-08 §4, still present in code:

| Fix | Where it lives now |
|---|---|
| Recommendation trigram fold (`similarity ≥ 0.55`, 14-day window, `seen_count`) | `internal/dataplane/store/agent_runtime.go` |
| `ListRecommendations` LIMIT 50 | same |
| `formatRate` never prints `0%` for a non-zero rate | `web/lib/ia.ts` |
| One event → one funnel stage (deepest match) | `web/lib/ia.ts` `stageOf` |
| Funnel headlines use stage labels; non-adjacent stages named as a gap | `web/lib/ia.ts` `weakestNotice` |
| `CostUnpriced` + `formatCost` → `—` / `$n+` | `web/lib/format.ts:25-29`; agent monitor / agents roster pass the flag |
| Marketplace install idempotent on `preset_slug`; card "On your team" | `internal/dataplane/store/marketplace.go:84-98`; `web/modules/marketplace/page.tsx:147-179` |
| Lab icon-only (no clip to "I") | `web/modules/agents/page.tsx:107-112` |
| Persons axis local time, integer y-ticks, trait chips | `web/modules/persons/page.tsx` |
| Events tokens/cost `—` on non-agent rows; person id lead+tail | `web/modules/events/page.tsx:18-36` |

---

## Inventory — shipped surface

Nav source of truth: `web/lib/ia.ts` `NAV_ITEMS` (lines 21–35). Group headings are backend layers: Runtime / Channels / Workloads / Data / Workspace. Nested surfaces live in `CHILD_SURFACES` (94–106) and render as "Also" links via `RelatedSurfacesLabel`. Root `/` redirects to `/chat` (`web/app/page.tsx:6-7`). `AppShell.active` is accepted and **unused** — selection is `matchActiveHref(pathname)` (`web/modules/shared/components/app-shell.tsx:93-99`).

| Route | Module | What it does | Nav | Real? |
|---|---|---|---|---|
| `/` | `web/app/page.tsx` | Redirect to `/chat` | — | real |
| `/chat` | `web/modules/chat/page.tsx` | Conversational front door; FirstRunPanel; stream; docks | Runtime · Chat | real (`POST /api/agent/chat`) |
| `/start` | `web/modules/start/page.tsx` | Job index (validate / grow / operate) + checklist | **Not in sidenav.** Header ghost "Set up" on chat (`chat/page.tsx:891`). Also-link only if chat rendered `RelatedSurfacesLabel` — it does not. | real (composes existing APIs) |
| `/agent` | re-export of Agents roster | Same as `/agents` | alias of Agents | real |
| `/agents` | `web/modules/agents/page.tsx` | Roster, 24h spend/tokens, talk / setup / lab | Workloads · Agents | real |
| `/marketplace` | `web/modules/marketplace/page.tsx` | Hire presets + apply dashboard templates | Also under Agents | real; install idempotent |
| `/teams`, `/teams/[id]` | `web/modules/teams/` | Agent teams + kanban cards | Also under Agents | real (`/api/teams`) |
| `/agents/monitor`, `/monitor` | `web/modules/agents/monitor/page.tsx` | Fleet health table | Also under Agents | real |
| `/agents/[id]/setup\|lab\|monitor`, `/runs/[runId]` | `web/modules/agents/[agentId]/` | Authoring, Lab, run traces | deep under Agents | real |
| `/operations`, `/operations/[id]` | `web/modules/operations/` | Schedules + webhooks that run without a conversation | Channels · Operations | real. Slack/Discord/Telegram cards **Not yet** (`page.tsx:365-378`) — those are chat *ingress*, not alerting |
| `/dashboard`, `/dashboards` | `web/modules/dashboard/page.tsx` | Saved views, DailyReadout, first-event snippet | Data · Dashboards | real |
| `/templates` | `web/modules/templates/page.tsx` | Apply system dashboard templates | Also under Dashboards | real |
| `/sql` | `web/modules/sql/page.tsx` | SQL-lite + saved queries; handoff to Data Analyst | Also under Dashboards | real |
| `/web-analytics`, `/traffic` | `web/modules/web-analytics/page.tsx` | Visitors/pageviews/sources; filters `user.pageview` | Data · Traffic | real |
| `/product` | `web/modules/product/page.tsx` | Trend / funnel / retention / table via insights | Data · Product | real, **does not auto-run** |
| `/prototypes`, `/prototypes/[id]` | `web/modules/prototypes/` | Validate-job scoreboard (commit / decide tests) | Also under Product | real (`/api/validation/*`) |
| `/persons` | `web/modules/persons/page.tsx` | People + traits | Data · People | real |
| `/cohorts` | `web/modules/cohorts/page.tsx` | Weekly retention triangle + audiences | Also under People | real |
| `/events` | `web/modules/events/page.tsx` | Live event stream (5s poll) | Data · Events | real |
| `/replay` | `web/modules/replay/page.tsx` | **Agent session** timeline, not DOM replay | Also under Events | real |
| `/settings` | `web/modules/settings/page.tsx` | Workspace, plan, projects, members, AI, connectors, keys, activity | Workspace · Settings | real. **No alert-channel tab** |
| `/alerts` | `web/modules/alerts/page.tsx` | Threshold rules | Also under Settings | rules real; **no UI to create a channel** |
| `/pricing` | `web/modules/pricing/page.tsx` | Hosted plans; self-host honest bounce | Workspace · Plans (`hostedOnly`) | real interest-request, not billing |

Every named surface has a Go handler except `/start` and `/product`, which compose existing reads. Alerting delivery (Slack webhook, generic webhook, email-via-relay) is implemented in `internal/dataplane/alerting/deliver.go` and ticked from the scheduler. Discord/Telegram do not exist as deliverers. Support widget and voice are reserved channel kinds (`web/lib/ia.ts:62-63`).

Prototype/fake UI that is **not** a product surface: `web/app/prototypes/` is the *validate* feature (real). Dead decorative SVGs `AreaChart` / `RetentionChart` / `BarChart` in `web/modules/shared/components/charts.tsx:191-200` are hardcoded hex paths with no data binding; live charts go through ECharts `Chart`.

---

## A. Ranked improvement opportunities

Each item: surface + `file:line`, what is wrong, who it hurts, severity, smallest fix.

### P0

**1. First-run event names do not match the charts that claim to read them.**
- **Surface:** Dashboards / Traffic / first-event onboarding.
- **Code:** demo seed writes `pageview` / `signup` / `activation` / `purchase` (`internal/dataplane/store/seed_demo.go:78-95`). Seeded and template SQL filters `event_name = 'user.pageview'` (`internal/dataplane/store/store.go:1394, 1649-1705, 2552, 2626`). Default insight funnel is `user.pageview, user.signup, user.conversion` (`store.go:3183-3184`). Autocapture emits `user.pageview` (`sdk/browser/autocapture.ts:10-12, 101`). Dashboard FirstEventQuickstart website snippet sends `event: "pageview"` (`web/modules/dashboard/first-event-quickstart.tsx:40-41`). Chat FirstRunPanel promises "I'll find the weakest step" on sample data (`web/modules/chat/chat-parts.tsx:364-366`).
- **Wrong:** A stranger following the in-app snippet, or landing on the seeded Demo project, fills Events with `pageview` while Traffic / Product-overview charts stay empty. `weakestLink` *will* match `pageview` via `/page_?view/` (`web/lib/ia.ts:290`) so the chat headline can look healthy while Traffic is zero — two screens, two truths.
- **Hurts:** every new user (activation). Also the owner dogfooding Demo vs Production.
- **Smallest fix:** rename seed events to `user.pageview` / `user.signup` / `user.conversion` (keep `activation` or map it to a tracked activation event the funnel actually uses). Change FirstEventQuickstart to `user.pageview`. One contract, three writers.

**2. `npm install @agentray/browser` is the documented path and the packages are unpublished.**
- **Surface:** DX / QUICKSTART / README.
- **Code:** QUICKSTART.md:50-51, README.md:123, `sdk/browser/package.json` (`@agentray/browser` 0.1.0, no `publishConfig`). Same for `@agentray/server` and PyPI `agentray` 0.1.0. `docs/IMPLEMENTATION-PLAN.md` Phase 3a still open.
- **Wrong:** the 15-minute stranger path dies at step 2 unless they copy source. The in-app InstrumentSnippet already admits this (`web/modules/start/components/instrument-snippet.tsx:9-17`) and ships a correct copy-in snippet — the docs do not lead there first.
- **Hurts:** developers integrating; anyone following QUICKSTART literally.
- **Smallest fix:** either publish `0.1.0` or change QUICKSTART/README first snippet to the already-correct copy-in HTML (`instrument-snippet.tsx:105-136`) and mark npm as "coming". Do not leave `npm install` as the lead.

### P1

**3. Operate job has no reachable outbound channel in the UI.**
- **Surface:** `/alerts`, `/settings`, `/operations`.
- **Code:** Deliverer is real (`internal/dataplane/alerting/deliver.go:73-160`; Slack / webhook / email-relay). `POST /api/alerts/channels` exists (`web/lib/api.ts:1769-1770`; `useAlertChannels` create mutation `web/modules/app/hooks/alerts.ts:74-77`). **No caller.** Alerts page copy: "add a Slack/email/webhook channel in workspace settings" (`web/modules/alerts/page.tsx:159-162`) but Settings tabs are Workspace / Plan / Projects / Members / AI Provider / Data connectors / API keys / Activity (`web/modules/settings/page.tsx:14`) — no channels. Operations "Coming" cards are Slack/Discord/Telegram *chat ingress* (`web/modules/operations/page.tsx:365-378`), which reads as "we cannot notify you".
- **Wrong:** an owner can create a rule that "fires and appears here" with nobody to tell. The `operate` job's done-when ("something reaches me outside the app") is still false. Backend work is stranded.
- **Hurts:** operators; also scheduled Growth Lead `send_notification` (preset playbook in `internal/workloads/presets.go`) which errors and falls back to a page you have to open.
- **Smallest fix:** one "Add Slack webhook" form on `/alerts` (or a Settings "Notifications" tab) calling the existing `createAlertChannel`. Do not build Discord/Telegram. Do not wait for chat-ingress.

**4. Product "Where do new users drop off?" builds a funnel from the top 4 event names.**
- **Surface:** `/product`.
- **Code:** `ask('funnel')` uses `(summary?.event_counts ?? []).slice(0, 4).map(e => e.event_name)` (`web/modules/product/page.tsx:46-51`). Page still waits for a click (`active` starts `null`, empty "Pick a question to begin", lines 41, 96-103).
- **Wrong:** on a live AgentRay project the top events are likely `$autocapture`, `user.pageview`, `agent.tool_call`, … — not an activation funnel. The chart will look like a funnel and will not be one. This is the same class of trust defect §4 fixed in `weakestLink`, now living on the Product surface.
- **Hurts:** growth owners who skip chat and press the obvious question.
- **Smallest fix:** reuse `weakestLink` / `FUNNEL_STAGES` (or the default `user.pageview, user.signup, user.conversion`) as funnel steps. Auto-run that question on mount when the catalog is non-empty.

**5. Cost cap cannot fire on workspace tier aliases.**
- **Surface:** agent setup budgets; `/agents` spend.
- **Code:** host defaults record model as `plus` / `pro` / `lite` / `flash` (`internal/dataplane/store/workspace_tiers.go`). `DefaultPricing` has gpt/claude/gemini ids only (`agentcore/plugins/observe/pricing.go`). `SpendForAgentPeriod` sums `cost_usd` (`internal/dataplane/store/agent_budgets.go`). `budgetExceeded` / runner gate trip `MaxCostUSD` only on that sum (`internal/runtime/runner.go:620-622`). Token/run caps still work. UI correctly shows `—` via `formatCost`.
- **Wrong:** the cost rail looks armed and cannot fire. Unchanged from UX-REVIEW §5.2.
- **Hurts:** anyone who leaves a scheduled agent running "because there's a cap".
- **Smallest fix:** treat `cost_unpriced` as "cap cannot be evaluated" — refuse to start (or stop after first unpriced call) when `MaxCostUSD > 0` and the model is unpriced. Better follow-up: per-alias USD/MTok on the Models tab. Do not invent a price.

**6. Seeded Growth Lead is invisible to marketplace idempotency.**
- **Surface:** `/marketplace` after signup.
- **Code:** `SeedDefaultFoundationAgent` INSERTs `agents` without `preset_slug` (`internal/dataplane/store/marketplace.go:157-160`) despite comment "seeded *disabled*" (it sets `enabled=true`). `InstallAgentPreset` only short-circuits on `AgentByPresetSlug` (84-98).
- **Wrong:** first-run hires Growth Lead, then Marketplace still offers "Install agent". A click mints a second Growth Lead (`freeAgentSlug` → `growth-lead-2`). The 2026-08-19 duplicate-teammate fix does not cover the seed path.
- **Hurts:** every new workspace.
- **Smallest fix:** set `preset_slug = 'growth-lead'` on the seed INSERT. Backfill existing default agents the same way §4 backfilled pre-fix hires.

**7. `/start` is still the clearest screen and the hardest to find.**
- **Surface:** IA / chat header.
- **Code:** `NAV_ITEMS` has no `/start` row; it is only an *alias* that lights Chat (`web/lib/ia.ts:22-25, 96`). Chat header is a ghost "Set up" (`web/modules/chat/page.tsx:891`). Chat does not render `RelatedSurfacesLabel`. Nav groups are Runtime / Channels / Workloads / Data / Workspace (`ia.ts:9, 21-35`) — backend layers, which `ARCHITECT-WEB.md` already concedes only help people who know the artifact.
- **Wrong:** the job model (`web/lib/jobs.ts`) is the owner-facing index and lives off-nav. Unchanged from UX-REVIEW §5.5.
- **Hurts:** first-run and returning owners who are not already on Chat.
- **Smallest fix:** promote `/start` to a Runtime nav item labelled "Set up" (or make Chat's Also-row include it and stop using a ghost button). Do not rename the layer groups in the same PR if that fights the 2026-08-19 chrome work — just make the job index one click from every page.

**8. Docs teach the old browser client.**
- **Surface:** DX.
- **Code:** `docs/SDK.md:70-74` `import { AgentRayClient } from '@/sdk/browser/client'`. Real API is `import { init } from '@agentray/browser'` (`sdk/browser/index.ts:1-12, 41-74`). `init()` comment captures `'pageview'` (`index.ts:6`). README MCP self-host example uses `localhost:8080/mcp` (README.md:211) — container-internal; host port is 8088. Python SDK has no `alias` / `revenue` / `POST /identify` (identify is a `$identify` batch event).
- **Hurts:** developers. Smallest fix: rewrite SDK.md browser section to match `sdk/browser/README.md`; fix the MCP host example.

### P2

**9. `/events` StatsStrip "Events" is the loaded window, not a total.**
- `web/modules/events/page.tsx:223-224` `formatCompact(events.length)`. API `GET /api/events` returns no total (default limit 50, max 500). UX-REVIEW §5.7 still true. **Fix:** label "In this window" or show `activity.event_count` from `/api/activity` next to the table.

**10. Empty-prompt schedules are still armable.**
- UI `canAdd = kind === 'webhook' || cron.trim().length > 0` (`web/modules/agents/[agentId]/setup/page.tsx:608`). Store requires cron only (`internal/dataplane/store/agent_triggers.go`). Empty prompt → `MonitorPrompt` (`internal/app/operations_routes.go`, `internal/runtime/scheduler.go`). A `*/15 * * * *` with no instruction is a legal always-on watchdog. **Fix:** require a non-empty prompt for `kind=schedule`, or require interval ≥ 1h unless prompt is set. Surface the fallback text in the row (operations already says "No prompt set — it runs the default health check.", `operations/page.tsx:318`).

**11. Vocabulary drift and "Armed".**
- Hire vs Install vs New vs Spin up: marketplace CTA "Install agent" (`marketplace/page.tsx:182`) vs "Hire a teammate" heading (225) vs agents "New agent" / "Spin up" (`agents/page.tsx:60, 74, 123`) vs `/start` "Hire". Ask AI (`dashboard/page.tsx:146`) vs Talk to agent (`agents/page.tsx:104`) vs Ask Growth Lead (`product/page.tsx:59`). Operations stat and panel: "Armed" / "Armed channels" (`operations/page.tsx:204, 257`; `lib/operator.ts:28`). DESIGN.md forbids jargon statuses. **Fix:** one verb "Hire"; one ask "Ask {agent}"; replace Armed → "On" / "Scheduled".

**12. `/agents` "Needs attention" / "Fix setup" still does not say what.**
- Roster: `error_count > 0` → "Needs attention" + "Fix setup" → `/agents/:id/monitor` (`agents/page.tsx:21, 99-100`). Needs-key is now explicit ("Needs AI key") — that part of §5.4 is improved. Fleet monitor *does* say "failed N runs" (`agents/monitor/page.tsx:53-60`). Roster does not. `/start` jobSteps treats `hasModelKey !== false` as the key step (`jobs.ts:260`) and does not look at `error_count`, so a failing scheduled agent can still read as a finished checklist. **Fix:** pass `error_count` into the roster label (`"3 failed runs"`) and into `jobSteps`.

**13. Light default, no toggle, DESIGN.md still says dark SaaS.**
- `ThemeRoot` first paint is light (`web/modules/app/theme-root.tsx:30-41`, commit `50ce968`). `useColorMode` has no consumer outside itself. DESIGN.md:32 "dark SaaS product"; `globals.css` still pins the dark ramp. **Fix:** add a footer toggle calling `useColorMode().toggle`, or change DESIGN.md. Do not leave two truths.

**14. Replay cost can still render `$0.00`.**
- `web/modules/replay/page.tsx:61` `formatCost(replay.total_cost_usd)` — no `unpriced` flag. If the session burned tokens on aliases, this undoes §4 on this one screen. **Fix:** thread `cost_unpriced` on the replay payload (or treat `tokens > 0 && cost == 0` as unpriced at the render site).

**15. Dashboard has three adjacent primary actions.**
- New view + Ask AI + Add chart (`web/modules/dashboard/page.tsx:146`). DESIGN.md: one primary per screen. **Fix:** primary = Add chart (or Ask {Growth Lead}); the others outline/ghost.

**16. Marketplace still shows skill slugs.**
- `preset.skills.map(s => s.name)` (`marketplace/page.tsx:162-171`). UX-REVIEW §1.4. **Fix:** show `s.description` (already in `title=`) or a human label; keep slug in tooltip.

**17. Magic numbers / leftover hex.**
- Agent cards `p-[15px] gap-[11px] rounded-[9px] text-[13.5px]` (`agents/page.tsx:81-86`). Marketplace same class of off-scale values (`marketplace/page.tsx:141-148`). `charts.tsx` Sparkline default `#46B7E8`; AreaChart/RetentionChart/BarChart hardcoded hex decorative SVGs (167-200) — unused by pages, still importable. Templates description uses inline `style={{ color: 'var(--muted-foreground)', fontSize: 12.5 }}` (`templates/page.tsx:21`). **Fix:** delete the three fake chart exports; tokenise the card spacing in a follow-up, not a rewrite.

**18. Monitor primary CTA is mislabelled.**
- "Open live monitor" → `/chat` (`agents/monitor/page.tsx:42`). **Fix:** label "Ask about fleet health" or point at `/agents/:id/monitor` for `needsReview`.

**19. Ingest lag is invisible.**
- Capture HTTP 200 = NATS ack, not ClickHouse (`internal/dataplane/ingest/handler.go`). Batcher flush 1s/500 rows. Events poll 5s (`web/modules/app/hooks/console.ts:229-237`). `init()` client batch 3s (`sdk/browser/transport.ts`). Empty copy: "No events match these filters." (`events/page.tsx:236`). FirstEventQuickstart "I've sent it — check now" only invalidates queries (`first-event-quickstart.tsx:126-129`). **Fix:** one sentence on the empty state and on Check now: "Events can take a few seconds to appear."

**20. Chat page is 992 lines vs the 150-line route target.**
- `web/modules/chat/page.tsx`; `ARCHITECT-WEB.md` file-size table. Not a user-facing bug; it is why first-run / Set up / related-surfaces keep drifting. Split FirstRun + composer wiring in a later sweep — not a PHASE 2 blocker.

---

## B. Three user stories (highest-value paths)

Grounded in `web/lib/jobs.ts` and what the code actually ships, not a new persona.

### Story 1 — Grow: name the weakest step

> **As a founder with a live product, I want the single weakest step in my activation funnel named with a count I can open, so I know where to spend this week.**

**Why this is the top-value path.** It is the product promise (README lead, `FIRST_RUN_PROMPT` in `web/lib/ia.ts:189`, Growth Lead pack, FirstRunPanel primary button `chat-parts.tsx:387`). `needsEvents: true` is the load-bearing distinction from validate. The 2026-08-19 pass already made `weakestLink` honest; the remaining work is getting the *same* honest answer onto Traffic, Product, the seeded dashboard, and a digest that leaves the app.

**Flow today.**
1. Sign up → Demo project + Growth Lead + Product overview dashboard + `SeedDemoEvents`.
2. Add an AI key (`/settings?tab=ai`) or FirstRunPanel blocks.
3. Chat: "Find my weakest funnel step" (`FIRST_RUN_PROMPT`) → agent `run_insight` / `run_sql` → optional pin.
4. Or skip chat: `/product` → click "Where do new users drop off?" → top-4-event funnel.
5. Or `/dashboard` DailyReadout of scheduled recommendations (deduped, capped at 50).
6. Act: accept/dismiss a rec card. Nothing is pushed out of band.

**Friction on that flow.**
- Seed + snippet event names ≠ Traffic SQL (A.1). First-run can narrate a funnel the charts do not show.
- `/product` invents a funnel from volume, not stages (A.4).
- `/start` is off-nav, so the grow checklist ("point it at your product" → "let it run without you") is easy to skip (A.7).
- Standing loop has no Slack/email destination (A.3). 171 recs / 1 action from the prior live review is the behavioural evidence that an in-app feed is not an act path.
- Cost cap cannot fire on aliased models (A.5), so "let it run without you" is a promise the budget UI cannot keep.

### Story 2 — Validate: prove it before building

> **As a founder with an idea and no product, I want a teammate to name a kill/keep number, hand me a pasteable snippet, and score the waitlist against that number, so I spend a week instead of a quarter finding out.**

**Why this is top-value.** It is the only analytics product path that is *defined* on an empty event store (`needsEvents: false`, `jobs.ts:52-54, 70`). Product Scout + Marketing Lead + `/prototypes` + `/waitlist` are shipped, config-only. This is the wedge a stranger without PostHog can finish in one sitting.

**Flow today.**
1. `/start?job=validate` (if they find Set up) or chat chips "Is anyone already paying…" / "Design the cheapest test…".
2. Hire Product Scout (marketplace or job-plan Hire).
3. Agent proposes a test (`propose_test`); owner commits on `/start` or `/prototypes`.
4. InstrumentSnippet on `/start` / Settings keys: `user.pageview` + `$autocapture` + waitlist form posting `/waitlist` (`instrument-snippet.tsx:105-171`). Correct names.
5. Market it (hire Marketing Lead) → watch `/prototypes` / `/events`.
6. Decide pass/fail.

**Friction on that flow.**
- Finding `/start` (A.7). Validate is not a nav group; Prototypes nests under Product, which sounds like "I already have a product".
- Dashboard FirstEventQuickstart (shown on empty *and* Demo) teaches `pageview`, which is the wrong contract for the same owner who just pasted the correct snippet on `/start` (A.1).
- Tracking plan the agent writes is not auto-applied to the event catalog — owner re-types names. Named in UX-REVIEW 2026-08 job:validate gap; still true (no writer from `propose_test` into a tracking-plan table that Events "unplanned" can use beyond whatever already exists).
- Two hire verbs (Hire vs Install) on the path to the first teammate (A.11).

### Story 3 — Ask: one question, a chart from real events

> **As a growth lead (or an external agent over MCP), I want to ask a product question in plain language and get a chart backed by a query that ran this turn, so I do not need SQL to trust the number.**

**Why this is top-value.** Chat is the signed-in landing (`SIGNED_IN_LANDING = '/chat'`). Guardrails in every foundation preset forbid inventing a metric (`internal/workloads/presets.go:16-19`). The same operations project to REST, in-app tools, CLI, and `POST /mcp` (`docs/AGENT-GOVERNANCE.md`). Evidence-guard re-opens a figure-shaped answer with zero read tools. This is the loop the design system calls the front door.

**Flow today.**
1. Land `/chat`. If no agent runs yet: FirstRunPanel (sample-data honesty is good). Else FrontDoor chips grouped by job.
2. Optional: MCP `claude mcp add` with `X-API-Key`.
3. Agent runs `run_insight` / `run_sql` / `explore_events`, may `create_chart` after confirm.
4. User jumps to `/dashboard` or `/sql` "Ask AI" which is just `/chat?q=…` — no special endpoint. Correct.

**Friction on that flow.**
- First answer needs a workspace model key; events do not. FirstRunPanel states this; QUICKSTART step 3 does not (A.2 adjacent).
- Sample data uses the wrong event names, so the first chart the agent pins from Demo can be empty (A.1).
- SDK.md still documents `AgentRayClient` from `@/sdk/browser/client` (A.8); a developer wiring MCP + browser in parallel hits two APIs.
- `init()` batches 3s; capture 200 ≠ visible; empty Events copy does not say so (A.19). The first "it didn't work" is often impatience.

---

## C. Optimization plans (PHASE 2 input)

Ordered, small, shippable. No new Go agent. Reuse lohi-ui/Astryx tokens. Each step is independently mergeable.

### Plan for Story 1 (weakest step)

1. **Align the event contract** — `internal/dataplane/store/seed_demo.go` emit `user.pageview`, `user.signup`, `user.conversion` (and one activation event the funnel stages already match). Update `seed_demo_test.go` name assertions. Change `web/modules/dashboard/first-event-quickstart.tsx:40` to `user.pageview`. Change `sdk/browser/index.ts:6` comment off `pageview`.
2. **Honest Product funnel** — `web/modules/product/page.tsx` `ask('funnel')`: steps from `weakestLink(eventNames)` or default `['user.pageview','user.signup','user.conversion']`, never `event_counts.slice(0,4)`. On catalog ready, auto-call `ask('funnel')` once.
3. **Seed `preset_slug`** — `SeedDefaultFoundationAgent` INSERT includes `preset_slug = 'growth-lead'`. One-line backfill for existing default agents missing it.
4. **Notifications tab** — `/alerts`: if `channels.length === 0`, show kind=slack + webhook URL fields and `createAlertChannel`. Delete the "add it in settings" sentence. Optional: Settings Also-link already points here.
5. **Unpriced cost cap** — `budgetExceeded` / runner gate: if `MaxCostUSD > 0` and the run is `CostUnpriced`, stop with a clear error ("cost cap set, model unpriced — add a rate or use token/run caps"). Do not silently treat as $0.
6. **Set up in nav** — add `{ href: '/start', label: 'Set up', group: 'Runtime' }` to `NAV_ITEMS` (keep Chat as landing). Ghost button can stay as a duplicate.

### Plan for Story 2 (validate)

1. **Make Prototypes findable** — `CHILD_SURFACES` already nests Prototypes under Product. On `/start?job=validate`, the existing surfaces list includes `/prototypes` (`jobs.ts:74`). Add a Chat Also-row (`RelatedSurfacesLabel parentHref="/chat"`) so Set up is visible without the ghost button. Copy on `/product` empty-catalog already points at Prototypes (`product/page.tsx:99-102`) — keep it.
2. **One hire verb** — marketplace primary CTA "Hire" when `!installedAgent` (`marketplace/page.tsx:182`); agents empty state "Hire a teammate" not "Spin up". Leave "Install" out of owner-facing copy.
3. **Do not teach a second snippet** — FirstEventQuickstart website branch should render `InstrumentSnippet` (already correct) or the same `user.pageview` payload. Kill the inline `pageview` fetch.
4. **Commit is the act** — no new backend. Ensure job-plan "Review it" for a proposed test goes to `/prototypes/{id}` (not only `/start?job=validate`, which is LIMIT-1 in spirit even though Prototypes is the plural). Check `jobs.ts:315-316` action href.

### Plan for Story 3 (ask → chart)

1. **SDK.md = `init()`** — replace the browser section with the README snippet. Note copy-in path for no-npm. Fix README MCP example to `:8088`.
2. **Empty-state latency sentence** — Events emptyMessage + FirstEventQuickstart Check now.
3. **Publish or stop promising npm** — same as A.2. If publish is out of scope for PHASE 2, the copy-in path is the product.
4. **Replay `formatCost(..., unpriced)`** — thread the flag or derive it. One-line trust fix.
5. **Dashboard primary** — one `variant="primary"` on Add chart; Ask {Growth Lead} stays `agent`; New view `outline`.

### Explicitly out of PHASE 2

- Slack/Discord/Telegram as *chat channels* (Operations "Coming" cards). Alerting Slack webhook is the outbound that unblocks operate.
- DOM session replay.
- Publishing a new marketplace persona (that is a Go `Register()` in `internal/workloads/presets.go` — allowed as catalog, but not needed to close these stories).
- Renaming nav group headings Runtime/Channels/Workloads/Data (do `/start` first; the layer names are a follow-up).
- Splitting `chat/page.tsx`.
- Per-alias price table UI (the refuse-unpriced-cap fix is enough to stop lying).

---

## Appendix — DX trace (zero → first chart)

| t | Step | What actually happens |
|---|---|---|
| 0 | Key | Compose seed `lohi_dev_project_token`, or `agentray signup` + `agentray key`, or Settings → API keys. Signup also creates Demo + Growth Lead + Product overview. |
| 2 min | Event | QUICKSTART curl `hello_agentray` → Events (5s poll). Traffic stays empty (not `user.pageview`). In-app website snippet sends `pageview` — same miss. InstrumentSnippet and autocapture are correct. |
| HTTP 200 | Ingest | NATS ack, ClickHouse ~1s later, UI up to 5s, `init()` up to +3s batch. |
| 3 min | Agent answer | Needs workspace model key. FirstRunPanel blocks honestly. QUICKSTART does not mention the key. |
| Chart | Pin | Agent `create_chart` after a verified query (guardrails). Seeded dashboard SQL looks at `user.pageview` — empty on Demo until A.1. |

Catalog packs (config-only *runtime*, compiled *catalog*): `growth-lead`, `data-analyst`, `tracking-steward`, `marketing-strategist`, `marketing-lead`, `insight-digest`, `ops-watch`, `product-scout` (`internal/workloads/presets.go` `init()`). A tenant-created agent is data (UI/REST). A new gallery persona is still a Go `Register()`. That matches AGENT-GOVERNANCE: do not write agent code in the backend; do not pretend the catalog is a YAML drop-in.
