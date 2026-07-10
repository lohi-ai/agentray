# AgentRay Data System Architecture

As-built architecture of the data path — **event capture → data store → analytics
& insight presentation → agent** — followed by an ordered improvement plan.
Companion docs: [ARCHITECT-API.md](ARCHITECT-API.md) (service layout),
[AGENT-GOVERNANCE.md](AGENT-GOVERNANCE.md) (agent data-access wall).

## Part 1 — Current Architecture

```
┌─ CAPTURE ────────────────────────────────────────────────────────────────┐
│ sdk/browser (autocapture clicks+pageviews, BatchTransport, beacon)       │
│ @agentray/server (server-authoritative events, revenue, $insert_id)      │
│ sdk/python · PostHog-compatible HTTP: /capture /batch /e/ /identify      │
│ /alias — project API key auth, Redis sliding-window per-IP rate limit    │
│                                                                          │
│ ingest-time enrichment (ingestion/handler.go): UA → visitor_class/       │
│ bot_name, referrer → host/channel, agent fields (agent_id, tool IO,      │
│ tokens, cost_usd, latency, model, is_error), timestamp normalize         │
└──────────────┬───────────────────────────────────────────────────────────┘
               ▼  json → NATS core pub (subject agentray.events.ingest)
┌─ BUFFER ─────────────────────────────────────────────────────────────────┐
│ EventWorker: ChanQueueSubscribe (buf 1024, queue group) →                │
│ EventBatcher: coalesce to ≤500 rows / 1s flush → store.InsertEvents      │
│ failure mode: logBatchError → batch DROPPED (no retry, no DLQ)           │
└──────────────┬───────────────────────────────────────────────────────────┘
               ▼  batched INSERT
┌─ STORE ──────────────────────────────────────────────────────────────────┐
│ ClickHouse                                                               │
│   events        MergeTree · PARTITION toYYYYMM · ORDER BY (project_id,   │
│                 event_name, timestamp, distinct_id) · TTL 1y ·           │
│                 properties = String(JSON) · insert_id read-time dedup    │
│   aliases       ReplacingMergeTree (synced from PG: dual-write + boot    │
│                 backfill) + aliases_dict dictionary → dictGet canonical- │
│                 id stitching in every query                              │
│   sessions_mv   AggregatingMergeTree (session start/end, counts,         │
│                 tokens, cost)                                            │
│   migrations    idempotent CREATE/ALTER at boot (no version ledger)      │
│   chRO user     readonly=2 CONST + GRANT SELECT on app DB only — the     │
│                 run_sql surface (blocks DDL + table-function SSRF)       │
│ PostgreSQL — source of truth for everything non-event: users/sessions/   │
│   workspaces/projects(API keys), dashboards, charts, saved queries,      │
│   cohort audiences, subscription mappings, templates, query feedback,    │
│   aliases, agents + grants/budgets/triggers/conversations/secrets/lab,   │
│   alert rules/channels, audit logs                                       │
│ Redis — rate limits · NATS — ingest queue                                │
└──────────────┬───────────────────────────────────────────────────────────┘
               ▼
┌─ ANALYTICS & PRESENTATION ───────────────────────────────────────────────┐
│ opcore Operations (single definition → REST /api/op/*, in-process agent  │
│ tool, CLI command, MCP tool): activity_summary, recent_events, persons,  │
│ explore_events, run_sql (chRO), run_insight, list_dashboards,            │
│ create_dashboard, create_chart, send_notification,                       │
│ submit_recommendation, remember                                          │
│ Legacy web-only reads in app/routes.go (web-analytics, cohorts, …)       │
│ web/app: dashboards · events · sql · persons · web-analytics · traffic   │
│ · cohorts · replay · monitor · product · alerts — ECharts everywhere,    │
│ event-name catalog autocomplete, DailyReadout narrated home              │
│ alerting/: scheduled Evaluator (threshold + z-score) → channel fan-out,  │
│ recovery notices                                                         │
└──────────────┬───────────────────────────────────────────────────────────┘
               ▼
┌─ AGENT ──────────────────────────────────────────────────────────────────┐
│ One generic runtime (agentcore loop) — every agent is config only        │
│ (persona, scopes, skills, tools) via marketplace presets / AgentGarden.  │
│ Data access ONLY through: tool → opcore.Operation → usecase.Repo →       │
│ storage.Store (default-deny policy, credential vault, sandbox tiers,     │
│ injection guard). Harness: compaction, goal pinning, todos, subagents +  │
│ cross-agent delegation, triggers/scheduler, budgets, memory, Lab.        │
│ Insight write-path: create_chart/create_dashboard, submit_               │
│ recommendation, send_notification, remember (Growth Autopilot loop).     │
└──────────────────────────────────────────────────────────────────────────┘
```

### Why it is shaped this way

- **NATS between HTTP and ClickHouse** so ingestion returns before the OLAP
  write and bursts never block the HTTP layer; the **batcher** exists because
  ClickHouse wants few large inserts (part-count explosion otherwise).
- **Identity stitching via dictionary** (`aliases_dict`) replaced shipping
  N-sized `transform()` arrays into every query — O(1) in-memory `dictGet`,
  PG stays source of truth, ≤60 s staleness.
- **opcore single-definition operations** are the load-bearing governance
  choice: one schema/permission/handler serves web, CLI, in-house agents and
  external MCP clients, so surfaces cannot drift and agents can never reach
  infra directly.
- **`run_sql` on a dedicated `readonly=2` CH user** (not keyword denylisting
  alone) closes the table-function SSRF / cross-tenant class by privilege,
  not by parsing.

### Honest weaknesses (evidence-cited)

| # | Weakness | Evidence |
|---|---|---|
| W1 | **Silent event loss.** Core NATS is fire-and-forget (no persistence, no ack); a worker crash, server restart with queued messages, or CH outage loses events — `EventBatcher` logs failed inserts and drops the batch (`ingestion/batcher.go` `logBatchError`, buffer reset). | `ingestion/queue.go`, `batcher.go:92-99` |
| W2 | **No pipeline self-observability.** Queue depth, insert-failure rate, and ingest lag are visible only in server logs; no alert can fire on them. | `batcher.go` (log-only), `alerting/` (rules query product events only) |
| W3 | **Every read hits raw `events`.** Only `sessions_mv` pre-aggregates; dashboards, DailyReadout, web-analytics, and agent queries re-scan raw rows; cost grows linearly with volume. | `store.go` analytics methods; single MV at `store.go:994` |
| W4 | **Person properties are write-only.** `$set`/`$set_once` land inside event `properties` JSON; there is no person profile store, so person-property filtering/segmentation can't work. | `ingestion/handler.go:99-107` (folded into props), no persons table in `migrateClickHouse` |
| W5 | **CH schema evolution is ad-hoc.** Idempotent boot ALTERs work but have no version ledger; MV backfills and destructive changes have no story. | `store.go:942-958` |
| W6 | **Rate limiting is per-IP only.** No per-project/API-key quota, payload size, or property-count caps — one noisy tenant can degrade all. | `ingestion/ratelimit.go` |
| W7 | **`run_sql` has privilege caps but no resource caps.** An agent can still issue a full-scan monster query; no `max_execution_time` / `max_rows_to_read` on the chRO profile. | `store.go:1050-1067` |
| W8 | **Funnels and retention are not operations.** Cohorts/retention exist as a web-only endpoint; funnels don't exist; agents can't compute either except via hand-written `run_sql`. | `usecase/analytics.go` op list; `app/routes.go` cohorts |
| W9 | **Tracking plan is advisory.** The event-name catalog powers autocomplete but nothing validates incoming events against it (typo'd names create silent junk series). | event catalog (UI-side), no ingest check |

## Part 2 — Improvement Plan

Ordered by risk: durability first (data loss is unrecoverable), then cost,
then modeling, then insight depth. Each phase is an independent sub-ticket
with its own verification; later phases don't block on earlier ones except
P1 → P2 (rollup MVs want the durable pipeline underneath).

### P1 — Ingestion durability & pipeline observability (fixes W1, W2) — size M

**Design.** Replace core-NATS pub/sub with **JetStream**: file-backed stream
on `agentray.events.ingest`, explicit ack **after** `store.InsertEvents`
succeeds. Batcher gains bounded retry with backoff; a batch that exhausts
retries is NAK'd to a dead-letter subject (`agentray.events.dlq`) with a
small CLI replayer (`cmd/cli` gains `agentray events replay-dlq`).
Publisher uses JetStream publish-with-ack so HTTP 200 means "durably queued",
not "hopefully delivered". Duplicate-on-redelivery is acceptable for counts
(rare) and already handled for money paths by read-time `argMax … GROUP BY
insert_id`; set the stream's `Duplicates` window keyed on a message-id hash
as a cheap first-line dedup.

Self-metrics: batcher/worker emit `system.pipeline.*` events (queue depth,
flush size, insert failures, ingest lag = `inserted_at - timestamp` p95)
into an internal project, so the **existing** alerting evaluator and
dashboards cover the pipeline with zero new alert machinery.

*Alternative rejected:* in-process ring buffer w/ WAL file — loses multi-
instance fan-in and adds a bespoke persistence format for no operational win.

**Files:** `internal/ingestion/queue.go` (JetStream publish/consume),
`batcher.go` (retry/NAK), `internal/app/app.go` (stream provisioning),
`internal/config/config.go` (stream/DLQ knobs), `cmd/cli` (replay-dlq),
`infra/gce` (nats `-js` flag + volume).

**Verify:** `go test ./internal/ingestion/...`; live drill: `docker stop
clickhouse` → send 100 events → restart CH → all 100 land (count via
`explore_events`); kill server mid-burst → restart → no gap; DLQ replay
round-trips a poisoned batch.

### P2 — Read efficiency & query guardrails (fixes W3, W7) — size M

**Design.** Add two rollup MVs, both keyed for the dashboards that exist:
- `events_daily_mv` — (project_id, day, event_name, visitor_class,
  referrer_channel) → `count`, `uniqState(dictGet-canonical distinct_id)`;
- `agent_usage_daily_mv` — (project_id, day, agent_id, model_name) →
  tokens in/out, cost, error count, `quantilesState(latency)`.

Analytics reads (`ActivitySummary`, `WebAnalytics`, DailyReadout inputs,
`run_insight` trend metrics) resolve from rollups first and touch raw
`events` only for drill-downs/filters the rollup can't answer. Backfill
each MV once with an explicit `INSERT INTO … SELECT` (MVs only see new
inserts — this is the W5 trap; do it in the same boot-migration step,
guarded by a marker row). Add column codecs on the fat columns
(`properties CODEC(ZSTD(3))`, `timestamp CODEC(Delta, ZSTD)`).

Guardrails: attach a settings profile to the chRO user —
`max_execution_time=30`, `max_rows_to_read` ~500M, `max_memory_usage` cap,
`max_result_rows` — so an agent-issued monster query fails fast instead of
starving ingestion.

**Files:** `storage/store.go` (`migrateClickHouse` + read-path methods),
`usecase/analytics.go` (unchanged contracts, faster impls).

**Verify:** `go test ./internal/storage/... ./internal/usecase/...`;
`EXPLAIN` shows rollup tables on dashboard queries; rollup counts ==
raw-scan counts over a fixed window; a deliberate `SELECT * FROM events`
via `run_sql` fails with the quota error, dashboards still render.

### P3 — Person & group model (fixes W4) — size S/M

**Design.** `persons` ReplacingMergeTree in CH: (project_id, canonical_id) →
merged properties JSON, version = event timestamp. Maintained by the ingest
worker: on `$identify` / events carrying `$set`/`$set_once`, upsert the
profile row (apply `$set_once` only when key absent). The existing `persons`
operation reads profiles joined to activity instead of reconstructing from
raw events; `explore_events`/`run_sql` gain person-property filtering via
JOIN. `$groups` gets the same treatment later (`groups` table) — explicitly
out of scope for this ticket.

**Files:** `ingestion/queue.go` worker fork, `storage/store.go` (table +
`Persons` rewrite), `usecase/analytics.go` (persons op schema additions).

**Verify:** identify with `$set` twice → profile reflects last-write for
`$set`, first-write for `$set_once`; persons page and `persons` op (REST +
MCP) show profile fields; existing persons tests green.

### P4 — Insight depth as config, not code (fixes W8, W9) — size M

**Design.** Promote funnel + retention to first-class `opcore.Operation`s
(`run_funnel`: ordered event list + window → per-step counts/conversion,
built on `windowFunnel()`; `run_retention`: generalizes the cohorts query
behind the web page). Per governance, this instantly gives web, CLI, agents,
and MCP the capability — the cohorts page then consumes its op like any
client. Tracking-plan enforcement: ingest tags events whose name is absent
from the project catalog (`is_unplanned` LowCardinality flag, no rejection),
plus a `system.tracking.unplanned_event` daily digest event the alerting
layer and Growth agent can watch; dead-event detection is a saved insight
over the catalog vs `events_daily_mv`. Scheduled insight digests need **no
new Go**: a marketplace preset wires existing triggers + `run_insight` +
`send_notification` (per AGENT-GOVERNANCE, config only).

**Files:** `usecase/analytics.go` (+2 ops), `storage/store.go` (funnel/
retention queries), `ingestion/handler.go` (catalog flag),
`storage/marketplace.go` (digest preset), web cohorts page → op client.

**Verify:** `run_funnel` via REST, agent tool, and MCP return identical
results on a seeded fixture; cohorts page unchanged pixel-wise but served
by the op; misspelled event shows `is_unplanned=1` and appears in digest.

### P5 — Tenancy fairness & scale-out (fixes W5, W6) — size L, deferred

Gate on volume/tenant growth; do not build speculatively. Contents when
triggered: per-API-key token-bucket quotas + payload/property caps at
ingest; CH versioned migration ledger (small `schema_migrations` table
replacing marker-less boot ALTERs); tiered storage (S3 disk policy for
parts > 90 days, replacing the blunt 1-year TTL); replica + `ON CLUSTER`
story; autocapture sampling policy per project.

### Risks

- **JetStream redelivery duplicates** counts; mitigated by the duplicates
  window + `insert_id` read-time dedup on money paths. Accept ~0 practical
  dup rate over exactly-once complexity.
- **MV backfill trap (W5):** an MV without explicit backfill silently
  undercounts history — P2 makes backfill a guarded, verified step.
- **Rollup drift:** rollup-vs-raw equality check in P2 verification must be
  kept as a test, not a one-off.
- **Ingest-time person upserts** add a PG/CH write per identify; volume is
  low (identifies ≪ events), but the worker must not let profile writes
  block the event batch path — fork them onto their own batcher.

### Next

Implement P1 (`bbs:implement` on this doc's P1 section). P2–P4 are
independent tickets after it; P5 stays parked until a tenant or volume
trigger fires.
