# As-built: `docs/redesign/` after 001–010

Status: as-built 2026-09-15. Subject: this worktree (`a3d2787`), which contains the landed code tickets. This file is the index of what shipped. Corrections to the older proposal files are traced in the ticket evidence; they do not rewrite intent.

Sibling audits: 001 `bs-5agfp7jr`, 002 `bs-196doi84`, 003 `bs-imvxkfav` (audit), 004 `bs-vc198hvf` (audit), 006 `bs-mt0e195m`, 007 `bs-nrwjyctw`, 008 `bs-y892eezp`, 009 `bs-sw1606kj`, 010 `bs-hv4s2nlr`.

## What the product is

Overview is the signed-in front door (`/overview`). Navigation is owner tasks: Overview · Analytics · People · Data · Plans · Agents · Settings (`web/lib/ia.ts`). Acquisition, Monetization and Usage are linked Analytics children at `/acquisition`, `/monetization`, `/usage` — declared boards (`board_key`), not Coming soon.

Values come from AgentRay events and the metric catalog. Tile **titles** on the three analysis boards follow App Store Connect analytics labels (user decision 2026-09-14). That is naming only: there is no store connector, credential, Apple API, or Apple provenance. An unsourced or immature tile renders `Set up` / `Not ready` / `Not available`, never a fabricated, sampled or zero-filled number.

Board content is a declarative document on the dashboard row. Contract: [DESIGN-BOARD-CONTENT-MODEL.md](../DESIGN-BOARD-CONTENT-MODEL.md) (007 chose that path over `docs/redesign/dashboard-content-model.md`). Operations: `list_metrics`, `read_metric`, `get_board`, `save_board`.

## Authorization (010)

**Superseded 2026-09-15 (`bs-hv4s2nlr`).** There is no `viewer` role. Workspace roles are `owner` / `admin` / `member`. Members hold access classes. Demo non-owners receive the read grant set (`analytics:read` + `sources:read`) because `sessionGrants(project)` sees `IsDemo` on the project (`internal/app/principal.go`). The mutating floor asks `opcore.Allow`; it does not branch on whether a demo is configured. `demoWriteGuard` is a second *invocation* of Allow, not a second rule.

## Registry (004 F4)

One registry at `usecase.Registry()` (`internal/dataplane/usecase/analytics.go`). **47** `opcore.Register` calls. Dashboard lifecycle (`update_dashboard` … `reorder_charts`) and connector lifecycle (`test_source` … `cancel_source_run`) **are** registered, plus the 007 board operations. The 2026-09-11 sentence that those were missing is stale.

## Storage (003)

DuckDB is the analytics store (shipped 2026-09-13, `d9cc85a` / `c881f6e`). PostgreSQL is control-plane metadata. JetStream is durable ingest. There is no ClickHouse engine left to benchmark against. `strategy.md` keeps the pre-port proposal below an explicit historical fence.

## Overview contract (001)

Headline strip: Active people, Sessions, New people. Trend and retention sit above the three groups. Freshness is an absolute receipt stamp plus a word+dot pill. Best-next-step uses a real finding or a labeled capability explanation; it does not invent advice. Investigation handoff on Overview is `/chat` (alias of Agents).

## Prototypes

`concept.html`, `overview-states.html` and `agentray-dashboard-states.html` remain disposable design artifacts with sample data. They are not the product.

`architecture.json` / `architecture.html` are regenerated from the as-built storage claim (DuckDB shipped). Visual-check sidecars were re-collected after that deliver.
