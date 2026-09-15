# Plan
**Goal:** Make AgentRay a data-to-decisions product: SDK/sync → useful default analytics → external-agent workflows → marketing and operations.
**Scope:** L initiative, decomposed into the independently verifiable slices in [strategy.md](strategy.md#delivery-sequence-l-initiative-independently-verifiable-slices). As-built index: [as-built.md](as-built.md).
**Out of scope:** Production migration, broad CRM/content/live-chat execution, and Agent Garden expansion in the first release.
**Approach:** Overview is home; reuse Astryx, PageShell, StatsStrip, Chart, existing dashboards/SDKs/connectors and the opcore/usecase boundary.
Keep events and versioned synced entities distinct; central metric definitions supply web, MCP and Garden with shared evidence/freshness metadata.
Keep PostgreSQL metadata and durable ingest. **Superseded 2026-09-13:** "benchmark a bounded DuckDB worker against ClickHouse before any engine switch" — DuckDB is the shipped store (`strategy.md` Decision block); no ClickHouse engine remains to benchmark against (003 `bs-imvxkfav`).
**Unknowns:**
- Audience/workload/freshness unconfirmed; [strategy.md](strategy.md#decision-experiment-and-rollback) defines provisional benchmark gates.
- Derived: identity/session/funnel and saved-SQL parity depend on [store.go](../../internal/dataplane/store/store.go) and [analytics.go](../../internal/dataplane/usecase/analytics.go).
- Derived: capture/management credential separation needs migration from [mcp_routes.go](../../internal/app/mcp_routes.go).
**Verify:** `git diff --check`; open `docs/redesign/concept.html` at desktop/mobile widths and inspect navigation, first-run/error states and keyboard access.
**Implementation verification:** `go test ./internal/dataplane/... ./internal/shared/opcore/... ./internal/app/...`; `cd web && pnpm test && pnpm lint`.
**Design:** [design.md](design.md); standalone [concept.html](concept.html) remains a disposable prototype. Production UI shipped in 001/002/008.
