# AgentRay redesign proposal

Status: proposed, 2026-09-11. Product and architecture design; storage savings are unmeasured. This proposal supersedes the chat-first product direction only when implemented. Existing production behavior is unchanged.

## Product decision

AgentRay turns product activity and business data into marketing and operational decisions. Its first useful experience is: connect a website or app, see a trustworthy product overview, then use your preferred agent to investigate and improve it.

Assumed initial audience: builders and small product teams who need basic analytics but do not want to operate an analytics engineering stack. This is a working assumption, not customer evidence. Project count, event volume, concurrency, retention and acceptable freshness remain unconfirmed.

The product has three layers:

1. **Data:** capture events and sync business records; maintain identity, schemas, freshness and lineage.
2. **Understanding:** predefined metrics, dashboards, funnels, retention, people and reusable audiences.
3. **Decisions and work:** evidence-backed marketing plans and operational investigations, followed later by execution integrations.

External agents are a first-class way to operate all three layers through MCP. Agent Garden is an optional built-in client of the same capabilities, with lower delivery priority. Basic analytics and setup must work without an LLM provider key or an installed agent.

The initial wedge is **instrument → understand → choose one measurable improvement**. CRM, content production, sales, live chat and general automation expand this later; none is a prerequisite for the first dashboard.

## Experience and navigation

Replace the current Runtime / Channels / Workloads navigation with destinations a product owner can recognize:

| Destination | Job | Existing foundation |
|---|---|---|
| Overview | Understand usage, conversion, retention and data freshness | Default dashboard, activity and product views |
| Analytics | Explore acquisition, engagement, funnels and retention; save dashboards | Traffic, Product, Dashboards, Templates |
| People | Inspect a person's activity and business attributes; save audiences | Persons, identities, cohorts |
| Data | Connect SDKs and sources; inspect events, datasets and pipeline health | SDK setup, Events, connectors, SQL |
| Plans | Keep findings, evidence, proposed experiments and results | Recommendations and growth tests |
| Agents | Connect an external agent; optional Agent Garden subsection | MCP, portable skills, existing runtime |
| Settings | Workspace access, credentials, retention and usage | Existing settings |

Do not label the current scheduler screen as business operations: today `/operations` means agent triggers. Keep its routes reachable under Agent Garden during the transition. Preserve old deep links, saved dashboards and user layouts. Start new projects on Overview; migrate the returning-user default without deleting existing views.

### First session

Create project → choose Web or App → install SDK manually or copy an agent setup task → verify an actual event → open Product Overview → optionally map signup, activation and purchase events → connect a business dataset when needed.

An agent setup task carries the project context, canonical event contract, platform-specific installation instructions and verification steps. The external coding agent edits the user's repository using its own authorized workspace tools; MCP supplies AgentRay configuration and verifies arrival. MCP alone does not edit an arbitrary local repository. Treat Codex, omp and Claude Code as client choices; verify each client's transport/auth support before claiming compatibility.

Installation success means a known test event is visible with correct platform and identity, retry behavior is understood, and test data is excluded from production metrics. Source keys embedded in a browser/app must be capture-only; private management credentials stay with the operator/agent.

### Default Product Overview

Borrow the quick-overview and drill-down *pattern* from a store analytics dashboard — the scan order, the grouped KPI tiles, the See more affordance — while defining AgentRay's own metric semantics from AgentRay's own events. That screen was a layout and information-hierarchy reference only: there is no store integration, no store credential, no store metric and no store provenance anywhere in this product, and no store figure is ever inferred from SDK events. A metric AgentRay cannot verify from its own data stays `Not available` / `Set up` with the required instrumentation named.

| Block | Definition / prerequisite |
|---|---|
| Active users + daily trend | Distinct canonical people with qualifying human product activity; exclude bot, test, internal and server-only background activity |
| New users | First observed qualifying activity across retained history; label as first observed, not signup or download |
| Sessions | One versioned session rule, reusing current SDK/server sessionization; show web/app splits |
| Activation | Users completing the project's chosen activation condition / eligible users in a defined cohort and conversion window |
| Retention | D1/D7/D30 return activity for mature first-activity or signup cohorts; declare which cohort and return condition |
| Top pages/screens and sources | SDK page/screen events and attribution fields; show unknown/direct explicitly |
| Revenue, optional | Trusted server events or billing sync, deduplicated; state gross/net, refunds and currency; never sum unlike currencies |
| Data status | Latest processed event, source-specific watermark, lag and schema failures |

Defaults: last 7 complete days, comparison with the preceding 7, project timezone, all platforms with a visible filter. Offer today as a partial period. A metric includes its definition, range, filters, coverage and last refresh. Cross-device identity requires an explicit identify link; anonymous people remain approximate. Retention immature cohorts show “Not ready”; unmapped activation/revenue shows “Set up”; collection failure shows stale/error, never a misleading zero. Daily distinct-user counts cannot be summed into weekly users.

Overview renders from deterministic queries, with no per-view agent run. Custom dashboards reference versioned metric definitions. Existing boards are preserved; a versioned system template supplies new defaults and optional upgrades.

## Agent capabilities and product contracts

One operation registry supplies web/API, MCP and Garden. Reuse `opcore.Operation → usecase.Repo → storage` and the existing tool names where their contracts fit. Agent expertise lives in skills/config; do not create a bespoke backend marketing agent.

| User task | MCP capability | Completion evidence |
|---|---|---|
| Integrate SDK | Discover project schema and SDK instructions; inspect ingestion and verify a test | Event arrived, platform/identity checked, overview usable |
| Manage dashboards | List/create/update/archive dashboards and charts; preview metric query | Saved artifact ID and reproducible result |
| Manage pipelines | List/test sources, discover tables, preview mapping, create/update/pause/run sync, inspect failures | Validated preview, run status, watermark and retry outcome |
| Analyze → marketing plan | Run metrics/cohorts/funnels/retention, save findings and propose tests | Baseline, evidence query IDs, audience, hypothesis, action, owner, success metric, guardrail and review date |
| Future execution | Publish content, CRM actions, sales tasks, live chat and scheduled workflows | Explicit scope, action history, delivery/outcome and cancellation path |

Existing registry coverage is partial: analytics, dashboard creation, chart creation, recommendations and tests are present; dashboard editing/deletion and connector lifecycle management are not registered there. SDK setup skills already exist. Make the missing capabilities operations, not MCP-only handlers. See [registry](../../internal/dataplane/usecase/analytics.go), [MCP adapter](../../internal/app/mcp_routes.go), and [governance](../AGENT-GOVERNANCE.md).

Proposed operation response envelope: `result + evidence {query_id, metric_version, dataset_version, range, filters, watermark, warnings} + artifact_id when saved`; bounded expensive work returns `job_id`, status and cancellation. Project/workspace scope is resolved by authenticated server context, never trusted from arbitrary model arguments. Updates carry revision checks and retryable writes use idempotency keys.

Separate capture keys from private read/manage credentials before expanding external-agent writes. The current MCP adapter reuses the project-key resolver; audit the complete grant model and existing deployments, then migrate deliberately. Credential scopes should distinguish analytics reads, dashboard writes, source management and external execution, with revocation and an audit trail. Confirmation is for impactful actions, not routine authorized dashboard edits. Secrets should be entered through a credential flow and referenced by ID.

A marketing plan records **observation → hypothesis → proposed experiment → measured outcome**. Agents must distinguish correlation from a tested causal claim and cite the actual data window. Operational v1 focuses on tracking quality, stale syncs and observed business failures; it does not claim full infrastructure monitoring.

## Data model

Keep events and synced records distinct. An order row is current business state; an `order_paid` event is a fact at a point in time. Joining both without a grain/identity contract creates double counts.

- **Events:** workspace/project/source, stable event ID, event name/version, occurred/received timestamps, anonymous and identified IDs, session, platform, properties, environment/consent context.
- **Entities:** source + dataset + primary key, source version/cursor, loaded time, deleted marker and attributes. Declare current-state versus historical/as-of semantics; current plan status must not silently rewrite historical segments.
- **Identity links:** project-scoped canonical mapping with explicit merges and erasure behavior; preserve the existing stitching semantics during migration.
- **Metrics/audiences:** grain, qualifying events, identity/session rules, dimensions, time semantics, definition version and source dependencies.
- **Sources/pipelines:** credentials reference, schema, mapping/version, schedule, cursor, checkpoints, run history and freshness expectations.
- **Plans:** findings, evidence, experiments and outcomes stored independently of chat transcripts so external and built-in agents can continue the same work.

Start with the existing PostgreSQL connector. Support bounded snapshot and incremental sync, stable cursor+primary-key tie-breaking, retry-safe writes and an explicit deletion policy. A timestamp poll cannot discover hard deletes on its own: require a tombstone feed or periodic full reconciliation. Do not advertise CDC until it exists. Preview schema drift and recompute affected metrics deliberately.

## Storage recommendation

**Evaluate DuckDB as the preferred small-deployment candidate; do not declare it better or migrate production until the workload test passes.** Keep PostgreSQL for transactional metadata. Retain ClickHouse as the migration fallback, not an indefinite promise to maintain every feature on two engines.

Evidence from this checkout: `infra/gce/infra/docker-compose.yml` pins ClickHouse 24.12, caps it at 2 GB and shares it across dev/prod; Redis is capped at 128 MB and NATS at 256 MB. The ingest code already batches and supports durable JetStream acknowledgements. `store.go` contains ClickHouse-specific identity dictionaries, materialized views, rollups, `windowFunnel`, deduplication and SQL guards. This is a substantial adapter migration, not a driver substitution. These are configuration/code observations, not measurements of the running deployment.

| Concern | DuckDB candidate | ClickHouse |
|---|---|---|
| Small deployment cost | Embedded execution may reduce standing overhead; measure whole service RSS/CPU | Existing deployment and queries; measure idle cost, merges and query bursts separately |
| Interactive concurrency | Bound admission, cache dashboard responses, queue large agent queries | Existing server workload controls and parallel serving |
| Writes | Start with one owning process per native database; batch ingestion | Existing JetStream consumer and batch writer |
| Sync + event joins | Natural analytical SQL; profile join cardinality and memory | Existing landing table and identity dictionary behavior |
| Migration burden | Rewrite dialect, funnel/retention, identity, rollups and SQL isolation | No storage rewrite needed |

DuckDB's native in-process read/write database is owned by one process, which can have concurrent threads. Current docs also describe Quack remote access and DuckLake with a PostgreSQL catalog for multi-process designs; those are separate architecture choices, not permission to share an ordinary writable file between replicas. Start with the simpler ownership model for the evaluation. [DuckDB concurrency](https://duckdb.org/docs/current/connect/concurrency).

DuckDB can spill analytical work to disk, but some complex queries still exhaust memory, and many tiny concurrent queries are not its primary target. Therefore lower idle footprint does not establish safe dashboard serving under agent load. [DuckDB workload tuning](https://duckdb.org/docs/current/guides/performance/how_to_tune_workloads). ClickHouse likewise needs measured concurrency, memory budgets and sensible insert batches. [ClickHouse serving guidance](https://clickhouse.com/resources/engineering/high-concurrency-sizing-user-analytics).

Candidate flow: SDK capture → durable JetStream → validated/idempotent batch writer → DuckDB analytics store → shared metric/query service → web and MCP. PostgreSQL owns control-plane state. A trusted Go analytics worker owns the writable file and services bounded reads; other API processes call it rather than open that file. Bound the number of active databases/workers so tenant count cannot multiply memory without limit. Keep deployed queue/rate-limiter changes outside the DB experiment.

Use immutable Parquet exports for backup/replay and later cold history if justified; avoid building a homegrown lakehouse/catalog in v1. Define a consistent backup checkpoint and event high-water mark. Restore a database snapshot then replay subsequent durable events; acknowledge writes only after committed persistence. Erasure must cover hot data, exported data, derived results and documented backup expiry. If multi-writer scale is required, evaluate DuckLake or retain ClickHouse rather than adding distributed locking around a file.

Advanced SQL is a migration gate: DuckDB can read files/network and load extensions. A SELECT-only regex or read-only connection is not a tenant boundary. Use structured metric operations by default. Preserve advanced SQL through a project-isolated, resource-bounded execution environment with only authorized data mounted and no general credentials/network, plus locked-down extensions/configuration. Do not run arbitrary agent SQL inside the trusted writer process. [DuckDB security model](https://duckdb.org/docs/current/operations_manual/securing_duckdb/overview).

### Decision experiment and rollback

Capture baseline idle/peak RSS, CPU, query latency/concurrency, data/parts size, insert batch sizes, queue lag, disk growth and errors; separate dev/prod and system logs. Compare a reasonably tuned current ClickHouse baseline with a pinned DuckDB release on identical hardware and the same scrubbed workload. Include Go/driver build, restart and disk compatibility checks.

Test current data volume and 3× that volume; if no snapshot is available use explicitly synthetic 1M/10M-event fixtures with realistic JSON width, identity links, late events and entity joins. Exercise 1/5/20 simultaneous queries alongside ingest and sync, seven/30/90-day queries, first cold load, warmed dashboards, expensive funnels and retention. Synthetic results cannot establish production capacity.

Provisional acceptance targets, to adjust to the owner's workload: at least 30% lower total deployment memory at idle and representative load; whole overview p95 under 2 seconds warm / 5 seconds cold at target concurrency; 95% of accepted events queryable within 60 seconds; no OOM and no acknowledged-event loss in crash/retry tests. Measure total cost including disk, object requests, backups, query workers and operational effort. These are proposed gates, not measured results or DuckDB guarantees.

Correctness gates: exact fixture parity for identities, sessionization, deduplication, late events, ordered funnel/window rules, mature retention cohorts, entity updates/deletes and currency; explain tolerances only where current metrics are explicitly approximate. Include cross-project and SQL isolation tests, backup restore, replay after commit-before-ack failure, and query cancellation.

Rollout: extract an analytics-engine boundary behind existing usecases → backfill DuckDB at a recorded watermark → feed both stores through independent durable consumers → compare shadow reads → switch one project → expand. Keep ClickHouse ingestion caught up throughout the rollback window so reverting reads is immediate. Inventory and translate saved SQL/charts explicitly; unsupported queries block that project's switch. Retire old storage only after parity, restore testing, stable operation and completion of the rollback window.

## Delivery sequence (L initiative, independently verifiable slices)

1. **Product contract and overview:** validate this concept, lock metric semantics, reuse the current store to ship overview/onboarding and source status. Verify first-event flow, empty/stale states, identity counts, responsive layout and existing deep links.
2. **MCP access and lifecycle parity:** separate capture/management access, add missing dashboard/source operations, SDK verification and asynchronous job contracts. Verify external-client workflows, revocation, project isolation and retry/concurrent-edit behavior.
3. **Storage decision:** instrument and benchmark the current deployment and isolated DuckDB candidate. Deliver reproducible results and a go/no-go decision, independent of UI completion. If accepted, execute the parity/backfill/canary migration above as a separate ticket.
4. **Business data and plans:** make the PostgreSQL sync understandable, expose stable datasets/joins and save evidence-backed marketing experiments. Verify retry/update/delete semantics and that a plan can be resumed from another agent without chat history.
5. **Later:** Agent Garden polish and optional scheduled execution, then demand-led content/CRM/sales/live-chat integrations. Reuse the same operation contracts and domain artifacts.

Product validation: observe whether a builder can install the SDK, explain the overview, and ask an external agent for a useful next step without assistance. Track first-event-to-overview conversion, time to first useful dashboard, weekly returning projects and experiments reviewed with a measured outcome. Set targets after observing a baseline; no customer or adoption proof has been gathered here.

## Open decisions

- Audience, workload and freshness budget: awaiting user context; do not select engine capacity from event count alone.
- Derived, identity/metric parity: [storage implementation](../../internal/dataplane/store/store.go) and commit `063c947` contain session/platform behaviors that must survive the migration.
- Derived, permission migration: [MCP routes](../../internal/app/mcp_routes.go) reuse project keys; audit existing key distribution and grant behavior before tightening access.
- Derived, historical compatibility: [SQL operation](../../internal/dataplane/usecase/analytics.go) explicitly promises ClickHouse dialect; saved SQL needs inventory and per-query disposition.
- Derived, product direction: [current DESIGN.md](../../DESIGN.md) requires chat-first and architecture-layer navigation; this proposal deliberately changes those rules while keeping its component/token system.

See [execution plan](plan.md), [UI concept specification](design.md), and the [interactive architecture diagram](architecture.html) ([source](architecture.json), [delivery receipt](architecture.receipt.json)).
