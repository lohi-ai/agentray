---
project: AgentRay
status: active
updated: 2026-06-30
ui_system: Astryx v0.1.1 (@astryxdesign/core, 148 components)
source_of_truth:
  tokens: web/app/globals.css
  ui_library: "@astryxdesign/core (Astryx)"
  shared_patterns: web/modules/shared/components
  architecture: docs/ARCHITECT-WEB.md
product:
  category: data + agent operating system for SaaS growth teams
  promise: turn product signals into agent-assisted decisions and actions
  audience:
    - founders and growth leads
    - product managers
    - marketers
    - operators
    - technical reviewers in debug mode
principles:
  - Outcome before instrument. Lead with what changed, what matters, and what to do next.
  - Conversation is the front door. Users should be able to ask before they configure.
  - Data earns trust. Every recommendation should make the supporting signal reachable.
  - Agents feel like teammates. Show readiness, health, work in progress, and next action in plain language.
  - Marketing surfaces sell the next value moment. Remove friction between landing, insight, and action.
  - Technical depth is opt-in. Keep payloads, SQL, traces, and debug views available but secondary.
  - Compose Astryx components and AgentRay shared patterns before adding UI. Discover, don't guess.
---

# AgentRay Design System

AgentRay is a dark SaaS product for growth teams that need to understand product usage, traffic quality, and AI-agent work without becoming analytics engineers. The experience is a guided operating cockpit: a conversational front door, a signal-rich growth workspace, and an agent control plane that explains what is happening and what to do next.

This document is the design contract for `agentray/web`. The UI is built on **Astryx** (`@astryxdesign/core`, v0.1.1, 148 components). The visual source of truth is `web/app/globals.css`, which wires Astryx's reset/theme/token layers into Tailwind and pins AgentRay's bespoke dark ramp. Production screens are composed from Astryx components plus the AgentRay shared patterns in `web/modules/shared/components/`.

## Working With Astryx

Astryx is the one and only UI library. Its components own all layout and spacing, so **discover the right component before writing markup** — do not hand-roll structure or guess at props.

### Setup (already wired, do not duplicate)

`web/app/globals.css` already imports the Astryx reset, theme, and Tailwind bridge in an explicit cascade-layer order, and `web/modules/app/theme-root.tsx` provides the app-wide `<Theme theme={neutralTheme} mode="dark">` provider. Components render unstyled without these — but they are global, so never re-import `reset.css` / `astryx.css` per page.

### CLI workflow — discover, don't guess

Run every command as `pnpm exec astryx <cmd>` (written below as `astryx …`). Before writing any UI:

1. `astryx build "<idea>"` — **start here.** Returns a kit: the closest `[page]` template, `[block]`s, and `[component]`s for the idea. No args prints the full playbook.
2. `astryx template <name> [--skeleton]` — scaffold or study the `[page]`/`[block]`s it named. Templates are reference code to learn from.
3. `astryx component <Name>` — props + examples for every component you use.

More CLI:

| Command | Purpose |
|---|---|
| `astryx search "<query>"` | Find any component / hook / doc / template / block |
| `astryx component --list` | All 148 components by category |
| `astryx template --list` | Page + block recipes |
| `astryx docs <topic>` | `color`, `elevation`, `icons`, `illustrations`, `migration`, `motion`, `principles`, `shape`, `spacing`, `styling`, `theme`, `tokens`, `typography` |
| `astryx swizzle <Name>` | Eject component source (`--gap` reports why) |
| `astryx upgrade --apply` | Run after any `@astryxdesign/core` bump |

### Composition rules

- **No raw `<div>` for layout.** Components do all layout and spacing. A full page is an `AppShell`; sidebar navigation is `SideNav`. Reach for an Astryx layout component before a bare element.
- **Custom styling: component props first.** If a prop cannot express it, use Tailwind utilities backed by tokens (`bg-surface-1`, `text-foreground`, `rounded-lg`) via the `tailwind-theme.css` bridge. Never raw hex/px.
- **Tokens for every value** (`astryx docs tokens`). Brand/accent is set via `astryx theme` and the dark bridge in `globals.css` — never override `--color-*` in `:root` of a component.
- **Reuse before adding.** Compose existing Astryx components and AgentRay shared patterns before introducing a new component. Do not add a second UI library, CSS-in-JS runtime, or a one-off visual language.

## Product Positioning

### Core Promise

AgentRay helps a SaaS team answer four questions fast:

1. **What changed?** Traffic, activation, product behavior, or agent work that moved.
2. **Why did it change?** The supporting users, events, sources, sessions, and runs.
3. **What should we do next?** A recommended action, prompt, template, or investigation path.
4. **Which agent can help?** A ready teammate with clear status and safe configuration.

### Experience Model

AgentRay is not a passive dashboard and not a raw event console. It is a **growth operating system with agents**.

- **Ask** — Chat with a general or specialist agent.
- **Notice** — Review movement in traffic, product behavior, agents, and saved boards.
- **Investigate** — Open people, events, replays, or SQL when detail is needed.
- **Act** — Use recommendations, starter templates, saved charts, and agent workflows.
- **Trust** — Inspect source data, tool calls, costs, health, and audit history on demand.

### User Modes

- **Growth mode** — default. Plain language, guided next steps, traffic/product outcomes, starter templates.
- **Operator mode** — agent health, run status, cost, token usage, workspace access, setup safety.
- **Analyst mode** — event tables, SQL, replays, payloads, saved queries.
- **Debug mode** — traces, tool calls, raw payloads, and step inspectors behind explicit disclosure.

The default screen should be useful to a non-technical growth user. Technical reviewers should never lose access to detail, but they should choose to open it.

## Visual Direction

### Mood

- **Primary:** calm, precise, trustworthy.
- **Secondary:** energetic around action moments, never noisy.
- **Trust signal:** readable hierarchy, explicit state, consistent controls, and visible provenance.

### Surface Language

- Use spacing, contrast, and hierarchy before adding borders.
- Build panels from Astryx surface components and the tokenized surface ramp (`bg-surface-1` … `bg-surface-4`); use `rounded-lg` / `rounded-xl` for cockpit panels.
- Use borders (`border-border`) when they clarify a data boundary, a selected state, or a form region.
- Avoid decorative gradients, glassmorphism, emoji icons, oversized heroes, and generic marketing-card grids.
- Keep pages compact but breathable. The screen should feel operational, not theatrical.

### Product Copy

Use short, confident, human labels.

Prefer:
- “What moved” over “Metrics”
- “Best next step” over “Recommendation output”
- “Talk to agent” over “Open” when the action starts conversation
- “Healthy”, “Working now”, “Needs attention”, “Paused” over internal statuses
- “Saved view” over “Dashboard” when speaking to non-technical users
- “Advanced analysis” over unexplained SQL-first framing

Avoid:
- Internal implementation nouns as top-level language
- “No data” without explaining how data appears
- Jargon-only statuses such as “queued”, “errored”, “tool_calls” in default views
- Multiple equal-weight primary buttons in the same local area

## Tokens

Source of truth: `web/app/globals.css`. The AgentRay dark ramp is pinned there and bridged onto Astryx's mode-aware neutral tokens so every component shares one cool blue-slate identity. Use Tailwind classes backed by these CSS variables; inspect the full set with `astryx docs tokens`.

### Colors

| Role | Token | Utility |
|---|---|---|
| App background | `--background` | `bg-background` |
| Text | `--foreground` | `text-foreground` |
| Muted text | `--muted-foreground` | `text-muted-foreground` |
| Faint text | `--faint` | `text-faint` |
| Surface ramp | `--surface-1` … `--surface-4` | `bg-surface-1` … `bg-surface-4` |
| Card surface | `--surface-1` | `bg-card` |
| Popover surface | `--surface-2` | `bg-popover` |
| Primary action | `--primary` / `--primary-hover` | `bg-primary` |
| Primary foreground | `--primary-foreground` | `text-primary-foreground` |
| Agent accent | `--agent` / `--agent-foreground` | `text-agent`, `bg-agent` |
| Data accent | `--data` | `text-data` |
| Success | `--success` | `text-success`, `bg-success` |
| Warning | `--warning` | `text-warning`, `bg-warning` |
| Danger/destructive | `--danger` | `text-danger`, `bg-danger` |
| Border | `--border` | `border-border` |
| Input border | `--input` | `border-input` |
| Focus ring | `--ring` | `ring-ring` |

Raw hex values belong only in the token definitions in `globals.css`, never in component code.

### Radius

Astryx radius scale, defined in `globals.css`:

- `--radius-sm: 6px` → `rounded-sm` for controls
- `--radius-md: 8px` → `rounded-md` for compact data cells
- `--radius-lg: 12px` → `rounded-lg` for panels
- `--radius-xl: 14px` → `rounded-xl` for signature cards and shell regions

### Typography

`--font-sans` resolves to `Inter, system-ui, -apple-system, "Segoe UI", sans-serif`; `--font-mono` to Geist Mono / JetBrains Mono. Practical hierarchy:

| Use | Class pattern |
|---|---|
| App/sidebar brand | `text-sm font-semibold` |
| Page title | `text-xl font-semibold leading-tight` |
| Section heading | `text-xs font-medium uppercase tracking-wide text-muted-foreground` |
| Body | `text-sm` |
| Support copy | `text-xs text-muted-foreground` |
| Dense data/logs | `font-mono` or `tabular-nums` only where data benefits |

Rules:
- Short labels beat dramatic headline typography.
- Avoid giant marketing hero type inside the authenticated product.
- Use uppercase sparingly for stable labels, never paragraphs.

### Spacing

Use Tailwind scale values that match the existing rhythm (`astryx docs spacing`):

- Page stack: `space-y-4` or `space-y-6`
- Panel padding: `p-3` or `p-4`
- Dense rows: `px-3 py-2`
- Compact actions: `gap-1.5` or `gap-2`
- Card grids: `gap-3` or `gap-4`
- Shell gutters: `px-3 py-3` mobile, `lg:px-4 lg:py-4` desktop

Do not introduce off-scale magic spacing for one screen.

## Shared Components and Patterns

### Astryx Components

Astryx (`@astryxdesign/core`) provides the primitives: buttons, inputs, selects, dialogs, cards, sidebars, app shell, and 140+ more. Find what you need with `astryx search "<query>"` or `astryx component --list`, and read props with `astryx component <Name>` before use. Page shells use `AppShell`; sidebar navigation uses `SideNav` / `SideNavItem` / `SideNavSection`.

### AgentRay Shared Layer

Use and extend `web/modules/shared/components/` before adding page-local structure. These wrap Astryx into AgentRay's cockpit language:

- `app-shell.tsx` — `AppShell` (wraps Astryx `AppShell` + `SideNav` with AgentRay nav, account/language/logout footer).
- `signal-primitives.tsx` — `Intro` (page promise), `ContextChips` (project/range/filter context), `StatsStrip` (headline metrics), `StatusPill`, `Callout` (interpretation block), `Panel` (labeled section), `Segment`, `EmptyState`, `BarRows`, `Loading`, plus the AgentRay `Button` variants.
- `stack-sheet.tsx` — `StackSheetProvider` / `useStackSheet` for stacked detail, configuration, and advanced inspection sheets.
- `modal.tsx` — `Modal`, `PromptDialog`, `ConfirmDialog`.
- `data-table.tsx` — `DataTable` with row plugins (`rowNavPlugin`).
- `filter-bar.tsx` — `FilterBar`.
- `charts.tsx` — `Chart`, `Sparkline`, `AreaChart`, `BarChart`, `RetentionChart` (ECharts-backed).
- `event-name-picker.tsx` — `EventNameCombobox`, `EventCatalog`.
- `agent-markdown.tsx` — `AgentMarkdown` for agent text.

### New Shared Pattern Canon

Every major screen should compose this sequence when data allows:

1. **Page intro / promise** — what this screen helps the user do.
2. **Signal strip** — 3–6 key metrics or state facts.
3. **What moved / best next step** — one highlighted interpretation or recommendation.
4. **Primary artifact** — chat, cards, board, table, timeline, or form.
5. **Deep detail** — sheets, tabs, disclosure, SQL, payloads, traces.

If a screen cannot show all five because data is missing, it still needs a guided empty state that explains how to reach the value moment.

### Summary Callouts

Use the shared `Callout` for interpretation blocks:

- label: `text-[11px] uppercase text-muted-foreground`
- title: `text-sm font-semibold`
- detail: `text-xs text-muted-foreground`
- surface: `rounded-lg bg-surface-1` with a border only when adjacent density needs separation

### Primary Actions

Each screen gets one primary action:

- Chat: send/start chat.
- Agents: talk to agent or finish setup.
- Agent health: review issue or open monitor.
- Traffic: review top source/path or filter range.
- Product: run insight.
- Dashboards: create view or add chart.
- Templates: use starter.
- Settings: save or manage access.
- SQL: run saved/ad hoc query.

Secondary actions use the `outline`, `ghost`, or `secondary` variants, or sheet tabs. Do not create two adjacent primary CTAs unless one is clearly the submission and the other is a safe secondary workflow.

## Information Architecture

### Front Door

`/chat` is the front door. Root routes should lead users to conversation, not a static dashboard.

### Top-Level Navigation

Organize navigation by outcome:

#### Main

- **Chat** — ask an agent, start work, discuss recommendations.
- **Agents** — choose, configure, and trust AI teammates.
- **Dashboards** — saved views and repeatable growth checks.
- **Traffic** — acquisition, source quality, page movement, AI platform traffic.
- **Product** — funnels, retention, trends, product questions.
- **Settings** — workspace, projects, members, API safety.

#### Explore

- **People** — users, identity, journey context.
- **Events** — event stream and payload inspection.
- **Replay** — session story and timeline.
- **SQL** — advanced analysis and saved queries.
- **Templates** — starter growth dashboards and proven setups.

Agent monitoring and lab routes are deep routes under Agents. They should inherit the Agents navigation state.

## Screen Canon

### `/chat`

Purpose: start useful work in plain language.

Must include:
- one clear input surface
- agent picker when multiple agents are enabled
- starter tasks tied to growth, product, traffic, and agent operations
- recent work and recommendations near the start state
- debug traces hidden by default
- side panel for live work, runs, and recommendations

### `/agents`

Purpose: understand the AI team, readiness, and next action.

Must include:
- roster cards that describe each agent in human terms
- health/setup summary
- one primary action per agent
- setup, instructions, tools, triggers, secrets, and monitor detail in sheets/tabs
- cost/run facts without turning the card into a log table

### `/agents/monitor` and `/agents/[agentId]/monitor`

Purpose: know whether agents are safe, active, or need review.

Must include:
- plain-language health summary
- current work and recent failures first
- run/tokens/cost metrics second
- traces and raw run detail behind cards, sheets, or tabs
- link to lab or chat for follow-up

### `/agents/[agentId]/lab`

Purpose: test, explain, and safely steer an agent before trusting it.

Must include:
- clear Explain vs Test modes
- saved cases
- step timeline and inspector
- visible run state
- steering only when a run can accept it

### `/dashboard`

Purpose: keep trusted views for repeated growth checks.

Must include:
- saved view selector and description
- “what this view is for” copy
- charts as the primary artifact
- chart creation/editing in sheets
- empty states that steer to templates or first chart

### `/web-analytics`

Purpose: understand traffic quality and acquisition movement.

Must include:
- visitors/pageviews/sessions/conversions summary
- top page, top source, and AI traffic interpretation
- traffic class/provider breakdown
- page/referrer rankings
- filters available but not dominant

### `/product`

Purpose: answer product behavior questions without forcing SQL first.

Must include:
- guided question starters
- trend/funnel/retention/table/agent analysis modes
- event suggestions from current data
- result summary first, chart/table detail second
- advanced controls but simple first-run path

### `/events`

Purpose: inspect product and system events without overwhelming users.

Must include:
- loaded event summary
- scan-friendly table
- session/person path when available
- payload/detail in sheet
- replay handoff for session-level investigation

### `/persons`

Purpose: understand who users are and what journeys matter.

Must include:
- identified vs anonymous summary
- most active user/journey prompt
- table with identity, activity, sessions, last event, last seen
- clear action to view events

### `/replay`

Purpose: explain what happened during a session.

Must include:
- session ID load path
- session totals when loaded
- timeline as primary artifact
- raw detail behind event components

### `/templates`

Purpose: help teams reach value faster from proven setups.

Must include:
- confidence-building copy
- clear template use case
- smallest useful starter guidance
- add-to-view and use-starter CTAs
- chart details expandable, not always fully expanded

### `/settings`

Purpose: manage workspace, project, access, and API safety.

Must include:
- grouped setup/access/activity tabs
- workspace and project context
- permission-aware controls
- key reveal/rotation safety
- plain helper copy for destructive or sensitive actions

### `/sql`

Purpose: support analysts and technical operators.

Must include:
- advanced-mode framing
- saved queries and snippets
- read-only safety language
- results table
- save-to-chart workflow
- no pressure for non-technical users to use SQL before guided views

## States

Every route or major component must account for:

- **Default** — data present, one obvious primary action.
- **Loading** — skeleton or compact loading text matching final structure; avoid full-screen spinners for route content.
- **First run** — explain what to do first.
- **No data yet** — explain how data arrives.
- **No results** — name the filter/range possibility and give a reset/broaden path.
- **No permission** — explain missing role/access and who can fix it.
- **Error** — plain language, recovery action, and detail only when helpful.
- **Submitting/running** — disable duplicate submit, show immediate feedback.
- **Success** — toast, inline confirmation, or updated state.

Do not reuse a single empty state for first-run, no-results, and permission-limited cases.

## Data + Agent UX Rules

### Data Trust

- Show interpretation first, then make the supporting rows or charts reachable.
- Use `StatsStrip` for headline numbers; do not scatter metric cards across a page.
- Pair status color with text. Color is never the only state signal.
- Use `tabular-nums` for counts, currency, tokens, latency, and dates when aligned.
- Keep filters contextual. Filters support investigation; they are not the page purpose.

### Agent Trust

- Agents should expose four facts: what they do, whether they are ready, what they did recently, and what action is safest next.
- Technical traces are opt-in through debug toggles, detail sheets, lab, or monitor routes.
- “Working now” should feel live and reassuring, not mysterious.
- Cost and token usage are trust facts, not the main product promise.

### Growth + Marketing UX

- The value moment should be reachable in three actions or fewer from `/chat`, `/templates`, `/product`, or `/web-analytics`.
- Copy should answer “why this matters for growth” before exposing controls.
- Templates and guided questions should reduce blank-page friction.
- CTA labels should name the outcome: “Run insight”, “Use starter”, “Talk to agent”, “Save as chart”.
- Marketing-facing/auth-adjacent screens should have one primary CTA and a clear next value moment.

## Accessibility

- Text contrast must meet WCAG AA: 4.5:1 for normal text, 3:1 for large text.
- Focus rings use `ring-ring`; every interactive control remains keyboard reachable.
- Touch targets should be at least 44px high/wide or padded to that hit area.
- Inputs use visible labels; placeholders are hints only.
- Dialogs/sheets need `aria-label` or `aria-labelledby`, Escape close, and visible close controls.
- Async status changes that matter should be visible and announced through existing toast/status patterns where available.
- Motion must respect `prefers-reduced-motion`, already handled globally in `globals.css` and by Astryx motion tokens (`astryx docs motion`).

## Responsive Behavior

- Mobile: single-column, vertical stacks, sticky top context, sheets for side panels.
- Tablet: two-column where it improves scanning; avoid cramped dense tables.
- Desktop: sidebar + content grid, max width `1680px`, detail panels only when they help scan.
- Tables must avoid accidental horizontal page scroll; let table containers own overflow.
- Chat keeps the input reachable and side context collapsible.

## Implementation Contract

Follow `docs/ARCHITECT-WEB.md`.

- Route files in `web/app/(analytics)` stay thin.
- Feature modules own page UI.
- Shared patterns live in `web/modules/shared/components/` only after at least two screens need them.
- Prefer frontend composition over backend changes unless data shape blocks the product promise.
- Discover Astryx components with the CLI before building; never hand-roll a primitive Astryx already ships.
- Do not add new design tokens without updating `web/app/globals.css` and this file; set brand/accent via the dark bridge and `astryx theme`, never by overriding `--color-*` in component `:root`.
- Do not introduce a second UI library, CSS-in-JS runtime, or one-off visual language.
- After any `@astryxdesign/core` bump, run `astryx upgrade --apply`.

## Rebuild Checklist

For every AgentRay screen:

- [ ] Purpose is clear in plain language.
- [ ] One best next step is visible.
- [ ] Headline state uses `StatsStrip` or a guided summary.
- [ ] Primary artifact is obvious.
- [ ] Details/debug/raw data are secondary and reachable.
- [ ] Empty/loading/error states are specific.
- [ ] Layout/controls use Astryx components + AgentRay shared patterns (no raw `<div>` layout).
- [ ] No raw color/font/radius/spacing values outside tokens.

## Decision Log

| Date | Decision | Rationale |
|---|---|---|
| 2026-06-19 | Reframe AgentRay as a data + agent operating system for SaaS growth teams | The requested rebuild needs product strategy, IA, and reusable screen patterns, not only visual polish. |
| 2026-06-19 | Keep dark calm cockpit visual language | Existing tokens and components already support trust, density, and agent/data workflows. |
| 2026-06-19 | Make `/chat` the front door and keep SQL/debug secondary | Conversation-first lowers friction while preserving technical trust for operators. |
| 2026-06-30 | Adopt Astryx (`@astryxdesign/core`) as the single UI library, replacing shadcn/`components/ui` | Astryx owns layout/spacing via a discoverable component set + CLI; `globals.css` bridges its neutral theme onto AgentRay's cool dark ramp so one coherent palette ships. |
