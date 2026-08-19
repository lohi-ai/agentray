# AgentRay — UX Review & Market-Fit Check

**Date:** 2026-08-19 · **Method:** signed-in walkthrough of the running local
instance (Go API `:8088`, web `:3200`, workspace "Kiem Lai"), plus source and
live-database inspection.
**Refines:** [`PRODUCT-REVIEW.md`](PRODUCT-REVIEW.md) (2026-07-02), which graded
market fit *C / unproven* and named distribution as the bottleneck. This review
finds a **prior** bottleneck that must clear first.

---

## 1. The headline

Two numbers from the live database decide this review:

| | |
|---|---|
| Open recommendations the agent has filed | **171** |
| Recommendations the owner has ever acted on | **1** |
| Agent runs recorded | **433** |
| Runs that recorded a cost | **0** |
| …of which demonstrably did LLM work (recorded tokens) | **417** |

The agent works. It ran 433 times and produced 171 findings. The owner acted on
**one of them**, and the product cannot say what any of it cost.

That is not a distribution problem. **AgentRay's bottleneck is the
trustworthiness of what the agent hands back.** Trust is the entire product —
"a governed AI analyst watching your product" is worth nothing if the owner
learns to skim past it. Every defect below is a trust defect, and they compound:
each one independently teaches the owner that the numbers on screen are not
load-bearing.

### 1.1 Why 171 findings produced 1 action

A scheduled agent re-derives its findings every cycle and words them slightly
differently each time. Nothing de-duplicated them. `CreateRecommendation` was a
blind `INSERT`; `ListRecommendations` had **no `LIMIT` and no time window**, so
it returned every recommendation the project had ever accumulated.

Measured against the live table with trigram similarity at 0.55, **80 of the 165
open recommendations (48%) restate an earlier one.** On the dashboard this shows
up plainly — two cards, adjacent:

> DATA · IMPACT 92 — "Investigate 24-hour ingestion gap and unplanned subscription replay"
> DATA · IMPACT 90 — "Investigate 24h ingestion gap and subscription-event replay"

One finding, filed twice, ranked separately. Scale that to 171 and the readout
stops being a recommendation surface and becomes a log. The owner's 1-in-171
action rate is the rational response.

Compounding it: `/operations` shows a **Growth Analyst schedule running every 15
minutes with "No prompt set"**. Nothing guards against arming a high-frequency
schedule with no instruction, so the loop that generates the noise is also the
loop nothing was watching.

### 1.2 Numbers the owner cannot trust

Each of these was observed on screen, signed in, on real data:

- **Spend is `$0.00` everywhere** — on `/agents` beside "Tokens (24h) 24.9M", on
  `/operations` beside "Runs (24h) 92", and on every row of `/events`. All 433
  runs record zero cost. One of the product's own advertised starter questions
  is *"Where is my AI spend going this week?"* — the product cannot answer its
  own pitch.

  The root cause is sharper than a missing price-table row, and it matters. This
  deployment runs through an internal OpenAI-compatible gateway using
  **workspace-defined tier aliases** — the `model` recorded on all 732 LLM calls
  is one of `plus` (483), `pro` (211), `lite` (30), `flash` (8). Those are
  labels, not model identities, so *no static price table can ever price them*.
  The defect is that `price()` treated "model not found" and "model costs
  nothing" as the same answer — zero — and every layer below it (Postgres,
  ClickHouse, the API, the web client) collapsed the ambiguity into a confident
  `$0.00`. A false `$0.00` is worse than an honest "unpriced": it reads as a
  fact, and it is wrong.
- **`(0%)` for a rate that is 0.3%.** The front-door headline read *"16 of 4,783
  people (0%) make it from user.pageview to subscription_activated."* Printing
  `0%` next to a non-zero count is the fastest way to teach someone the numbers
  are decorative.
- **The same event occupied two funnel stages at once.**
  `subscription_activated` matched both the *activation* pattern (`activat`) and
  the *revenue* pattern (`subscri`), inventing a phantom 100%-converting step
  between them.
- **"The weakest step" was not a step.** `visit → purchase` skips signup,
  activation and return — all untracked. Calling a four-stage tracking hole "the
  biggest drop" invites the owner to go optimise a step that does not exist. The
  truthful finding — *you cannot see where people drop off* — is both more
  honest and more actionable.
- **Event counts disagree across screens** with no visible scoping: `/dashboard`
  says Events 210, `/events` says Events 100 (the page limit, rendered as a
  total), the insight banner says 4,783 people. Three numbers, one workspace, no
  explanation.
- **Raw event names in the hero position.** `user.pageview →
  subscription_activated` is the developer's word for the step, in the product's
  front door. The design system's own rule says status language must not be
  jargon; the funnel headline had no such discipline.

### 1.3 A roster the owner cannot trust

`/agents` lists **two agents both named "Product Scout"** — one "Ready", one
"Needs attention". Root cause: `web/modules/marketplace/page.tsx` had **no
installed-state check of any kind**; every preset rendered an unconditional
"Install agent" button, and the server had no idempotency guard. Hire twice, get
two. The duplicate then threw a React key collision in the chat agent picker
(`AgentMenu` keyed by display name, not id).

Also on that screen: **4 of 7 agents badged "Needs attention"** with a "Fix
setup" button and no statement of *what* needs attention, while `/start`
simultaneously shows all four setup steps green. Two surfaces, contradictory
answers to "am I set up?".

### 1.4 Craft defects that read as neglect

Individually cosmetic; collectively they set the reliability prior.

| Surface | Defect |
|---|---|
| `/agents` | Card action row clips "Lab" to the single letter "I" |
| `/persons` | X-axis prints raw ISO-8601 UTC (`2026-08-18T10:00:00Z`), leftmost tick clipped |
| `/persons` | Integer people counts smoothed through fractional values; y-axis ticks at 0.5 / 1.5 |
| `/persons` | Trait chips break the key mid-word — "email" renders as "ema" / "il" |
| `/events` | Person column truncates every id to 12 chars, so `00000000-0000-0000-0000-000000000001` and any other zero-prefixed id both render as the identical `00000000-000` — the column silently claims unrelated rows share a person |
| `/events` | Tokens / Cost / Latency print `0` / `$0.00` / `—` on ordinary user events where they do not apply |
| `/operations` | Last-run cell leaks raw markdown: `**Key takeaway: yes…` |
| `/marketplace` | Unbounded card descriptions leave the grid ragged and "Install" buttons unaligned |
| `/marketplace` | Internal skill slugs (`pmf-scorecard`, `weakest-link-triage`) shown to the buyer |
| `/product` | Half the viewport is dead space behind "Pick a question to begin" — the data to answer the first question is already loaded |

**Two corrections to earlier drafts of this list**, both worth recording because
they are the characteristic failure modes of a screenshot-driven review:

- *Retracted:* "sidebar avatar overlaps the user name, every page." It does not.
  Measured in the DOM, the footer avatar is 24×24 at x=20 and the name starts at
  x=52. The circle sitting on top of it is the **Next.js dev-tools button**
  (32×32 at x=22) — `next dev` only, never production. A screenshot cannot tell
  the app's chrome from the toolchain's.
- *Reframed:* the `/events` Person column was first written up as "renders a
  null UUID as if it were an identifier — show Anonymous instead." Checking the
  data first would have caught it:
  `00000000-0000-0000-0000-000000000001` is the lohi API's `PLATFORM_USER_ID`, a
  real system principal behind 784 events, not an anonymous visitor. Labelling
  it "Anonymous" would have replaced one false statement with another. The
  actual defect is the ambiguous 12-character truncation.

### 1.5 Vocabulary drift

One workspace, five words for one concept: **hire** a teammate, **install** an
agent, **add** a specialist, **new** agent, **spin up** from a template. Two for
asking: "Ask AI" and "Ask the agent", on the same screen. And `Armed` /
`Armed channels` / `OPERATOR` on `/operations` — weapons jargon for "this runs
on a schedule".

The nav is the same problem one level up. Its group headings — **Runtime,
Channels, Workloads, Data, Workspace** — are the backend's four architectural
layers, surfaced verbatim as navigation. `ARCHITECT-WEB.md` already concedes the
point: *"That only helps someone who already knows which artifact answers their
question."* A solo founder does not know what a Workload is. The `jobs.ts` model
(validate / grow / operate) is the owner-facing index and it is *better* — but it
lives on `/start`, which is not in the nav at all, reachable only through a small
text button in the chat header.

**`/start` is the clearest screen in the product and the hardest one to find.**

---

## 2. User stories

Grounded in `lib/jobs.ts`, which already models three jobs correctly. Each story
names the gap that currently blocks it.

### Job: validate — "I have an idea, not a product yet"

> **As a founder with no product yet,** I want a teammate to tell me whether
> anyone already pays to solve this problem, so that I can decide to build or
> drop it before writing code.
> **Done when:** I get a written answer with sources in one session, without
> having any events.
> **Status:** works. `needsEvents: false` is the load-bearing design decision
> here and it is right.

> **As a founder about to build,** I want the cheapest test that could prove me
> wrong, so that I spend a week instead of a quarter finding out.
> **Done when:** I get a named test, a success threshold committed in advance,
> and the tracking plan that will measure it.
> **Gap:** the tracking plan is the job's output, but nothing carries it into
> the event store — the owner re-types it.

### Job: grow — "My product is live and I want it to grow"

> **As an owner with a live product,** I want to know the single weakest step in
> my funnel, so that I know where to spend this week.
> **Done when:** one step is named, the drop is quantified honestly, and I can
> open the rows behind the claim.
> **Gap (fixed in this pass):** the headline named a four-stage tracking hole as
> a "step", printed `0%` for 0.3%, and used raw event names.

> **As an owner,** I want to know whether last week's change actually moved the
> number, so that I learn instead of guessing.
> **Done when:** the agent remembers what it recommended and reports back
> against the threshold it committed to.
> **Gap:** the memory exists; the **report-back does not close**. 171 findings,
> 1 acknowledged — nothing follows up on the other 170.

> **As an owner,** I want to trust that a number on screen means what it says.
> **Done when:** two screens showing the same metric over the same window agree,
> and an unknown reads as unknown rather than as zero.
> **Gap:** the largest one. See §1.2.

### Job: operate — "My product is live and I need it to stay up"

> **As an owner,** I want to be told what broke, for whom, and since when —
> before a customer tells me.
> **Done when:** something reaches me outside the app.
> **Gap:** there is **no outbound channel**. Slack, Discord and Telegram all
> render "Not yet". An always-on watcher whose only output surface is a page you
> have to remember to open is not an always-on watcher. This is the single
> biggest *missing capability* in the product.

> **As an owner,** I want to know what my agents cost me this week, so that I
> can leave them running without fear.
> **Done when:** spend is attributed per agent and per schedule, and I can cap it.
> **Gap:** per-agent budgets *do* exist (`agent_budgets`, surfaced on
> `/agents/[id]/setup`, with cost / token / run caps). But the cost cap is
> **silently inert on this deployment**: `SpendForAgentPeriod` sums
> `agent_llm_calls.cost_usd`, which is 0 for all 732 calls because the tier
> aliases are unpriced — so `MaxCostUSD` can never trip. The token and run caps
> still fire. A safety rail that appears armed and cannot fire is worse than a
> missing one, because the owner has already relied on it.

> **As an owner,** I want a finding I have already seen to stop being news.
> **Done when:** a repeat folds into the finding it repeats.
> **Gap (fixed in this pass):** it did not, 48% of the time.

---

## 3. Market fit

### Confirmed, unchanged from the July review

The whitespace is real. Nobody credibly occupies *agents with governed hands
standing on your own event warehouse*: analytics vendors (PostHog, Amplitude,
Mixpanel) don't act; LLM-observability vendors (Langfuse, Helicone, Braintrust)
observe AI but don't own product truth; agent platforms (Lindy, Dust, Zapier
Agents) act but are blind to your data. The ICP is right — solo founders and
2–10-person teams who cannot afford an analyst. The harness quality is real and
rare.

### The refinement — and it changes the ordering

The July review concluded the engineering had out-run the product's **market
exposure**, and ranked distribution first: reposition, then get 3–5 external
design partners.

**That ordering is now wrong, and this walkthrough is why.** The July review
assessed the product by feature inventory. Assessed by *what the one existing
user actually receives*, AgentRay has a prior problem: it does not yet survive
its own dogfood at the output layer. The internal-PMF signal that the July
review graded strong is contradicted by the only behavioural number available —
**171 findings, 1 acted on.** That is not an engaged user. That is a user who
has learned to ignore the feature.

Shipping this to design partners now would spend the scarcest resource the
project has. A stranger's first week would be: a `$0.00` spend figure next to
24.9M tokens, a `0%` conversion rate that is not zero, a duplicate teammate they
did not knowingly hire, and a recommendation feed that repeats itself twice an
hour. They would not file a bug. They would quietly stop opening it, and the
project would learn nothing about the market from their churn.

### Revised grades

| Dimension | July | Now | Why moved |
|---|---|---|---|
| Product idea | A− | **A−** | Unchanged. The loop is still the right identity. |
| Feature depth (for the wedge) | B+ | **B+** | Unchanged. Breadth is genuinely there. |
| **Output quality of the loop** | *not assessed* | **D** | New axis, and the binding one. 48% duplicate findings, 100% of runs unpriced, 1/171 acted on. |
| Market fit today | C / unproven | **C− / unproven** | Same absence of external signal, and the internal signal is weaker than it looked. |
| Market fit potential | B+ → A | **B+ → A** | Unchanged, and still reachable — every defect found is a fixable defect, not a design dead end. |

### Recommended ordering

1. **Make the loop's output trustworthy** — dedupe findings, price the runs,
   never render a false zero, make two screens agree. *(This pass covers most of
   it; see §4.)*
2. **Close the act-path** — one outbound channel (Slack or email digest), and
   make the cost cap that already exists actually able to fire. Without an
   outbound channel the `operate` job does not exist; with an inert cost cap,
   "let it run without you" is a promise the product cannot keep.
3. **Then distribute** — reposition around the loop, then design partners. The
   July review's list is right; it is step 3, not step 1.

The encouraging read: none of this is architectural. The hard part — a governed
agent runtime at Claude-Code parity, sitting on a real event store — is built.
What is broken is the last mile between that machine and the person reading its
output, and the last mile is cheap to fix.

---

## 4. Changes made in this pass

| Area | Change |
|---|---|
| Finding de-duplication | `CreateRecommendation` now folds a repeat into the open finding it repeats (pg_trgm similarity ≥ 0.55, same project + category, 14-day window), recording `seen_count` / `last_seen_at` instead of appending a card. Threshold validated against live titles: the real duplicate pair scores **0.662**, two genuinely different findings score **0.163**. |
| Readout bounding | `ListRecommendations` had no `LIMIT`; now capped at 50 and ordered by `last_seen_at`. |
| Readout UI | A repeated finding now reads "raised N times" — a standing problem stated once, rather than N cards. |
| Index support | `pg_trgm` extension + GIN trigram index on `title`, so the "have I said this already?" check is index-backed rather than a scan of every open row (per the repo's fuzzy-search constraint). |
| Funnel honesty | `formatRate` never rounds a non-zero rate to `0%` (0.3% renders `0.3%`, below 0.1% renders `<0.1%`). |
| Funnel correctness | An event now claims exactly one funnel stage — the deepest it matches — ending the phantom 100% step where `subscription_activated` was both activation *and* revenue. |
| Funnel language | Headlines read in plain stage names ("visit → purchase") with the raw event name kept as evidence, not as the headline. |
| Funnel framing | When the matched stages are non-adjacent, the headline now names the untracked gap instead of calling it "the biggest drop". |

| Cost honesty | `price()` treated "model not found" and "costs nothing" as the same answer. A `CostUnpriced` flag is now threaded end-to-end (agentcore → `agent_runs` / `agent_llm_calls` → rollups → web), and `formatCost` renders `—` rather than `$0.00` when the price is unknown. **Verified live: `/agents` now reads "Spend (24h) —" beside "Tokens (24h) 25.9M".** |
| Cost backfill | The new column's constant default marked all 434 historical runs "priced, and it cost $0.00" — reproducing the exact false zero on every existing row. Backfilled: a run that burned tokens and recorded `$0` is unpriced by definition. 417 runs reclassified; the 17 with zero tokens correctly stay free. |
| Duplicate teammates | Root cause was `freeAgentSlug` *numbering* a second install (`product-scout` → `product-scout-2`) rather than refusing it. Agents now record `preset_slug`, install is idempotent (returns the existing agent), and the marketplace renders "On your team" + "Open {name}". `AgentMenu` keys by agent id, ending the React key collision. The existing duplicate was removed and `preset_slug` backfilled so pre-fix hires are protected too. |
| `/agents` action row | "Lab" no longer clips to "I" — icon-only with an accessible label. |
| `/persons` chart | Axis ticks render as local time (`16:00`, `17:00`) instead of raw ISO-8601 UTC; smoothing off and integer y-ticks for count series, so the chart no longer plots 1.5 people. |
| `/persons` traits | Chip keys no longer break mid-word; values truncate with the full text in a tooltip. |
| `/events` columns | Tokens / Cost / Latency render `—` on event types where they do not apply, instead of a false `0` / `$0.00`. |
| `/events` person ids | Replaced the ambiguous 12-char truncation with a lead-and-tail short form plus full id on hover, so two different people stop rendering as the same string. |

## 5. Not addressed — ranked for the next pass

1. **No outbound channel.** Slack/email digest. Blocks the `operate` job entirely.
2. **Cost budgets cannot fire on aliased models.** The caps exist; the cost
   dimension sums an always-zero column. Either price the aliases (let a
   workspace declare a per-alias rate alongside the tier mapping) or make an
   unpriced run refuse to count as free — silently under-enforcing is the worst
   of the three options. Token and run caps are unaffected.
3. **Schedule guards.** A 15-minute schedule with no prompt should not be armable.
4. **Setup state contradicts itself** between `/start` and `/agents`; "Needs attention" never says what needs attention.
5. **Nav names the backend's layers, not the owner's jobs**; `/start` — the clearest screen — is unreachable from the nav.
6. **Vocabulary drift:** settle on one verb for hiring, one for asking, and retire "Armed".
7. **`/events` "Events 100"** renders a page limit as a total.
8. **`/product` opens dead** — auto-run the first question.
9. **Test fixtures in the roster** ("E2E Test Agent") on a real workspace.
