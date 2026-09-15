# AgentRay API Architecture

Go service that handles event ingestion, analytics queries, auth, and dashboard management for AgentRay.

## Stack

| Layer | Technology |
|---|---|
| HTTP | Echo v4 |
| Metadata DB | PostgreSQL (pgx/v5 pool) — users, sessions, workspaces, projects, dashboards, charts, saved queries |
| Event DB | DuckDB (embedded in the API process) — all captured events, queried for analytics |
| Message queue | NATS — decouples HTTP ingestion from DuckDB writes |
| Rate limiting | Redis (sliding window, per IP) |
| Language | Go 1.25 |

## Directory Layout

Layer map (source of truth): [`ARCHITECTURE.md`](ARCHITECTURE.md).

```
agentray/
  cmd/server/main.go            — entry point, wires config + graceful shutdown
  agentcore/ · sandbox/         — public runtime libraries
  internal/
    channels/                   — ingress catalog (chat, mcp, schedule, webhook, lab)
    workloads/                  — Garden packs (growth, marketing, data)
    runtime/                    — AgentGarden + scheduler + runner (package agentruntime)
      authoring/                — free text → draft SOUL.md / AGENTS.md (authoring-time)
    dataplane/
      ingest/                   — Capture, Batch, Identify, NATS → DuckDB
      connector/                — source plugin registry (postgres shipped)
    app/                        — composition root (HTTP)
    shared/                     — config, cronx, credential, opcore, mcpclient
    dataplane/store · usecase · alerting
  sdk/browser/autocapture.ts   — browser SDK
```

## Request Lifecycle

### Analytics reads

```
GET /api/activity?project_id=xxx
  └─ projectFromRequest()         — resolves project from api_key or session cookie
       └─ store.ActivitySummary() — DuckDB query
            └─ JSON response
```

`projectFromRequest` accepts either `?api_key=` / `X-API-Key` header (SDK use) or a valid session cookie + `?project_id=` (dashboard use). Auth and project resolution are always the first two steps in every protected handler.

It answers **admission, not access** — which credential may address the project.
That is the whole question for exactly one route, `GET /api/projects`, which
returns the project a credential named and reads no analytics and is the only
route that calls it: there is deliberately no wrapper with a reassuring name,
because a named admission resolver is a hatch any later route could reuse to
skip its class. Every other route declares the class of its work and asks the
same `Registry.Allow` decision `/api/op` makes, through `authorizedProject` —
one resolver, so the requirement decides and the name cannot disagree with the
verb — with `legacyRead` / `legacyWrite` in `internal/app/op_adapter.go`
stating the requirement: `analytics:read` for the reads (the class
`activity_summary` and `persons` carry), `dashboards:write` / `plans:write` /
`analytics:read` for the mutations. Without it a route ran for any credential
that could reach the project — a management key minted `sources:read` alone
read every analytics route and a key minted `analytics:read` alone created and
deleted audiences and saved queries, while `/api/op` refused the identical
calls. The routes that predate the registry (cohort audiences, saved queries,
templates, the subscription mapping, activity, persons, events, sessions) state
their class at the call site.
`TestNoRouteResolvesThroughTheReadResolver` counts the admission-only resolver's
call sites over this package's source — form-independently, so a helper cannot
hide one — and requires the set to be exactly `GET /api/projects` and
`authProject`, the modern surface's resolver; it separately requires every route
that declares a class to have a behavioural case in the read or write matrix,
and every case to still match a route.

### Writes: the floor in front of every handler

Reads are open to any member of a project's workspace. Writes go through one
middleware first — `demoWriteGuard` in `internal/app/demo_guard.go`, mounted in
`app.go` after the operation registry exists so it can ask the same `Allow`.

It is a second *invocation* of the one decision, not a second rule. The modern
`authProject` surface has no per-route class (the fence names that surface as
admission-only on purpose), so a mutating request that would otherwise skip
Allow is asked here: `legacyWrite(AccessDashboardsWrite)`. Demo non-owners fail
because `sessionGrants`, given the project, withheld the write class — Allow
itself stays demo-blind. The demo fact lives on the project.

**It is fail-closed by default.** A route is exempt only by being named in
`writeClasses` with a reason: session lifecycle, the caller's own account, a
public collection endpoint, a read that carries a body, or the agent-ask
surface (open to demo visitors, and metered in the chat handler against
`AGENTRAY_DEMO_AGENT_RUNS_PER_USER_PER_DAY`). Anything unlisted — including a
route added next year — is denied for a caller without the write class.
`TestTheRouteTableMatchesTheSource` scans this package's source and fails when
a mutating route is registered that the floor's table does not name.

The floor is demo-unaware: it does not return early when no demo is configured.
A Bearer that is present but does not resolve is a denial rather than an
absence: `principalFromRequest` answers it `401` instead of falling through
to the cookie, and `resolveWriteScope` reports the same refusal.

One legacy route decides twice on purpose: `POST /api/saved-queries/:id/run` is
an `analytics:read`, but caching the result is an `UPDATE` to the owner's
`saved_queries` row, so the handler asks the registry for `dashboards:write`
before refreshing the cache.


### Event ingestion

```
POST /capture  (or /batch, /e/, PostHog-compatible aliases)
  └─ ingestion.Handler.Capture()
       └─ EventQueue.InsertEvents()   — json.Marshal → nats.Publish
  └─ HTTP 200 returned immediately

NATS subject (agentray.events.ingest)
  └─ EventWorker (goroutine)
       └─ json.Unmarshal → store.InsertEvents() → DuckDB batch insert
```

The NATS queue is the only async component. HTTP returns before the DuckDB write. The worker uses a channel-buffered NATS subscription (`ChanQueueSubscribe`, buffer 1024) so bursts don't block the HTTP layer.

### Auth flow

Sessions are stored in PostgreSQL. On login/signup the server sets an `HttpOnly` cookie (`agentray_session`). All dashboard API calls carry this cookie. The `authFromRequest` helper reads and validates the cookie, returning an `authContext{User, Session}`.

## Storage Layer (`internal/dataplane/store/store.go`)

`Store` holds a `*pgxpool.Pool` (Postgres) and an embedded `*DuckDB`.

- **Postgres** — users, sessions (TTL), workspaces, projects (API keys), dashboards, charts, saved queries
- **DuckDB** — `events` table. All analytics queries (`ActivitySummary`, `WebAnalytics`, `Persons`, `ExploreEvents`, `AgentReplay`, `RunInsight`, `RunSQL`) hit DuckDB directly.

`EventFilter` is the shared query parameter struct populated by `filterFromRequest()` from query string params (`hours`, `from`, `to`, `event_type`, `event_name`, `distinct_id`, `session_id`, `agent_id`, `model_name`, `search`, `error_only`, `limit`).

### Metric catalog and declared boards (`store/metric_catalog.go`, `store/boards.go`)

A board's content is one document on its `dashboards` row
(`definition JSONB`, `board_key`, `definition_updated_at`): sections, and the
tiles inside them. A tile declares a catalog metric (the server computes it) or
places a chart that already belongs to the board. The model, its rules and its
compatibility story are in
[DESIGN-BOARD-CONTENT-MODEL.md](DESIGN-BOARD-CONTENT-MODEL.md).

`metric_definitions` is the catalog: one row per metric, carrying its label,
unit, kind, definition, required instrumentation, and the displays a tile may
draw it as. The rows are the projection of the Go declaration in
`metric_catalog.go`, refreshed on every boot — the declaration is the authority,
the table is what every consumer reads (web, MCP, `run_sql`). A metric's
`definition` text is the same constant the overview read prints beside the
number, so the explanation cannot outlive the computation.

Five operations serve the model, each declared once in
`internal/dataplane/usecase/boards.go` and projected onto REST, the agent tool,
the CLI and MCP by opcore: `list_metrics`, `read_metric` and `get_board`
(`analytics:read`), and `save_board` + `set_metric_target`
(`dashboards:write`, revision-fenced and idempotent). `read_metric` projects
the metric out of the shared `Store.Overview` read — there is no second
computation behind a board tile.

`metric_targets` is the project-scoped, append-only target history
(`store/metric_targets.go`): a target is a direction, a value on the metric's
own scale, and the complete-day window it is judged over — plus a currency for
the per-currency revenue metric. Changing or clearing a target appends a
version; a read cites the highest version whose `effective_at` is at or before
the window's end, and the verdict (on_track / at_risk / off_track, or a named
reason it cannot judge) is computed once in `Overview` and served on
`OverviewMetric`, `OverviewRetentionPoint` and `MetricReading`. A board tile's
`target` object declares through the same write — an identical declaration
restates the latest version instead of duplicating it — and only
`set_metric_target` with `clear` removes one: a board never clears by
omission.

### Event retention (`internal/dataplane/store/retention.go`)

`events` is append-only, so it is bounded by policy rather than by the engine: a
daily sweep deletes events whose `"timestamp"` is older than
`EVENT_RETENTION_DAYS` — **default 365 days; `0` keeps every event**. The default
is not a new decision. The ClickHouse schema this store replaced carried
`TTL toDateTime(timestamp) + INTERVAL 1 YEAR`, and 365 matches that window in
days rather than by calendar — the old TTL was calendar arithmetic, so the two
differ by a day across a leap day.

What the sweep bounds is how long events live, not how large the file gets:
volume inside the window is unbounded, and the file also holds `persons`,
`aliases` and `external_rows`, which the sweep never touches. A per-colour
DuckDB file can therefore keep growing on a single VM while retention is
enabled, and the disk-headroom argument this default carries is about the event
log alone.

The sweep is admitted from the scheduler's minute tick but runs on its own
goroutine, in batches, under a wall-clock budget — that tick also drives alert
evaluation and connector syncs, and a multi-minute delete must not hold it. Each
run logs the cutoff and how many events it deleted; a window shortened on an
already-grown database converges over consecutive sweeps instead of taking one
window per day. Only `events` is swept: person profiles, aliases and connector
landing rows are kept.

## Configuration (`internal/shared/config/config.go`)

## Shutdown

`Server.Shutdown()` drains the NATS subscription (flushes in-flight events), closes NATS, Redis, and Postgres in order. Triggered by SIGTERM in `cmd/server/main.go`.

## Adding a New Analytics Endpoint

There are two surfaces, depending on whether the agent should be able to call it:

**Agent-facing capability (preferred for anything the analyst may use).** Declare
it **once** as an `opcore.Operation` in the usecase layer; the REST endpoint
(`POST /api/op/<name>`), the in-process agent Tool, the CLI command, and the MCP
tool (`POST /mcp`, for external agents like Claude Code) are all derived from that
single definition. Full recipe — and the rule that the agent reaches data only
through the `usecase` `Repo` interface, never `storage` directly — is in
[AGENT-GOVERNANCE.md](AGENT-GOVERNANCE.md).

The MCP server is mounted in `internal/app/mcp_routes.go` via `opcore.MountMCP`
over the same registry, deps, and project resolver as the REST adapter; external
clients authenticate with the project API key. A portable client skill ships at
`.agents/skills/agentray-analytics/SKILL.md`.

**Plain web-only endpoint (not exposed to the agent).** Legacy `routes.go` style:

1. Add a query method to `storage.Store` in `store.go` — Postgres or DuckDB query depending on data source
2. Register a new `GET /api/my-endpoint` handler in `routes.go` — call `projectFromRequest(c, store)` first, then call the store method
3. Return `c.JSON(http.StatusOK, map[string]any{"project": project, "my_data": result})`
4. Add the corresponding API method and TypeScript types in `web/lib/api.ts`
