# Implementation spec: value-first Overview and app analytics

Status: proposed, 2026-09-13. Companion to [strategy.md](strategy.md) and [design.md](design.md). Prototype: [overview-states.html](overview-states.html) — open locally, use the state switcher at the top. Every figure in the prototype is illustrative; production renders a metric's state label, never a number without a verified source.

## User, job, entry point

- **User:** builder or small product team operating a web and/or app product.
- **Primary job:** understand whether people arrive, activate and return — and see the single most useful next investigation — before configuring anything.
- **Entry point:** `/overview` is the front door (already shipped). This spec extends it with an app view, platform-conditional navigation, and a Plans-backed "best next step" panel.

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

### App view (platform = iOS/Android with an Apple source)

When the project has a connected App Store Connect source and the platform filter selects the app, the overview gains three grouped sections between the stats strip and data status. Each group is a `Panel` with a `See more` action drilling into the matching Analytics subsection.

- **Acquisition** — First-time downloads, Redownloads, Conversion rate, Impressions/day (daily average), Product page views, Updates. **Conversion rate adopts Apple's versioned definition** ([Apple metric definitions](https://developer.apple.com/help/app-store-connect-analytics/reference/metrics-definitions/)): (total downloads — first-time downloads + redownloads — plus pre-orders) ÷ unique-device impressions. A pre-order is **not** counted a second time when it later converts to a download. The tile is `unavailable`/`not_ready` until the source exposes both numerator fields and the unique-device-impression denominator with its eligibility window; it is never computed from product page views.
- **Monetization** — Proceeds (currency-labeled: **estimated customer price less applicable tax and Apple's commission**; refunds are reported separately and are not netted into the figure), Paying users, In-app purchases/day, Download→paid D1/D7/D35 (immature cohorts → "Not ready").
- **App usage** — Average retention D1/D7/D14/D28 (opt-in devices only, labeled), Crashes by app version (table).

**Provenance contract (hard requirement):** every Apple-sourced tile carries a provenance line — `App Store Connect · UTC days` — and the section opens with a `Callout` stating these are not SDK counts and usage/crash figures cover opt-in devices only. Apple withholds or thresholds low-volume rows and reports usage/crash data only for devices that opted in to sharing, so a tile whose source row is absent or below threshold renders `unavailable` with the qualification named — never zero, never a sample value. AgentRay SDK metrics keep project-timezone labeling; the two day-boundary conventions are never silently mixed. No Apple source → every tile renders `Not available` + `Requires App Store Connect` with a `Connect source` action, and the generic SDK metrics remain usable. Never derive, estimate, or sample-fill an Apple metric from SDK events.

### Navigation

`web/lib/ia.ts` gains a conditional child surface: **App analytics** under Analytics (`/dashboard` aliases), visible only when the project has a connected Apple source; otherwise it renders as a non-linked "Not connected" affordance or is omitted per the hosted/self-hosted rules already in `navItemsFor`. Proposed hierarchy when connected: Acquisition (Sources, Product Pages, In-App Events, App Clip, Campaigns), Monetization (Sales, Subscriptions, Cohorts, Offers, Retention, Benchmarks), App Usage. Destinations with no connected source are not linked. All existing URLs and aliases stay reachable.

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
| `source_not_connected` (modifier) | app view without Apple source | Apple groups render `Not available` tiles + `Connect source`; SDK metrics unaffected |

Stale and source-not-connected are modifiers, not replacements: last-known data stays visible with its timestamp; gated groups show state labels.

## Responsive and accessibility

- Desktop: sidebar + content grid, max width per `PageShell`; stat strips 5-up (generic) / 3-up (Apple groups) / 4-up (usage).
- ≤900px: single column, stat strips collapse to 2-up, nav becomes the library mobile navigation, tables scroll inside their panel.
- All interactive targets ≥44px (`TARGET_44` wrapper); `Segment` used instead of `Selector` (Selector's trigger is keyboard-unreachable — recorded in the existing code comment).
- Status is word + icon/dot, never color alone; trend has a textual equivalent; skip link present; `aria-current` on the active nav item and state; errors announced via `role="status"`/`Callout`.
- **`--faint` must not carry meaningful text.** Measured on the pinned dark ramp, `--faint` (#5C6678) is 3.13:1 on `--surface-1` and 2.95:1 on `--surface-2` — below WCAG AA for normal text. State values ("Not available", "Not ready", "Set up") and the not-connected nav affordance therefore use `--muted-foreground`; `--faint` stays for incidental, non-essential text only. This is a pre-existing token property, not a new token.
- Verified in the prototype at 1440×1000 and 390×844: document width stays 390px, zero sub-44px targets, exactly one primary CTA per state, no JS errors.

## Component mapping

Every element maps to an existing component: `AppShell`, `SideNav`/`SideNavSection`/`SideNavItem`, `PageShell`, `Panel`, `StatsStrip`, `Chart`, `BarRows`, `Callout`, `EmptyState`, `Segment`, `StatusPill`, `Button`, `ContextChips`, `Loading`, `FirstEventQuickstart`, `evidenceLine`.

**NEW:** `MetricGroup` — a `Panel` + `StatsStrip` composition with a `See more` action and a provenance footer line, used by the three Apple groups. One clause why: the provenance footer and per-group drill-down are a repeated contract no existing component expresses; it is a composition, not a new primitive.

## Value-first story (visible in every state, not prose)

The requirement is that the value of the product — and of any paid capability — be understandable **in context, on the screen**, in the app-connected and first-run states, not only in a populated web view. The rule is one panel, two honest branches:

- **A complete finding exists** → render the **Best next step** panel from the Plans contract: the finding's title, its observation with a comparison, and its evidence line via `evidenceLine()` (query ref, metric version, range, timezone, watermark, warnings). This is the same panel the populated state uses; it appears in the app view too, so the app-connected state shows a concrete, evidence-backed next action rather than a wall of numbers.
- **No complete finding exists** → render a **capability/value explanation** instead: what the connected data makes possible, stated as capability, with a clearly labeled example (marked "Example — not your data") or a setup action that would produce the first real finding. It never shows a fabricated live number, an upgrade CTA, a price, or an ROI claim.

Both branches are truthful about what is and is not measured. The paid-value story is "your own store data, joined to product usage, with evidence-backed next steps" — never an invented return. Monetization tiers are not implemented, so no upgrade flow ships; if a future tier gates Apple sync, the `Not available` state is where that prompt belongs, and it must name the real capability.

## Data contract additions

`OverviewResult` gains an optional `app` block, present only when an Apple source is connected and the platform filter selects the app:

```
app: {
  source: { kind: 'app_store_connect', connected: true, day_boundary: 'utc', latest_complete_day, opt_in_only: true, thresholded: true },
  acquisition: { first_time_downloads, redownloads, pre_orders, unique_device_impressions, conversion_rate, impressions_daily_avg, product_page_views, updates },
  monetization: { proceeds: {value, currency, basis: 'estimated_customer_price_less_tax_and_commission', refunds_reported_separately: true}, paying_users, iap_daily_avg, download_to_paid: {d1, d7, d35} },
  usage: { retention_avg: {d1, d7, d14, d28}, crashes_by_version: [{version, crashes, devices}] }
}
```

Every field is an `OverviewMetric`-style state object (`ok | no_data | not_ready | unavailable`) — never a bare number. `conversion_rate` is derived only from `(first_time_downloads + redownloads + pre_orders) ÷ unique_device_impressions`, with pre-orders excluded from the download terms once converted; it is `unavailable` until every input is present and eligible. Currency is per-field; unlike currencies are never summed. `download_to_paid` and `retention_avg` points carry `eligible`/`mature` semantics like `OverviewRetentionPoint`. `thresholded`/`opt_in_only` on the source drive the qualification labels on affected tiles.

## Verification

- Prototype: `open docs/redesign/overview-states.html` — state switcher covers populated, app view, first run, no data, stale, error, source-not-connected; verified at 1440×1000 and 390×844.
- Implementation: `cd web && pnpm test && pnpm lint`; `go test ./internal/dataplane/...`; browser journey for web-without-Apple and app-with-Apple per the ticket's acceptance criteria.
- **Live Apple verification requires an authorized App Store Connect app with generated analytics reports.** When that is unavailable, QA runs the fixture-based contract tests plus the browser journey against fixtures, and records live-source verification as a **named limitation** — never an invented PASS. A fixture PASS does not establish live-source correctness.
