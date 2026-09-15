# AgentRay data-only default dashboard

Status: as-built 2026-09-15. Prototype [agentray-dashboard-states.html](agentray-dashboard-states.html) is sample data. Production Overview shipped in 001; analysis destinations in 008. Index: [as-built.md](as-built.md).

## Job and composition

- **User / entry:** a project owner opens `/overview`. Drill-downs: `/dashboard`, `/events`, `/plans`, `/agents`, plus shipped `/acquisition`, `/monetization`, `/usage` (008). Investigation handoff on Overview is `/chat` (Agents alias; 001 D8).
- **Layout:** retain `AppShell` / `PageShell`, range and platform `Segment`, freshness `StatusPill`, `StatsStrip`, `Panel`, `Chart`, `BarRows`, `Callout`, `EmptyState`, `Button`, and `FirstEventQuickstart`. Shipped order (001 F8): headline → trend / retention → groups → Plans next step → data status.
- **NEW: `MetricGroup`:** a local `Panel` + `StatsStrip` composition. See more links to the shipped analysis destinations.
- **Responsive / access:** the existing shell becomes its library mobile navigation below 900px; every grid becomes one column, every control remains at least 44px, chart data has a textual equivalent, status uses text plus dot, and semantic state labels use `--muted-foreground` rather than low-contrast `--faint`.

## Default-tile contract

The selected period is `Overview.context.range`: `7d` defaults to seven **complete** project-local calendar days; `today` is an explicit partial period and carries no comparison. Every Overview metric is project-scoped and filtered by the selected platform. `context.timezone` is the display/day boundary (or visible UTC fallback); `context.generated_at` plus `data_status.last_received_at` define freshness. `no_data`, `unconfigured`, `not_ready`, and `unavailable` have no numeric value and must render their named state, never `0`.

| Group / tile | Real source and definition | Unit / period / freshness | Empty, immature, or unsupported state |
|---|---|---|---|
| Overview — Active people | `Overview.metrics.active_users`: distinct stitched canonical identities with qualifying `user` / human activity, excluding onboarding verification, bots, and server-only background activity. | People; selected Overview range, platform, and project timezone; receipt freshness from `data_status.last_received_at`. | `no_data` when nothing usable is in range; no zero.
| Overview — Sessions | `Overview.metrics.sessions`: distinct nonempty session IDs on qualifying activity; server sessionizer ends after 30 minutes idle. | Sessions; selected range/platform/timezone; same receipt freshness. | `no_data`; no zero.
| Overview — New people | `Overview.metrics.new_users`: first-ever qualifying activity inside the range, explicitly **not** signup or download. | People; selected range and first-event platform, project timezone; same receipt freshness. | `no_data`; no zero.
| Trend — Active people per day | `Overview.trend`: daily distinct active people. | People/day; every local day in the selected range, including measured zero days; same freshness. | Empty / receipt-only explanation; daily values are never summed into the period distinct count.
| Acquisition — Top sources | `Overview.content.top_sources`: `user.pageview` events grouped by `referrer_channel`, with missing channel as `unknown`. | Pageviews; selected range/platform/timezone; same freshness. | `BarRows` says no attributed sources; direct/unknown stays present, never silently dropped.
| Acquisition — Top pages / screens | `Overview.content.top_pages`: project-scoped `user.pageview` property count for `path`. | Pageviews; selected range/platform/timezone; same freshness. | `BarRows` says no pageviews. Screen names require a verified pageview/path event shape; no synthetic app screen metric.
| Monetization — Instrumented revenue | `Overview.metrics.revenue` is the only current revenue tile. Its definition requires a trusted, deduplicated server or billing source with declared gross/net basis and currency. Analysis boards title this tile **Proceeds**. | Currency only when the metric supplies one; selected range/platform/timezone; source receipt freshness. | Current state is `unconfigured` → **Set up**, with that requirement. The money grid *is* deduplicated at read time (`internal/dataplane/store/money.go`); the 2026-09-13 "SDK revenue events are not deduplicated" sentence is stale (001 D1). |
| Monetization — Purchases | No verified `Overview` / `Activity` / `Plans` purchase metric exists. | Not applicable. | **Not available**. Required future instrumentation is a versioned, deduplicated purchase event/metric contract; do not derive it from generic event volume.
| Monetization — Subscriptions | No verified project-scoped subscription lifecycle or current-state metric exists. | Not applicable. | **Not available**. Required future instrumentation is an explicit subscription state/source contract; do not render a count, MRR, or conversion proxy.
| Usage — Activation | `Overview.metrics.activation` defines a cohort completing the project-selected activation event inside a conversion window. | Percent plus eligible/converted counts only after a stored condition exists; selected range/platform/timezone; same freshness. | Current `unconfigured` → **Set up**: no activation condition is stored today. State copy names that prerequisite; it must not imply an existing configuration form.
| Usage — D1 / D7 / D30 retention | `Overview.retention.{d1,d7,d30}`: returned/eligible for lifetime first-activity cohorts, with maturity calculated per cohort day. | Percent and returned / eligible people; return days use project timezone and selected platform; same freshness. | `not_ready` if no eligible mature cohort. A measured zero rate remains 0%, not “Not ready.”
| Usage — Crashes | No verified crash event/version aggregate exists in `Overview`, `ActivitySummary`, or the SDK event contract. | Not applicable. | **Not available**. Required future instrumentation is a verified crash event plus normalized app-version field and a project-scoped aggregate; never infer it from errors.
| Plans — Best next step | Existing `list_findings` / `AgentRecommendation`: pick the first **open**, highest-impact finding with a parseable evidence envelope. Render title, rationale, and existing `evidenceLine()`. | Textual finding; its evidence envelope carries query ref, metric/dataset versions, range, filters, timezone, watermark, and warnings. | Omit the *finding* branch when none exists (never stub with sample advice). 001 restores a labeled capability explanation instead ("Example — not your data"). No price, paywall, or ROI. |
| Data status | `Overview.data_status`: event/qualifying counts, last occurred/received, capture state, and persisted connector-source statuses. `ActivitySummary` remains the real `/events` drill-down for activity/event detail; it is not a second competing overview aggregate. | Events / source rows; range and project scope; `last_received_at` is capture freshness. | Pipeline lag and schema health say “not measured yet” while their contract is `unavailable`; quiet means capture age, not inferred processing lag.

## State and action rules

- **First run:** `FirstEventQuickstart`; metric groups do not pretend a verification receipt is product usage. Real actions: `/events` for setup/data inspection and `/agents` for an agent setup task.
- **Filtered empty / receipt-only / no data:** reuse the existing mutually exclusive `overviewViewState()` states. Show a reset only for the filtered case; receipt-only explains that non-qualifying events arrived; empty reports no measurement rather than zeros.
- **Stale:** a `quiet` data-status modifier retains last-known values with their actual receipt timestamp and a capture warning. It does not relabel stale data as current or call quiet “pipeline lag.”
- **Error / no access:** existing `Callout` semantics and retry / missing-role copy apply. No status is guessed while the overview fails.
- **Links:** `Explore activity` → `/dashboard`; data inspection → `/events`; full findings → `/plans`; investigation handoff → `/chat`. Acquisition / Monetization / Usage are shipped 008 destinations; See more points at them. Unserved App Store Connect labels render `Not available`, never a zero.

## Scope boundary

This file was the 2026-09-13 data-only tile contract. 007/008 superseded its "no analysis routes" and "do not relabel as a store metric" clauses (naming only). Honesty (`Set up` / `Not ready` / `Not available`, never fabricated numbers) still binds.
