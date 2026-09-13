#!/bin/bash
set -euo pipefail

# Deploy AgentRay to the GCE VM (Caddy + docker-compose).
# Run from: agentray/
#
# Usage: ./infra/gce/deploy.sh --env <dev|prod> [--tag TAG] [--skip-build]
#                                        [--no-gate] [--gate-timeout SECONDS]
#
#   --env           Required. "dev" or "prod".
#   --tag           Image tag (default: <git-sha>-<unix-ts>).
#   --skip-build    Don't run Cloud Build; deploy an existing tag.
#   --no-gate       Switch traffic the moment the new colour is created,
#                   without waiting for its healthcheck.
#   --gate-timeout  Seconds to wait for the new colour to go healthy (default 240).
#
# THE GATE IS A DATA-COHERENCE GATE. The API healthcheck targets /readyz, which
# answers 503 until this colour has applied every row on the durable ingest
# stream — event batches AND connector sync batches — because a colour that is
# still replaying would answer queries from a DuckDB file with a hole in it. Two
# consequences the operator owns:
#   * A legitimately long replay (a long quiet period, a broker outage that
#     backed events up, or a large connector table) can outrun --gate-timeout.
#     Raise it: `--gate-timeout 900`. That is necessary but NOT sufficient: the
#     wait is bounded by the compose healthcheck's own window
#     (start_period + interval × retries, 330s in infra/gce/<env>/docker-compose.yml),
#     because Docker's `unhealthy` verdict is terminal for bg_wait — it removes
#     the new colour instead of waiting the deadline out. A replay expected to
#     outlast that window needs `retries` raised to cover it as well. The refusal
#     either way is safe: bg_wait removes the new colour and Caddy is never
#     repointed, so the old colour keeps serving.
#   * `--no-gate` skips that wait entirely and is therefore a data-coherence
#     waiver, not just a speed switch: traffic can land on a colour that is
#     behind. Read /readyz's body first — it names the reason. `purged-gap`
#     never clears (messages were purged before this colour applied them and no
#     amount of waiting recovers them), and `stream-mismatch` means the stream
#     does not carry this env's subjects at all, so the colour will never be
#     offered another row. Both envs default INGEST_STREAM_NAME to the SAME
#     stream on the shared broker and EnsureStreams rewrites that stream's
#     subject list to the booting env's on EVERY boot, so the env that restarted
#     last owns it and the other one is unwired — including for publishing, which
#     fails with "no response from stream". The durable fix is a stream per env
#     (`INGEST_STREAM_NAME: AGENTRAY_EVENTS_PROD` / `_DEV` in that env's app.env,
#     next to the subjects it already sets); a hand-edited subject list is only a
#     stop-gap, because the next boot of either env rewrites it, and re-running
#     this env's deploy clears this env's refusal by handing the same one to its
#     sibling.
#
# Schema migrations are automatic: the API creates/updates Postgres and
# DuckDB tables at startup. Redis/NATS are shared single instances (infra/)
# serving both envs; Postgres is the existing Cloud SQL instance (secret
# agentray-db-url-<env>); DuckDB is embedded in the API container, one file
# per colour.

PROJECT_ID="lohi-dev-lohi"
ZONE="asia-southeast1-a"
VM_NAME="lohi-app"
API_IMAGE="asia-southeast1-docker.pkg.dev/${PROJECT_ID}/docker/agentray-api"
WEB_IMAGE="asia-southeast1-docker.pkg.dev/${PROJECT_ID}/docker/agentray-web"

ENV=""
TAG=""
SKIP_BUILD=false
GATE=true
GATE_TIMEOUT=240

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env)        ENV="$2"; shift 2 ;;
    --tag)        TAG="$2"; shift 2 ;;
    --skip-build) SKIP_BUILD=true; shift ;;
    --no-gate)      GATE=false; shift ;;
    --gate-timeout) GATE_TIMEOUT="$2"; shift 2 ;;
    -h|--help)    sed -n '3,39p' "$0" | sed 's/^# //; s/^#$//'; exit 0 ;;
    *)            echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

[ -z "$ENV" ] && { echo "ERROR: --env is required (dev or prod)" >&2; exit 1; }
[[ "$ENV" == "dev" || "$ENV" == "prod" ]] || { echo "ERROR: --env must be dev or prod" >&2; exit 1; }

case "$ENV" in
  dev)  WEB_API_URL="https://agentray-dev.lohi2.com" ;;
  prod) WEB_API_URL="https://agentray.lohi2.com" ;;
esac
# Web and the API share one hostname here (one Caddy site, two upstreams), so
# the site origin is the same string. It is threaded separately because
# metadataBase/canonical/robots/sitemap ask a different question than "where do
# I call the API" — see web/lib/brand.ts.
WEB_SITE_URL="$WEB_API_URL"

GCE_DIR="$(cd "$(dirname "$0")" && pwd)"        # agentray/infra/gce
SERVICE_ROOT="$(cd "${GCE_DIR}/../.." && pwd)"  # agentray/
REPO_ROOT="$(cd "${SERVICE_ROOT}/.." && pwd)"   # repo root
CADDY_DIR="${REPO_ROOT}/infra/gce/caddy"

# shellcheck source=../../../infra/gce/lib/blue-green.sh
source "${REPO_ROOT}/infra/gce/lib/blue-green.sh"
# AgentRay is two containers behind one hostname, so it owns two upstream
# snippets. Both move together: the colour is read off the api snippet, both
# containers are started on it, and both snippets flip only after BOTH report
# healthy — a half-switched hostname would serve a new dashboard against an old
# API. (agentray-web declares no healthcheck; bg_wait accepts "running" for it.)
UPSTREAM="${ENV}-agentray-api"
UPSTREAM_WEB="${ENV}-agentray-web"
# --no-gate is not "the old behaviour": traffic still moves in one atomic Caddy
# reload, it just moves before anything has vouched for the new colour.
if [ "$GATE" = false ]; then bg_wait() { :; }; fi

if [ -z "$TAG" ]; then
  if [ "$SKIP_BUILD" = true ]; then
    # No build this run, so a fresh <sha>-<ts> tag wouldn't exist in the registry;
    # reuse the last image published for this env.
    TAG="latest-${ENV}"
  else
    TAG="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)-$(date +%s)"
  fi
fi

echo "==> agentray (${ENV}) → api:${TAG} web:${TAG} (web API URL: ${WEB_API_URL})"

if [ "$SKIP_BUILD" = false ]; then
  echo "==> Cloud Build (api + web)"
  gcloud builds submit "$SERVICE_ROOT" \
    --project "$PROJECT_ID" \
    --config "${SERVICE_ROOT}/infra/cloudbuild.yaml" \
    --substitutions "_API_IMAGE=${API_IMAGE},_WEB_IMAGE=${WEB_IMAGE},_TAG=${TAG},_WEB_API_URL=${WEB_API_URL},_WEB_SITE_URL=${WEB_SITE_URL}" \
    --timeout 30m --quiet
  echo "==> Re-tag :latest-${ENV}"
  gcloud container images add-tag --quiet "${API_IMAGE}:${TAG}" "${API_IMAGE}:latest-${ENV}"
  gcloud container images add-tag --quiet "${WEB_IMAGE}:${TAG}" "${WEB_IMAGE}:latest-${ENV}"
fi

echo "==> Syncing config to ${VM_NAME}"
# scp -r DIR target nests a copy inside target when it already exists, leaving
# the bind-mounted Caddyfile stale — copy caddy files into the dir instead, and
# recreate the agentray dir (secret.env is regenerated by fetch-secrets.sh).
gcloud compute ssh "$VM_NAME" --zone "$ZONE" --project "$PROJECT_ID" --tunnel-through-iap \
  --command "mkdir -p ~/gce/caddy && rm -rf ~/gce/agentray"
gcloud compute scp "$CADDY_DIR"/* "${VM_NAME}:~/gce/caddy/" \
  --zone "$ZONE" --project "$PROJECT_ID" --tunnel-through-iap
gcloud compute scp --recurse --scp-flag="-O" "$GCE_DIR" "${VM_NAME}:~/gce/agentray" \
  --zone "$ZONE" --project "$PROJECT_ID" --tunnel-through-iap

echo "==> Rolling on ${VM_NAME}"
gcloud compute ssh "$VM_NAME" --zone "$ZONE" --project "$PROJECT_ID" --tunnel-through-iap --command "
set -euo pipefail
sudo docker network inspect edge >/dev/null 2>&1 || sudo docker network create edge
sudo bash ~/gce/caddy/fetch-origin-cert.sh
$(bg_seed)
$(bg_validate)
sudo docker compose -f ~/gce/caddy/docker-compose.yml up -d
sudo docker exec caddy caddy reload --config /etc/caddy/Caddyfile 2>/dev/null || true
sudo bash ~/gce/agentray/fetch-secrets.sh '${ENV}'
sudo docker compose -f ~/gce/agentray/infra/docker-compose.yml up -d --remove-orphans

$(bg_pick "$UPSTREAM")
BG_NEW_API=${ENV}-agentray-api-\$BG_COLOR
BG_NEW_WEB=${ENV}-agentray-web-\$BG_COLOR
BG_OLD=\${BG_OLD_COLOR:+${ENV}-agentray-api-\$BG_OLD_COLOR ${ENV}-agentray-web-\$BG_OLD_COLOR}
BG_LEGACY=\${BG_LEGACY:+${ENV}-agentray-api ${ENV}-agentray-web}
sudo env AGENTRAY_TAG='${TAG}' DOCKER_CONFIG=/root/.docker \
  docker compose -f ~/gce/agentray/${ENV}/docker-compose.yml up -d --pull always \
  agentray-api-\$BG_COLOR agentray-web-\$BG_COLOR
$(bg_wait "$GATE_TIMEOUT" "\$BG_NEW_API \$BG_NEW_WEB")
$(bg_switch "$UPSTREAM" "\$BG_NEW_API" 8080)
$(bg_switch "$UPSTREAM_WEB" "\$BG_NEW_WEB" 3200)
$(bg_park "\$BG_OLD \$BG_LEGACY")
sudo docker image prune -f >/dev/null 2>&1 || true
"

echo "==> Deployed. Rollback (seconds, exact same image): ../infra/gce/rollback.sh --service agentray --env ${ENV}"
echo "    The previous colour is parked, not deleted. Add --dry-run to see what it returns to."
