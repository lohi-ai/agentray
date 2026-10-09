# AgentRay deployment configuration

Run these commands from the AgentRay checkout with 2server CLI 0.2.0 or newer.
Use a release containing `2srv`; older installations can use the `2server` alias.
`api.yaml` and `web.yaml` each declare a complete production workload. 2server
generates Compose/Caddy; existing physical volume bindings remain on the VM.
The API declares logical volumeMounts and instanceEnv for its NATS durable.
Development AgentRay on the Lohi VM is retired.

Both source images stay on `:latest`. Cloud Build publishes each unique release
tag and `:latest` from the same build. The wrapper deploys the unique tag using
`--image`, leaving YAML unchanged. Direct CLI deployment and plain `--skip-build`
use the latest published build, even if rollout failed. API/web rollout is not
atomic; each image's `latest` may advance independently if a build fails partway.

```bash
2srv validate -f 2server/api.yaml
2srv plan -f 2server/api.yaml
2srv deploy -f 2server/api.yaml --apply
2srv deploy -f 2server/web.yaml --apply

# Optional override for a freshly built image; the YAML stays unchanged.
2srv deploy -f 2server/api.yaml --image REGISTRY/IMAGE@sha256:DIGEST --apply
2srv rollback -f 2server/api.yaml --apply
```

The CLI discovers the nearest private `.2server/connection.yaml` (legacy JSON
also works), or accepts `--connection FILE`. Source files own public configuration.
The VM owns secret values, shared infrastructure and deployment history. No local
dotenv is implicitly used for a deployment.

Secret names and references are in `api.yaml`. Import values from a private file:

```bash
2srv secret set --app agentray-api --env-file /private/agentray.env --apply
2srv secret list --app agentray-api
```

Changing a stored secret takes effect on the next deployment. Keep the encryption
key stable: replacing it can make existing provider credentials unreadable.

## Readiness and data

The API uses `/readyz` for the deployment gate and Caddy health checks. `/healthz`
only proves that the process is alive. Each colour has its own DuckDB volume and
NATS durable; both consume the production stream declared in `api.yaml`.
Never share the DuckDB file or reset a durable as a deployment shortcut.

A parked colour can have a large replay backlog. Inspect readiness and verify
that pending work decreases before increasing `spec.progressDeadlineSeconds`
(maximum 3600). The old colour keeps serving while the candidate catches up.
`purged-gap`, `store-behind` and `stream-mismatch` require a data/stream repair;
waiting or replacing the gate with `/healthz` does not repair them. Do not erase
loss markers, volumes or consumers merely to pass the gate.

API database migrations run at application startup. The release script builds
images; the CLI manages container rollout, readiness and Caddy. API and web are
separate deployments, so releases must support the intermediate API/web version
combination. If the second deployment fails, inspect it and explicitly retry or
roll back the first; the CLI does not claim a multi-app atomic transaction.

Redis/NATS remain existing shared services defined in
[`infra/gce/infra`](../infra/gce/infra/). Applying these App files does not replace
their containers or volumes. PostgreSQL remains on Cloud SQL.

Migration is declared in `2server/api.yaml` as `preDeploy: {command: [agentray,
migrate], timeoutSeconds: 600}`. Ship an image with the `migrate` subcommand;
it migrates shared PostgreSQL without HTTP, NATS ingestion or DuckDB access.
Existing startup migrations remain idempotent for deployments outside 2server;
per-colour DuckDB initialization stays at startup. The hook must pass before
rollout begins, and its private output stays on the VM.
