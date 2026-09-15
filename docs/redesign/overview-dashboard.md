# Implementation spec: value-first Overview

Status: as-built 2026-09-15 (see [as-built.md](as-built.md)). Companion to [strategy.md](strategy.md) and [design.md](design.md). Prototype: [overview-states.html](overview-states.html) — sample data only. Production renders a metric's state label, never a number without a verified source.

> **Scope correction (2026-09-13) — superseded 2026-09-14 by tickets 007 `bs-nrwjyctw` and 008 `bs-y892eezp`.** The App Store Connect screen remains a layout/hierarchy reference only: there is still no store connector, credential, report ingest, Apple API, Apple provenance, Apple-only route, or iOS-only gate. **What the user then overrode is naming.** Tile titles on the analysis boards follow App Store Connect labels verbatim (First-time downloads, Proceeds, Active devices, …). Values stay AgentRay catalog metrics. The honesty rule did **not** fall: an unsourced or immature tile renders `Set up` / `Not ready` / `Not available` — never a fabricated, sampled or zero-filled number.

> **Data contract (this document, 2026-09-13) — superseded 2026-09-14 by ticket 007.** "`OverviewResult` is unchanged … no new response block, no second query path" is replaced by the declarative board model in [DESIGN-BOARD-CONTENT-MODEL.md](../DESIGN-BOARD-CONTENT-MODEL.md). Overview still reads the deterministic Overview operation. Acquisition / Monetization / Usage are `get_board` documents addressed by `board_key`; tiles name catalog metrics computed by `read_metric`.

## User, job, entry point

- **User:** builder or small product team operating a web and/or app product.
- **Primary job:** understand whether people arrive, activate and return — and see the single most useful next investigation — before configuring anything.
- **Entry point:** `/overview` is the front door (already shipped). This spec extends it with grouped metric panels, platform-conditional navigation, and a Plans-backed "best next step" panel.

## Layout and controls

Compose from existing components only: `AppShell`/`SideNav`, `PageShell`, `Panel`, `StatsStrip`, `Chart`, `BarRows`, `Callout`, `EmptyState`, `Segment`, `StatusPill`, `Button`, `ContextChips`. No new tokens, no new UI library.

1. **Header** — `PageShell` title "Overview", sub = range label (`Sep 5–11 · 7 complete days · Asia/Ho_Chi_Minh`, or `Today so far · partial day, no comparison`). Actions: platform `Segment` (All platforms / Web / iOS / Android / Server / Unknown — always visible) and range `Segment` (Today / 7 days / 30 days). Both wrapped in the existing `TARGET_44` hit-area contract.
2. **Freshness line** — `StatusPill` with word + dot and an **absolute** receipt stamp (`Data fresh · Last received 2026-09-12 11:59 UTC`). Relative copy ("2 min ago") is illustration only; production keeps an absolute stamp so an untouched tab cannot go stale. Quiet uses the same stamp plus the word Quiet.
3. **Headline `StatsStrip`** — Active people, Sessions, New people (001 shipped this three-tile strip; Activation and Revenue live on the analysis boards, not duplicated here). Each tile renders `metricTile()` output: `ok` → value + delta; `no_data` → "No data"; `unconfigured` → "Set up" + reason; never a fabricated zero.
4. **Definitions disclosure** — existing `<details>` "How these numbers are computed" listing each metric's `definition` + `notes`.
5. **Trend + Retention grid** — `Chart` (area) with textual equivalent; Retention panel with D1/D7/D30 lines and cohort-window note.
6. **Top pages / Top sources** — two `BarRows` panels, units declared, "Direct / unknown" explicit.
7. **Best next step** — `Panel` rendering the top open `AgentRecommendation` (Plans contract): title, one-line observation with comparison, evidence line via `evidenceLine()`, and two actions: "Open the finding" → `/plans`, "Ask your agent to investigate" → `/chat` (Agents alias) or MCP handoff. **No complete finding** → do not stub the finding branch; render the capability/value explanation instead (001 F3/F10). Never sample advice, price, paywall or ROI.
8. **Data status** — existing panel: events in range, qualifying count, last occurred/received, pipeline lag ("not measured yet" until a watermark exists), per-source rows with `StatusPill` states.

### Metric groups

The template's scan pattern is kept: three named groups, each a `Panel` with a `See more` action to a shipped destination. **Superseded 2026-09-14 (008):** the 2026-09-13 clause "No tile is relabeled as a store metric — no downloads, impressions, proceeds…" is naming-only override, not a licence to invent data. Seeded boards (`internal/dataplane/store/default_boards.go`) plus unserved labels (`web/lib/analysis.ts`):

- **Acquisition** (`/acquisition`, `board_key=acquisition`) — First-time downloads (`new_users`), Top pages, Top acquisition sources. Unserved (named empty, never zero): Redownloads, Conversion rate, Impressions / day, Product page views, Updates.
- **Monetization** (`/monetization`, `board_key=monetization`) — Proceeds (`revenue`, trusted billing, deduplicated net). Unserved: Paying users, In-app purchases / day, Download→paid D1/D7/D35.
- **Usage** (`/usage`, `board_key=usage`) — Active devices (`active_users`), Sessions, Average retention D1/D7/D30. Unserved: Average retention D14, Crashes by app version. Immature cohorts render `Not ready`, never a partial number.

Overview **See more** opens those destinations. Direct / unknown sources stay visible.

**Provenance contract (hard requirement):** every tile carries a provenance line naming the metric version, the range, the project timezone, the coverage and the freshness — the same `evidenceLine()` contract the Best next step panel uses. A metric with no verified source renders its state label (`no_data` / `not_ready` / `unconfigured`); it is never derived, estimated, sampled or zero-filled from unrelated events. There is exactly one day-boundary convention in this product: the project timezone.

### Navigation

`web/lib/ia.ts` lists **Acquisition**, **Monetization**, **Usage** as linked child surfaces under Analytics. All existing URLs and aliases stay reachable. No nav item is conditional on an external source connection — nothing in this product depends on a store connector.

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

- Desktop: sidebar + content grid, max width per `PageShell`; `StatsStrip` is a shared auto-fit grid (`AutoGrid min={140}`), not a fixed 5/3/4 column count.
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

Overview itself still serves `OverviewResult`. The three analysis destinations are **not** composed only from that envelope: they are 007 board documents (`get_board`) whose metric tiles call `read_metric` against the catalog. Every value is a state object (`ok | no_data | not_ready | unconfigured | unavailable`) — never a bare number. `unavailable` is the client's fallback for an unserved tile; the Overview Go path emits it for pipeline lag / schema status, not for KPI tiles. Retention points carry `eligible`/`mature` semantics. Currency is per-field; unlike currencies are never summed.

## Verification

- Prototype: `open docs/redesign/overview-states.html` — state switcher covers populated, grouped, first run, no data, stale, error, and uninstrumented monetization. Verified at 1440×1000 and 390×844.
- Implementation: `cd web && pnpm test && pnpm lint`; `go test ./internal/dataplane/...`; browser journey for the populated, first-run, no-data and uninstrumented-monetization states per the ticket's acceptance criteria.
- No external source is involved, so no live-source verification step exists and none is claimed.
