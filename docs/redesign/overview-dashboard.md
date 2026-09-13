# Implementation spec: value-first Overview

Status: revised 2026-09-13. Companion to [strategy.md](strategy.md) and [design.md](design.md). Prototype: [overview-states.html](overview-states.html) — open locally, use the state switcher at the top. Every figure in the prototype is illustrative; production renders a metric's state label, never a number without a verified source.

> **Scope correction (2026-09-13, authoritative).** The App Store Connect analytics screen was a **visual/layout and information-hierarchy reference only** — never a request to connect a store source. Every App Store Connect connector, credential flow, report-ingestion path, Apple API call, Apple-specific metric, Apple provenance line, Apple-only route, iOS-only gating rule and Apple-branded surface that earlier revisions of this spec carried is **superseded and must not be implemented**. AgentRay builds its own dashboard from existing AgentRay events, sources and Plans data. The AgentRay-only successor spec is the child ticket **`bs-neaqk9f0` — "Template-inspired AgentRay dashboard using AgentRay data only"**; this document keeps the generic Overview/IA guidance that child reuses. Unsupported acquisition, revenue, subscription or crash metrics stay honestly `Not available` / `Set up` — never invented, never attributed to a store.

## User, job, entry point

- **User:** builder or small product team operating a web and/or app product.
- **Primary job:** understand whether people arrive, activate and return — and see the single most useful next investigation — before configuring anything.
- **Entry point:** `/overview` is the front door (already shipped). This spec extends it with grouped metric panels, platform-conditional navigation, and a Plans-backed "best next step" panel.

## Layout and controls

Compose from existing components only: `AppShell`/`SideNav`, `PageShell`, `Panel`, `StatsStrip`, `Chart`, `BarRows`, `Callout`, `EmptyState`, `Segment`, `StatusPill`, `Button`, `ContextChips`. No new tokens, no new UI library.

1. **Header** — `PageShell` title "Overview", sub = range label (`Sep 5–11 · 7 complete days · Asia/Ho_Chi_Minh`, or `Today so far · partial day, no comparison`). Actions: platform `Segment` (All platforms / Web / iOS / Android / Server / Unknown — always visible) and range `Segment` (Today / 7 days / 30 days). Both wrapped in the existing `TARGET_44` hit-area contract.
2. **Freshness line** — `StatusPill` with word + dot: `Data fresh · last event received 2 min ago`, or `Quiet — nothing received for 3 days`.
3. **Headline `StatsStrip`** — Active people, New people, Sessions, Activation, Revenue. Each tile renders `metricTile()` output: `ok` → value + delta; `no_data` → "No data"; `unconfigured` → "Set up" + reason; never a fabricated zero.
4. **Definitions disclosure** — existing `<details>` "How these numbers are computed" listing each metric's `definition` + `notes`.
5. **Trend + Retention grid** — `Chart` (area) with textual equivalent; Retention panel with D1/D7/D30 lines and cohort-window note.
6. **Top pages / Top sources** — two `BarRows` panels, units declared, "Direct / unknown" explicit.
7. **Best next step** — `Panel` rendering the top open `AgentRecommendation` (Plans contract): title, one-line observation with comparison, evidence line via `evidenceLine()` (query ref, metric version, range, timezone, watermark, warnings), and two actions: "Open the finding" → `/plans`, "Ask your agent to investigate" → `/chat` or MCP handoff. Absent findings → panel omitted, not stubbed.
8. **Data status** — existing panel: events in range, qualifying count, last occurred/received, pipeline lag ("not measured yet" until a watermark exists), per-source rows with `StatusPill` states.

### Metric groups (AgentRay data only)

The template's scan pattern is kept: three named groups of concise KPI tiles below the headline strip, each a `Panel` with a `See more` action. The groups are named and defined for AgentRay's own verified event data. **No tile is relabeled as a store metric** — no downloads, impressions, proceeds, store conversion, App Clip or benchmark figure appears anywhere in this product.

- **Acquisition** — New people, Top acquisition sources, Top landing pages, Activation rate. All derived from project-scoped events; "Direct / unknown" is shown explicitly, never dropped.
- **Monetization** — Instrumented revenue, Paying people, Purchases, Purchase→repeat. Every tile requires a trusted billing source. With none connected the group renders `Set up` per tile with the required instrumentation named — never zero, never a sample figure, never a projection.
- **Usage** — Active people, Sessions, D1/D7/D30 retention, Top actions. Retention points carry cohort-maturity semantics; an immature cohort renders `Not ready` rather than a partial number.

A destination whose page is not yet implemented renders as a non-linked "Coming soon" affordance — never a link to a route that does not exist.

**Provenance contract (hard requirement):** every tile carries a provenance line naming the metric version, the range, the project timezone, the coverage and the freshness — the same `evidenceLine()` contract the Best next step panel uses. A metric with no verified source renders its state label (`no_data` / `not_ready` / `unconfigured`); it is never derived, estimated, sampled or zero-filled from unrelated events. There is exactly one day-boundary convention in this product: the project timezone.

### Navigation

`web/lib/ia.ts` gains the grouped child surfaces under Analytics: **Acquisition**, **Monetization**, **Usage**. Each is represented in the IA; a destination whose page is not implemented renders as a non-linked "Coming soon" affordance rather than a dead link. All existing URLs and aliases stay reachable. No nav item is conditional on an external source connection — nothing in this product depends on a store connector.

## States

One mutually exclusive view state, extending `overviewViewState()`:

| State | Trigger | Render |
|---|---|---|
| `loading` | query in flight | layout retained, `Loading` placeholders |
| `no_access` | 403 | `Callout` naming the missing role |
| `error` | other failure | `Callout` + Retry; data status panel notes unavailability |
| `first_run` | never received / verification-only catalog | `FirstEventQuickstart` + data status |
| `filtered_empty` | events exist, filter excludes all | `EmptyState` + reset action + data status |
| `receipt_only` | events arrived, none qualifying | stats strip states + "no qualifying activity" trend panel |
| `empty` | no events in range | honest empty, data status visible |
| `data` | qualifying events present | full layout |
| `stale` (modifier) | `data_status.state == 'quiet'` | warn `Callout` + last-known figures timestamped "As of …" |
| `grouped_metrics` (modifier) | qualifying events present | the three AgentRay groups render under the headline strip |
| `monetization_uninstrumented` (modifier) | no trusted billing source | Monetization group renders `Set up` per tile with the required instrumentation named; never zero |

Stale and the group modifiers are modifiers, not replacements: last-known data stays visible with its timestamp; an uninstrumented group shows its state labels rather than disappearing.

## Responsive and accessibility

- Desktop: sidebar + content grid, max width per `PageShell`; stat strips 5-up (headline) / 3-up (groups) / 4-up (usage).
- ≤900px: single column, stat strips collapse to 2-up, nav becomes the library mobile navigation, tables scroll inside their panel.
- All interactive targets ≥44px (`TARGET_44` wrapper); `Segment` used instead of `Selector` (Selector's trigger is keyboard-unreachable — recorded in the existing code comment).
- Status is word + icon/dot, never color alone; trend has a textual equivalent; skip link present; `aria-current` on the active nav item and state; errors announced via `role="status"`/`Callout`.
- **`--faint` must not carry meaningful text.** Measured on the pinned dark ramp, `--faint` (#5C6678) is 3.13:1 on `--surface-1` and 2.95:1 on `--surface-2` — below WCAG AA for normal text. State values ("Not available", "Not ready", "Set up") and the not-connected nav affordance therefore use `--muted-foreground`; `--faint` stays for incidental, non-essential text only. This is a pre-existing token property, not a new token.
- Verified in the prototype at 1440×1000 and 390×844: document width stays 390px, zero sub-44px targets, exactly one primary CTA per state, no JS errors.

## Component mapping

Every element maps to an existing component: `AppShell`, `SideNav`/`SideNavSection`/`SideNavItem`, `PageShell`, `Panel`, `StatsStrip`, `Chart`, `BarRows`, `Callout`, `EmptyState`, `Segment`, `StatusPill`, `Button`, `ContextChips`, `Loading`, `FirstEventQuickstart`, `evidenceLine`.

**NEW:** `MetricGroup` — a `Panel` + `StatsStrip` composition with a `See more` action and a provenance footer line, used by the three AgentRay groups. One clause why: the provenance footer and per-group drill-down are a repeated contract no existing component expresses; it is a composition, not a new primitive.

## Value-first story (visible in every state, not prose)

The requirement is that the value of the product — and of any paid capability — be understandable **in context, on the screen**, in the grouped and first-run states, not only in a populated web view. The rule is one panel, two honest branches:

- **A complete finding exists** → render the **Best next step** panel from the Plans contract: the finding's title, its observation with a comparison, and its evidence line via `evidenceLine()` (query ref, metric version, range, timezone, watermark, warnings). This is the same panel the populated state uses; it appears in the grouped view too, so the grouped state shows a concrete, evidence-backed next action rather than a wall of numbers.
- **No complete finding exists** → render a **capability/value explanation** instead: what the connected data makes possible, stated as capability, with a clearly labeled example (marked "Example — not your data") or a setup action that would produce the first real finding. It never shows a fabricated live number, an upgrade CTA, a price, or an ROI claim.

Both branches are truthful about what is and is not measured. The paid-value story is "your own product data, joined to usage, with evidence-backed next steps" — never an invented return. Monetization tiers are not implemented, so no upgrade flow ships; if a future tier gates a capability, the `Not available` state is where that prompt belongs, and it must name the real capability.

## Data contract

`OverviewResult` is **unchanged** by this spec. The three groups are composed on the client from the existing deterministic Overview, Activity and Plans operations — no new response block, no second query path, no store-shaped field. Every value is an `OverviewMetric`-style state object (`ok | no_data | not_ready | unavailable`) — never a bare number. Retention points carry `eligible`/`mature` semantics like `OverviewRetentionPoint`. Currency is per-field; unlike currencies are never summed.

## Verification

- Prototype: `open docs/redesign/overview-states.html` — state switcher covers populated, grouped, first run, no data, stale, error, and uninstrumented monetization. Verified at 1440×1000 and 390×844.
- Implementation: `cd web && pnpm test && pnpm lint`; `go test ./internal/dataplane/...`; browser journey for the populated, first-run, no-data and uninstrumented-monetization states per the ticket's acceptance criteria.
- No external source is involved, so no live-source verification step exists and none is claimed.
