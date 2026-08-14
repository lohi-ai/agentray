# AgentRay API Architecture

Go service that handles event ingestion, analytics queries, auth, and dashboard management for AgentRay.

## Stack

| Layer | Technology |
|---|---|
| HTTP | Echo v4 |
| Metadata DB | PostgreSQL (pgx/v5 pool) — users, sessions, workspaces, projects, dashboards, charts, saved queries |
| Event DB | ClickHouse — all captured events, queried for analytics |
| Message queue | NATS — decouples HTTP ingestion from ClickHouse writes |
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
    dataplane/
      ingest/                   — Capture, Batch, Identify, NATS → ClickHouse
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
       └─ store.ActivitySummary() — ClickHouse query
            └─ JSON response
```

`projectFromRequest` accepts either `?api_key=` / `X-API-Key` header (SDK use) or a valid session cookie + `?project_id=` (dashboard use). Auth and project resolution are always the first two steps in every protected handler.

### Event ingestion

```
POST /capture  (or /batch, /e/, PostHog-compatible aliases)
  └─ ingestion.Handler.Capture()
       └─ EventQueue.InsertEvents()   — json.Marshal → nats.Publish
  └─ HTTP 200 returned immediately

NATS subject (agentray.events.ingest)
  └─ EventWorker (goroutine)
       └─ json.Unmarshal → store.InsertEvents() → ClickHouse batch insert
```

The NATS queue is the only async component. HTTP returns before ClickHouse write. The worker uses a channel-buffered NATS subscription (`ChanQueueSubscribe`, buffer 1024) so bursts don't block the HTTP layer.

### Auth flow

Sessions are stored in PostgreSQL. On login/signup the server sets an `HttpOnly` cookie (`agentray_session`). All dashboard API calls carry this cookie. The `authFromRequest` helper reads and validates the cookie, returning an `authContext{User, Session}`.

## Storage Layer (`internal/dataplane/store/store.go`)

`Store` holds a `*pgxpool.Pool` (Postgres) and a `clickhouse.Conn`.

- **Postgres** — users, sessions (TTL), workspaces, projects (API keys), dashboards, charts, saved queries
- **ClickHouse** — `events` table. All analytics queries (`ActivitySummary`, `WebAnalytics`, `Persons`, `ExploreEvents`, `AgentReplay`, `RunInsight`, `RunSQL`) hit ClickHouse directly.

`EventFilter` is the shared query parameter struct populated by `filterFromRequest()` from query string params (`hours`, `from`, `to`, `event_type`, `event_name`, `distinct_id`, `session_id`, `agent_id`, `model_name`, `search`, `error_only`, `limit`).

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

1. Add a query method to `storage.Store` in `store.go` — Postgres or ClickHouse query depending on data source
2. Register a new `GET /api/my-endpoint` handler in `routes.go` — call `projectFromRequest(c, store)` first, then call the store method
3. Return `c.JSON(http.StatusOK, map[string]any{"project": project, "my_data": result})`
4. Add the corresponding API method and TypeScript types in `web/lib/api.ts`
