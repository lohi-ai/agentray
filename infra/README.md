# Build and deploy

Production workloads are declared in [`2server/`](../2server/README.md). Use CLI
0.2.0 or newer, Bun and a private 2server connection to the target VM. Building
also requires authenticated `gcloud` access to Cloud Build and Artifact Registry.

```bash
# Build both images, then deploy API followed by web.
./infra/gce/deploy.sh --env prod

# Reconcile exactly the images declared in the source files.
./infra/gce/deploy.sh --env prod --skip-build

# Deploy a previously built tag; CLI resolves it to a digest on the VM.
./infra/gce/deploy.sh --env prod --skip-build --tag RELEASE_TAG

# Test the script with fake cloud/CLI commands, without a VM.
make test-deploy
```

The script validates both files and checks the connected target before building.
It owns Cloud Build and image publication, then calls `2srv deploy -f` directly.
It works in a standalone AgentRay checkout: no parent Lohi helper is required.
Public settings and secret declarations live in the app files. The VM retains
secret values. The API runs its existing database migrations at startup.

Blue-green, readiness, traffic switching, drain and rollback belong to the CLI.
An API failure stops the script before web. A web failure leaves the successful
API deployment in place; use the individual file commands to retry or roll back.
There is no shared transaction across the two app files.

`cloudbuild.yaml` remains the build definition. `gce/infra/docker-compose.yml`
and `gce/infra/nats.conf` are retained for the existing Redis/NATS services; they
are not applied by normal app releases. Their volumes and live VM configs are
not removed by the source cleanup. The retired dev/prod app Compose files and
Secret Manager fetch script are superseded by the App files and VM secret CLI.

Both Cloud Build and Docker contexts exclude private dotenv and `.2server/`
files. Do not place secrets in build arguments or tracked YAML.

Migration is declared in `2server/api.yaml` as `preDeploy: {command: [agentray,
migrate], timeoutSeconds: 600}`. Ship an image with the `migrate` subcommand;
it migrates shared PostgreSQL without HTTP, NATS ingestion or DuckDB access.
Existing startup migrations remain idempotent for deployments outside 2server;
per-colour DuckDB initialization stays at startup. The hook must pass before
rollout begins, and its private output stays on the VM.

## Private 2ai dependency

The Go foundation is pinned in `go.mod`. On the build host, authenticate GitHub
and set `GOPRIVATE=github.com/2found/*`. `deploy.sh` prepares a temporary context
from tracked working-tree files and the pinned dependency source, then uploads
that context. Git credentials never enter the context. Local module replacements
are rejected. New source files must be staged in Git before deploying.

For a local container build without deploying:

```sh
python3 infra/prepare_build.py /tmp/agentray-build-source
docker build -t agentray-local /tmp/agentray-build-source
```

Choose a new output path; the preparer never overwrites an existing directory.
Only the staged `go.mod` receives a local replace. The committed module remains
version-pinned and works without sibling checkouts. Cloud Build configuration
and runtime deployment targets are unchanged.
