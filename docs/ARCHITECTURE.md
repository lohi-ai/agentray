# AgentRay architecture — four layers

**Status:** implemented. This is the folder-and-API map. Product thesis stays
the closed growth loop on owned event data; these layers keep that loop
extensible without becoming three products.

```
┌── channels ──────────────────────────────────────────┐
│  chat · mcp · schedule · webhook · lab               │
│  reserved: support_widget · voice                    │
└───────────────────────────┬──────────────────────────┘
                            ▼
┌── workloads (Garden packs, config only) ─────────────┐
│  validate · growth · marketing · data · operator     │
│  reserved: support                                   │
└───────────────────────────┬──────────────────────────┘
                            ▼
┌── runtime ───────────────────────────────────────────┐
│  AgentGarden + agentcore loop + policy + sandbox     │
│  authoring/ — draft a definition (authoring-time)    │
│  public libs: ./agentcore  ./sandbox                 │
└───────────────────────────┬──────────────────────────┘
                            ▼
┌── dataplane ─────────────────────────────────────────┐
│  ingest · connector · store · usecase · alerting     │
└──────────────────────────────────────────────────────┘

internal/shared  — config, cronx, credential, opcore, mcpclient
internal/app     — composition root (HTTP). cmd/ wires it.
```

`internal/app` and `cmd/` are the composition root. They may import every
layer. Layers may import `internal/shared`. A `TestLayerImportRules` test
enforces this.

## Layers

| Layer | Path | May import | Must not import |
|---|---|---|---|
| Channels | `internal/channels` | shared | dataplane, workloads, runtime, app |
| Workloads | `internal/workloads` | (none) | shared, dataplane, runtime, channels, app |
| Runtime | `internal/runtime{,/authoring}` | shared, dataplane, agentcore, sandbox | channels, workloads, app |
| Dataplane | `internal/dataplane/{ingest,connector,store,usecase,alerting}` | shared | channels, workloads, runtime, app |
| Shared | `internal/shared/{config,cronx,credential,opcore,mcpclient}` | (none of the layers) | channels, workloads, runtime, dataplane, app |

Persistence (`store`) lives in dataplane. The pack catalog lives in
workloads; `app.New` injects it into store via `SetPackCatalog` so store
never imports workloads.

Public import paths `github.com/lohi-ai/agentray/agentcore` and
`.../sandbox` stay at the module root. External importers (swatter) do not
move.

## How to add a connector

No new source ships in this change. When one is needed:

1. `internal/dataplane/connector/<kind>.go`
2. Implement `connector.Source`
3. `func init() { connector.Register("stripe", openStripe) }`

Admin UI and sync engine read `connector.Kinds()`. Agents never receive a DSN
or a `Source` — they query landed rows via `run_sql`.

## How to add a channel

No new channel ships in this change. When one is needed (Slack, email,
support widget):

1. Add a `channels.Kind` (or un-reserve `KindSupportWidget` / `KindVoice`)
2. `channels.Register(Info{Kind, Mode, Description})`
3. Mount ingress in `internal/app`
4. Convert with `channels.NewEnvelope` and call `runtime.Dispatcher` (async)
   or `Runner` (sync stream)

Do not add a new run engine. Do not put business logic in the adapter.
Support/voice must land transcripts as events on the same person graph.

## How to add a pack

1. Write a `workloads.Pack` literal
2. `workloads.Register(p)` from `init`

Marketplace list, install, and default-agent seed read the registry. A pack
carries persona + scopes + skills, and may name non-scope tools in `Tools`
(e.g. `web_fetch`) — install grants those through the same RBAC-checked setter
the Tools tab uses, and only tools that need no config. `support` is the one
category still reserved and empty: a support agent has no inbound channel to
read yet.

Categories track a product's phases, not org charts. `validate` is the
pre-product one — `product-scout` runs with an **empty dataplane** by design
(evidence from `web_fetch` plus the owner's testimony) and ends by writing the
tracking plan that fills the dataplane, which is the handoff into the others.

## What this is not

- Not three products (growth / ops / CS). One runtime, one data plane, packs
  and channels as config/adapters.
- Not a move of `agentcore`/`sandbox`. Those remain the public runtime API.
- Not a CDP. Only add a connector when a pack will query the landed table
  the following week.
