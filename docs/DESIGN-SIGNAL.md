---
project: AgentRay
design_language: Signal
status: proposed
supersedes: DESIGN.md
updated: 2026-06-21
source_of_truth:
  tokens: web/app/globals.css   # to be rewritten to the Signal tokens below
  ui_primitives: web/components/ui
  shared_patterns: web/modules/shared
product:
  category: data + agent operating system for SaaS growth teams
  promise: turn product signals into agent-assisted decisions and actions
flagship_screen: /chat (agent workspace)
---

# AgentRay — "Signal" Design Language

A fresh visual + UX direction for AgentRay, designed from what the product
actually does rather than from the previous look. The previous design was a
generic Linear-indigo-on-pure-black theme; **Signal** replaces it with a system
where the canvas is calm and dim, and **color only appears where there is
signal** — a metric that moved, an agent working, an action to take. Color means
something; nothing decorative competes with it.

This doc defines the design language (tokens, type, surface, motion, components)
and fully specs the flagship screen, **`/chat`**. Every other screen follows the
patterns established here.

---

## 1. The product, in one breath

AgentRay turns product/traffic signals into agent-assisted decisions. The loop is
**Signal → Sense → Act**, with AI teammates doing the work. The interface is an
*operating cockpit*: chat is the front door, agents are visible coworkers, and
data is always reachable but never shouted.

Three truths the design must carry:

1. **It is alive.** Agents run, stream, and finish work. The UI shows live state
   honestly — "working now" should feel reassuring, not mysterious.
2. **It is an instrument.** Numbers, runs, costs, traces. They must read precise
   and aligned, like a console, not like a marketing dashboard.
3. **It is calm.** Operators live here for hours. Low ambient brightness, high
   contrast only where it earns attention.

---

## 2. Visual concept — "Signal"

> Dim observatory canvas. Depth comes from **elevation**, not borders. Color is a
> scarce resource spent only on meaning: live data, agent activity, primary action.

Departures from the old look (deliberate, so it reads new):

The palette is **"Spring"** — color maps to the product's three jobs: **growth**
gets the brand hue (emerald), **agents** get an intelligence spark (iris), and
**data** stays cool and precise (sky + neutral).

| Old | Signal / Spring |
|---|---|
| Pure black `#010102` | Cool-neutral **ink** `#0A0E12` — softer on the eyes for long sessions |
| Linear indigo `#5e6ad2` as primary | **Emerald growth** `#22C786` — reads momentum/up/money, owns "growth", not purple-slop |
| Borders everywhere | **Stepped elevation** (4 surface levels); borders only mark data boundaries |
| One accent | **Role-mapped accents**: emerald = growth/action, **iris** `#8A7CFF` = agent/AI, **sky** `#46B7E8` = data |
| 20px signature radius | Calmer **radii**: 14px panels, 8px controls — instrument-crisp |
| Inter for everything | Inter for UI + **mono for all data/numbers** to reinforce the console feel |

No gradients-as-decoration, no glassmorphism, no emoji icons, no oversized heroes
inside the product.

---

## 3. Tokens

These replace the values in `web/app/globals.css`. Hex appears **only** here; in
component code use the Tailwind classes backed by these variables.

### Color — canvas & ink

| Role | Token | Value | Use |
|---|---|---|---|
| App background (ink) | `--background` | `#0A0E12` | the canvas |
| Surface 1 | `--card` | `#11161F` | panels, cards |
| Surface 2 | `--popover` / `--secondary` | `#161C26` | popovers, raised rows, composer |
| Surface 3 | `--muted` | `#1E2733` | hover, selected row, inset wells |
| Surface 4 | `--accent` | `#28323F` | active/pressed, chips |
| Foreground | `--foreground` | `#E9EEF4` | primary text |
| Muted text | `--muted-foreground` | `#8C97A8` | support copy, labels |
| Faint text | `--foreground-faint` | `#5C6678` | timestamps, meta |
| Hairline border | `--border` | `#222C38` | data boundaries only |
| Input border | `--input` | `#2C3645` | form controls |

### Color — growth, agent, data, meaning

| Role | Token | Value | Use |
|---|---|---|---|
| Primary / growth | `--primary` | `#22C786` | primary action, CTAs, "up"/positive, focus glow |
| Primary hover | `--primary-hover` | `#3FE0A0` | |
| Primary foreground | `--primary-foreground` | `#04221A` | text on emerald (dark, for contrast) |
| Agent / AI | `--agent` | `#8A7CFF` | agent identity, "working now", thinking |
| Agent foreground | `--agent-foreground` | `#0E0828` | text on iris |
| Data / info | `--data` | `#46B7E8` | charts, links, neutral data emphasis |
| Success | `--semantic-success` | `#3FCB7A` | healthy, completed (always paired with text) |
| Warning | `--semantic-warning` | `#E8A93B` | needs attention |
| Danger | `--destructive` | `#F0626B` | failed, errored, destructive |
| Focus ring | `--ring` | `color-mix(--primary 60%)` | keyboard focus |

Emerald is both brand and the "positive movement" language — fitting a growth
product. Success stays a distinct lighter green **and is always paired with a
label/icon** so the two greens never have to be told apart by hue alone. Iris is
reserved for agents so "an AI is doing this" reads instantly; sky carries neutral
data so charts don't fight the brand.

### Charts

Emerald-anchored, rotating through the role accents so series stay distinct on the
dim canvas:

`--chart-1 #22C786` (emerald) · `--chart-2 #8A7CFF` (iris) · `--chart-3 #46B7E8` (sky) · `--chart-4 #E8A93B` (amber) · `--chart-5 #7E8AA0` (slate)

### Radius

`--radius: 0.875rem` (14px).
- `rounded-sm` 6px — chips, badges, inline tags
- `rounded-md` 8px — buttons, inputs, message bubbles
- `rounded-lg` 12px — data cells, list rows
- `rounded-xl` 14px — panels, cards, sheets
- The old `rounded-[20px]` shell radius is **retired**.

### Typography

```
--font-sans: "Inter", system-ui, sans-serif;
--font-mono: "Geist Mono", "JetBrains Mono", ui-monospace, monospace;
```

Body letter-spacing `-0.01em`. **All numeric/data uses `--font-mono` +
`tabular-nums`** — counts, currency, tokens, latency, dates-in-tables, run ids.

| Use | Class |
|---|---|
| Brand / app name | `text-sm font-semibold tracking-tight` |
| Page title | `text-lg font-semibold tracking-tight` |
| Section label | `text-[11px] font-medium uppercase tracking-[0.08em] text-muted-foreground` |
| Body | `text-sm leading-relaxed` |
| Support | `text-xs text-muted-foreground` |
| Data / metric | `font-mono tabular-nums` |

No giant hero type inside the authenticated product.

### Spacing & elevation

Spacing scale unchanged from Tailwind; rhythm:
- Page stack `space-y-5` · panel padding `p-4` · dense rows `px-3 py-2.5`
- Card grids `gap-3`/`gap-4` · shell gutters `px-4 py-4`

**Elevation** replaces borders as the primary depth cue:
- `elev-0` background (ink)
- `elev-1` surface-1, no shadow
- `elev-2` surface-2 + `shadow-[0_1px_0_rgba(255,255,255,0.03)_inset]` (top hairline of light)
- `elev-3` surface-3 + soft drop `shadow-[0_8px_24px_-12px_rgba(0,0,0,0.6)]` (sheets, popovers, dialogs)

### Motion

```
--motion-fast: 120ms;   /* hover, press */
--motion-base: 200ms;   /* enter/exit, panel slide */
--motion-slow: 320ms;   /* sheet, large transitions */
ease: cubic-bezier(0.2, 0.8, 0.2, 1)
```

Signature live motion (the product feeling alive):
- **Pulse** — a 2s iris breathing dot for "working now" (agent activity = iris). `prefers-reduced-motion` → static dot.
- **Signal sweep** — on a new agent result, a one-shot 320ms emerald underline sweep beneath the result card header.
- **Stream caret** — a blinking emerald block caret while assistant text streams.

All gated behind `prefers-reduced-motion` (already wired globally).

---

## 4. Component skins (existing primitives, new clothes)

Reuse the shadcn primitives in `web/components/ui/` and the shared layer in
`web/modules/shared/`. Signal changes their *appearance via tokens*, not their API.

- **Button** — primary = solid emerald, dark text; secondary = surface-2; ghost = transparent → surface-3 on hover; destructive = solid danger. Height 36px (≥44px touch via padding on mobile).
- **Badge / status pill** — always **dot + text** (color never the only signal): `● Working`, `● Healthy`, `● Needs attention`, `● Paused`.
- **Card / Panel** — surface-1, `rounded-xl`, no border by default; border only when two equal-density panels touch.
- **Input / Textarea / composer** — surface-2, `--input` hairline, emerald focus ring.
- **Sheet (`AgentRaySheet`)** — `elev-3`, slides `--motion-slow`, for detail/config/build/authoring.
- **StatStrip / Stat** — mono numbers, label below, optional delta with semantic color + arrow glyph.
- **EmptyState** — distinct per case (first-run / no-data / no-results / no-permission / error). Never one generic empty state reused.

New shared primitives introduced by Signal (built reusable, not page-coupled):

- **`LiveDot` [NEW]** — pulsing status dot. Props `{state: 'working'|'healthy'|'attention'|'paused'|'idle'}`. Justify: "working now" needs a consistent live indicator across chat, agents, monitor; no existing component covers it. Lives in `modules/shared/`.
- **`ResultCard` [NEW]** — the inline artifact an agent emits in chat (a metric, mini-chart, table preview, or recommendation) with a header, body, and one action. Props `{kind, title, data, action}`. Justify: agent answers must render as scannable cards in the conversation; reused by chat, action-center, and dashboard previews.
- **`AgentChip` [NEW]** — avatar + name + `LiveDot`, used in composer, header, roster. Justify: agent identity recurs everywhere and must look identical.

---

## 5. Information architecture

`/chat` is the front door. Navigation organized by outcome, unchanged in
structure but re-skinned:

**Main:** Chat · Agents · Dashboards · Traffic · Product · Settings
**Explore:** People · Events · Replay · SQL · Templates
Agent **monitor** and **lab** are deep routes under Agents and inherit its nav state.

The global shell: left sidebar (220px, collapsible to icon rail), content grid
max-width 1680px. On `/chat` the page header is suppressed (the conversation is
the context).

---

## 6. Flagship screen — `/chat` (agent workspace)

### Intent

The user opens AgentRay and immediately *asks* — no setup, no blank dashboard.
They talk to an agent in plain language; the agent answers with scannable result
cards and, on the side, surfaces what it's doing and what to act on next. "It
works" feels like a knowledgeable teammate who shows their work without burying
you in it.

### Layout (desktop, mirrors a 3-zone cockpit)

```
┌────────────────────────────────────────────────────────────────────────────┐
│ [≡] AgentRay        ·  Project: Acme  ·  ⌘K               [Threads] [Panel] │  ← thin context bar (40px), no page title
├──────────┬───────────────────────────────────────────────┬─────────────────┤
│ THREADS  │                CONVERSATION                    │  WORK PANEL      │
│ rail     │                                                │  (action center) │
│ 240px    │   ┌─ agent message ─────────────────────────┐ │  ┌─ tabs ──────┐ │
│ collapse │   │ AgentChip ● working                      │ │  │ Recs│Act│Run│ │
│          │   │ "Sessions dropped 12% from /pricing…"    │ │  └─────────────┘ │
│ + New    │   │ ┌ ResultCard: mini-chart ─────────────┐ │ │                  │
│          │   │ │ Sessions  4,210  ▼12%   [Open]       │ │ │  ● 3 recs to    │
│ ▸ Today  │   │ └─────────────────────────────────────┘ │ │     review       │
│  Pricing │   └──────────────────────────────────────────┘ │  ┌ rec row ───┐ │
│  drop    │                                                │  │ Fix /pricing│ │
│ ▸ Earlier│   ┌─ user message ──────────────────────────┐ │  │ [Act] [Skip]│ │
│  …       │   │ "why did pricing sessions fall?"         │ │  └────────────┘ │
│          │   └──────────────────────────────────────────┘ │                  │
│          │                                                │  Activity stream │
│          │  ┌── composer (sticky bottom) ──────────────┐ │  · run finished  │
│          │  │ [AgentChip ▾] Ask anything…       [↑Send]│ │  · 2 events…     │
│          │  │ ⌥ debug  · ⎙ attach                       │ │                  │
│          │  └──────────────────────────────────────────┘ │                  │
└──────────┴───────────────────────────────────────────────┴─────────────────┘
```

- **Context bar** (40px, `elev-0`): brand/collapse, project chip, `⌘K` command, and two toggles — **Threads** (left rail) and **Panel** (right work panel). No page title; the conversation is the subject. (`spacing` gutters `px-4`.)
- **Threads rail** (240px, surface-1, collapsible): `+ New chat` primary, threads grouped Today / Earlier, active thread = surface-3 with a 2px emerald left edge. Each row: title + `LiveDot` if its agent is running.
- **Conversation** (center, max-w `760px` centered for readability): message stream + sticky composer.
- **Work panel** (320px, surface-1, collapsible): action center — tabs **Recommendations / Activity / Runs**.

### Conversation detail

- **Message bubbles** — user: surface-2, right-weighted, `rounded-md`. Agent: no bubble fill (flows on canvas), led by an `AgentChip` with `LiveDot`; this keeps agent answers feeling like the environment talking, not a chat partner boxed in.
- **ResultCard** inline — when the agent returns data, it renders a `ResultCard` (metric / mini-chart / table preview / recommendation) with one action ("Open", "Save as chart", "Run insight"). Signal sweep animates once on arrival.
- **Streaming** — progress line under the AgentChip ("Reading 3 events sources…") with a pulsing iris `LiveDot`; assistant text streams with the emerald block caret; tool traces collapsed behind a `⌥ debug` toggle (off by default).
- **Composer** (sticky, surface-2, `elev-2`): `AgentChip ▾` picker on the left, autosize textarea, `↑ Send` primary (emerald) on the right. Secondary affordances (debug, attach) as ghost icons below. Enter sends, Shift+Enter newline.

### Work panel detail (action center)

Tabs auto-select: **Recommendations** if any are open, else **Activity** while a
run streams, else **Runs**.
- **Recommendations** — rows with title, one-line why, `[Act]` (emerald) + `[Skip]` (ghost). Count badge on the tab.
- **Activity** — reverse-chron stream of run/event facts, faint timestamps (mono).
- **Runs** — recent runs with `LiveDot`, duration, tokens, cost (all mono/tabular). Row → opens run detail in `AgentRaySheet`.

### Empty / first state

Center column shows the front door, not an empty box:
- Headline (not hero): "What do you want to figure out?"
- `AgentChip` picker preselected to the default agent.
- **Starter prompts grouped by outcome**: *Traffic* ("Where is my best traffic coming from?"), *Product* ("Which feature drives retention?"), *Agents* ("What did my agents do today?"). Three chips per group, surface-2, click → fills composer.
- Below: "Recent" thread shortcuts if any exist.

### States

- **Default** — threads + conversation + panel; composer focused; one primary action (Send).
- **Loading (thread)** — skeleton message rows matching final structure; never a full-screen spinner.
- **First run (no agent configured)** — front door replaced with a guided card: "Connect a model key to start" → `[Set up agent]` opening the build sheet. Explains *why* before the control.
- **No data yet** — agent answers honestly: a `ResultCard` empty variant "No events in range yet — here's how data arrives" + link to setup.
- **No results** — agent names the filter/range and offers to broaden ("No pricing sessions in last 24h — try 7 days?").
- **No permission** — composer disabled with inline note: "You can view but not run agents. Ask a workspace admin to grant runner access."
- **Streaming / running** — Send becomes a **Stop** control (danger ghost); duplicate submit blocked; iris `LiveDot` live; progress text visible.
- **Error** — assistant message in plain language with a `[Retry]`; raw error behind `⌥ debug`.
- **Success** — ResultCard arrives with signal sweep; if it produced a saved artifact, an inline confirm ("Saved to Dashboards →").

### Interactions

- **Send** — Enter; disabled while empty or streaming; optimistic user bubble appears immediately.
- **Stop** — interrupts stream; partial text retained, marked "stopped".
- **Steer** — while a run accepts it, an inline "Add direction…" affordance appends guidance mid-run (existing `steer` flow), iris-accented.
- **Agent switch** — `AgentChip ▾` in composer; switching mid-empty-thread changes the thread's agent, switching mid-populated opens a new thread (existing behavior, kept).
- **Panel/Threads toggles** — `--motion-base` slide; collapsed state persisted per project.
- **⌘K** — command palette: jump to agent, thread, or screen.
- Micro: hover rows lift to surface-3 in `--motion-fast`; `LiveDot` pulse `2s`.

### Edge cases

- **Long thread titles** — truncate with ellipsis, full title in `title` attr.
- **Many agents** — composer picker becomes searchable (`Command`) past ~8.
- **Concurrent runs** across threads — each thread row shows its own `LiveDot`; panel Runs tab aggregates.
- **Very large ResultCard table** — preview first N rows + "Open full table" → Events/SQL screen; never dump 1000 rows in chat.
- **Stream drops / network loss** — message marked "connection lost", `[Retry]`, no silent failure.
- **Reduced motion** — pulses/sweeps/caret become static states.

### Accessibility

- Tab order: context bar → threads rail → conversation → composer → work panel.
- Composer textarea has a visible label (sr-only ok) and `aria-describedby` for the send hint.
- Focus returns to composer after a stream completes.
- `LiveDot` state announced via `aria-label` ("Agent working") and never color-only (dot shape + text label present).
- Streaming output in an `aria-live="polite"` region (throttled) so screen readers aren't flooded.
- Sheets: `aria-labelledby`, Escape close, visible close, focus trap.
- Contrast: foreground/ink ≥ 12:1; emerald-on-ink for text used only at ≥14px; emerald as a fill carries dark `--primary-foreground` text.
- Touch targets ≥44px (composer send, toggles, rec actions).

### Responsive

- **Mobile** — single column: conversation full-width; Threads and Work panel become `Sheet`s opened from the context bar; composer sticky to bottom safe-area.
- **Tablet** — conversation + one side panel at a time (toggle threads/work); the other is a sheet.
- **Desktop** — full 3-zone; panels collapsible; content max-width 1680px, conversation column max-w 760px.

---

## 7. The other screens

### Shared spine

Every authenticated screen (except `/chat`, which is the conversation) composes
the same five-band rhythm inside the global shell (sidebar + 1680px content grid):

1. **Page intro** — title + one-line promise + the single best next action (primary button, right-aligned).
2. **Context bar** — project · range · filters, as chips. Filters are contextual, never the page subject.
3. **Signal strip** — `StatStrip` of 3–6 headline metrics (mono/tabular, with deltas in semantic color + glyph).
4. **What moved / best next step** — one interpretation callout (the "read"), surfaced before the raw artifact.
5. **Primary artifact + deep detail** — chart / table / board / roster, with detail in `AgentRaySheet`.

Color discipline carries through: **emerald** = action & positive movement,
**iris** = anything an agent is doing, **sky** = data emphasis (charts, links),
neutral surfaces for everything else.

### `/agents` — the AI team roster

- **Intro:** "Your agents" · "Configure and trust the teammates doing the work." · primary `+ New agent`.
- **Signal strip:** Active · Working now · Needs attention · Runs today · Spend (24h) — all mono.
- **Artifact:** responsive grid of **agent cards** (`elev-1`, `rounded-xl`). Each card:
  - header: `AgentChip` (iris avatar + name) + `LiveDot` status pill (`● Working` / `● Healthy` / `● Needs attention` / `● Paused`).
  - one-line plain-language description of what it does.
  - a thin 3-fact row (mono): last run · runs (24h) · cost (24h).
  - one **primary action**: `Talk to agent` (emerald) when ready, or `Finish setup` (emerald) when first-run.
  - secondary: `⋯` menu → Configure / Monitor / Lab / Pause, opening `AgentRaySheet`.
- **States:** first-run (empty grid → one "Create your first agent" guided card); needs-attention card gets a warning-tinted left edge + an inline `Review` link; paused cards de-emphasized.

### `/agents/[id]` build & authoring (sheet, not a route page)

`AgentRaySheet` with tabs: **Instructions · Tools · Triggers · Secrets · Skills · Recipe**.
Save is the one primary (emerald); destructive (delete agent) lives last with plain
warning copy and a confirm. Tool/secret rows use mono ids; a `● connected` /
`● missing` dot per secret.

### `/agents/monitor` — fleet health

- **Intro:** "Agent health" · "Know what's safe, active, or needs review." · primary `Open live monitor`.
- **Signal strip:** Healthy · Working now · Failures (24h) · Avg latency · Tokens (24h) · Spend (24h).
- **What moved:** a health callout — e.g. "1 agent needs attention: Nudge Agent failed 3 runs on a missing secret." with a `Fix` action (iris-accented, since it's agent work).
- **Artifact:** a **fleet table** — agent · status `LiveDot` · current work · last failure · runs · p95 latency · cost. Failures and "working now" sort to the top. Row → run detail sheet with the step timeline + traces (debug-gated).

### `/agents/[id]/lab` — test, explain, steer

- Two-mode segmented control: **Explain** (why it would act) vs **Test** (run a case safely).
- Left: saved cases list + `+ New case`. Center: the run — step **timeline** (each step a row with an iris `LiveDot` while active) and an **inspector** for the selected step (input → tool → output, mono). Right: live run state + a **steer** box (iris) enabled only while a run accepts steering.
- States: idle (pick or write a case), running (timeline streams, steer live), done (verdict callout: pass/▲risk), error (failed step highlighted danger).

### `/web-analytics` (Traffic) — acquisition & traffic quality

- **Intro:** "Traffic" · "Where visitors come from and which sources are worth more." · primary `Ask about traffic` (jumps to `/chat` pre-seeded).
- **Signal strip:** Visitors · Pageviews · Sessions · Conversions · Avg session · AI traffic % — mono with deltas.
- **What moved:** interpretation callout — "Best source this week: **Organic** (+18%). AI crawler traffic up 2.4×." with a `Save as board` action.
- **Artifact:** a primary **time-series chart** (sky-anchored, area), then two side-by-side ranking tables (Top pages · Top sources) and a traffic-class breakdown (Human / AI / Bot) as a compact bar. Filters (range, source, path) in a contextual popover, not on the page.

### `/product` — product behavior questions

- **Intro:** "Product" · "Answer behavior questions without writing SQL first." · primary `Run insight`.
- **Mode segmented control:** Trend · Funnel · Retention · Table · Agent analysis.
- **Guided start:** question-starter chips grouped by intent + event suggestions pulled from current data, so the blank page is never blank.
- **Artifact:** result **summary callout first** (the answer in a sentence + key number), then the chart/table. Advanced controls (breakdown, filters, event picker) sit in a right config strip, collapsed on first run. A `Save as chart` (emerald) and `Ask the agent about this` (iris) close the loop.

### `/dashboard` (Dashboards) — saved views

- **Intro:** view selector (`Select`) + "what this view is for" description · primary `Add chart`.
- **Artifact:** responsive **chart grid** (`ResultCard`-style tiles, sky data, mono numbers), drag to reorder, each tile `⋯` → Edit (sheet) / Duplicate / Remove. Empty view → guided state steering to Templates or "Create first chart".

### `/events`, `/persons`, `/replay` — the instrument tables

- Shared shape: loaded-summary line → **scan-friendly table** (mono ids, tabular counts, faint timestamps) → detail in `AgentRaySheet`.
- **Events:** event · person · time · source; row → payload sheet (JSON, mono) + `Open session replay`.
- **Persons:** identified vs anonymous summary; table identity · activity · sessions · last event · last seen; row → person events.
- **Replay:** session-id load path → session totals strip → **timeline** as the primary artifact (each event a row, raw detail behind disclosure).

### `/sql` — advanced analysis

- **Intro:** "Advanced analysis" framing + read-only safety line (plain copy). Not pushed on non-technical users.
- Left: saved queries + snippets. Center: editor (mono) + `Run` (emerald). Below: results table (mono/tabular) with `Save as chart`. Errors render plain-language first, raw error behind disclosure.

### `/templates` — proven starter setups

- Confidence copy up top. Grid of template cards: use-case · the smallest useful starter · expandable chart list (collapsed by default). Each card: `Use starter` (emerald) + `Add to a view`.

### `/settings` — workspace, access, API safety

- Grouped **tabs:** Workspace · Projects · Members · API keys · Activity.
- Permission-aware controls (disabled + reason when lacking role). API keys: masked by default, reveal/rotate with confirm + plain warning copy. Destructive actions last, plainly labeled.

### Rebuild checklist (per screen)

- [ ] Purpose clear in plain language; one best next step visible.
- [ ] Headline state via `StatStrip`; one interpretation before raw data.
- [ ] Primary artifact obvious; detail/debug/raw secondary (sheets/disclosure).
- [ ] Empty / loading / first-run / no-results / no-permission / error all specific.
- [ ] shadcn + shared AgentRay components only; no off-token color/font/radius/spacing.
- [ ] **Color spent only on role** — emerald=action/growth, iris=agent, sky=data — never decoration.

---

## 8. Internationalization (i18n)

AgentRay must ship multi-locale from the design layer up, not as a retrofit.
English is the source locale; **Vietnamese (`vi`) is the first additional locale**
(team + market context). The design contract:

### Strings
- **No hardcoded user-facing copy.** Every label, button, empty state, status,
  recommendation chrome, and helper line comes from a message catalog keyed by id
  (`agents.title`, `status.working`, …). Use **`next-intl`** (App Router native);
  catalogs live in `web/messages/<locale>.json`.
- **ICU MessageFormat** for plurals/gender/select ("1 run" / "148 runs" /
  "0 lượt chạy"). Never concatenate translated fragments.
- **Do translate:** navigation, status labels, empty/error/first-run states,
  CTAs, callout labels ("What moved" → "Điều thay đổi"), tooltips.
- **Don't translate:** raw data — event names, SQL, agent ids, payload keys, user
  content. These stay canonical and render in `--font-mono`.

### Numbers, dates, currency
- Format via `Intl.NumberFormat` / `Intl.DateTimeFormat` / `Intl.RelativeTimeFormat`
  bound to the active locale. Metrics keep `tabular-nums` regardless of locale.
- Billing currency is fixed (USD) but formatted per-locale (`$3.91` / `3,91 $`).
- Relative times ("2m", "18m") localize ("2 phút"); table timestamps use the
  locale's date order.

### Layout — text expansion & RTL
- **Plan for +35% text.** Vietnamese, German, French run longer than English.
  Buttons, chips, stat labels, and nav must flex and truncate-with-tooltip — never
  fixed-width text boxes or single-line assumptions. The Signal card/chip/strip
  layouts already flex; keep it that way.
- **RTL-ready by construction.** Use CSS **logical properties** (`ms-*`/`me-*`,
  `ps-*`/`pe-*`, `text-start`, `border-s`) and `dir` on `<html>`. The shell, the
  thread rail, the 2px accent edges, and `LiveDot` placement mirror under
  `dir="rtl"`. Even before an RTL locale ships, building logical means it's a
  config flip, not a rebuild.

### Agent output & fonts
- **Agents answer in the user's locale** — the run prompt carries `locale`; agent
  prose, recommendations, and result-card titles are localized. Underlying
  data/ids stay canonical.
- Inter covers Latin + full Vietnamese diacritics (stacked tone marks verified).
  When CJK locales ship, add `"Noto Sans SC"/"Noto Sans JP"` to the font stack.
- Icon-with-text is preferred where space is tight, but **never icon-only for a
  primary action** — both an a11y and an i18n safeguard.

### Locale selection
- Resolved from user setting → workspace default → `Accept-Language` → `en`.
- Switchable in Settings and from the shell; persisted per user. URL may carry
  `/<locale>/…` for shareable localized links.

---

## 9. Decision log

| Date | Decision | Rationale |
|---|---|---|
| 2026-06-21 | Fresh "Signal" language; retire Linear-indigo/black theme | User judged current design weak; product needs an identity that reads "alive instrument", not a generic dark SaaS clone. |
| 2026-06-21 | "Spring" palette: emerald growth + iris agent + sky data; color = role | Maps the three product jobs (growth, agent, data) to color so meaning reads instantly; emerald owns "growth", avoids purple-slop. |
| 2026-06-21 | Depth via 4-step elevation, borders only for data boundaries | Reduces visual noise; reads as a console. |
| 2026-06-21 | Mono + tabular for all data | Reinforces the instrument feel; aligns metrics. |
| 2026-06-21 | `/chat` as flagship, 3-zone cockpit with inline ResultCards | Conversation-first front door is the product's core promise; result cards make agent answers scannable. |
| 2026-06-21 | New shared primitives: LiveDot, ResultCard, AgentChip | Recurring needs (live state, agent answers, agent identity) deserve one reusable component each. |
| 2026-06-21 | Lucide icon set, no emoji/glyph icons | Matches the app's `lucide-react`; consistent stroke weight reads as an instrument; emoji break across platforms/locales. |
| 2026-06-21 | i18n from the design layer (next-intl), `vi` first, RTL-ready via logical CSS | Multi-locale is a launch requirement; retrofitting layout for text expansion + RTL is far costlier than building logical now. |
