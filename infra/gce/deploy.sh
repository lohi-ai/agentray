#!/bin/bash
set -euo pipefail

# Build/push, then let 2server reconcile the explicit app files.
# Public runtime policy: 2server/*.yaml. Secret values: connected VM.
# API PostgreSQL migration: spec.preDeploy. Readiness/recovery: 2server/README.md.

PROJECT_ID="lohi-dev-lohi"
API_IMAGE="asia-southeast1-docker.pkg.dev/${PROJECT_ID}/docker/agentray-api"
WEB_IMAGE="asia-southeast1-docker.pkg.dev/${PROJECT_ID}/docker/agentray-web"

ENV=""
TAG=""
SKIP_BUILD=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env|--tag)
      [[ $# -ge 2 && "$2" != --* ]] || { echo "ERROR: $1 requires a value" >&2; exit 1; }
      if [ "$1" = --env ]; then ENV="$2"; else TAG="$2"; fi
      shift 2 ;;
    --skip-build) SKIP_BUILD=true; shift ;;
    -h|--help)
      echo "Usage: $0 --env prod [--tag TAG] [--skip-build]"
      echo "--skip-build uses images declared in 2server/*.yaml unless --tag overrides them."
      exit 0 ;;
    *)            echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

[ -z "$ENV" ] && { echo "ERROR: --env is required (prod)" >&2; exit 1; }
[[ "$ENV" == "prod" ]] || { echo "ERROR: Lohi runs production AgentRay only; dev is retired." >&2; exit 1; }

WEB_API_URL="https://agentray.lohi2.com"
# Web and the API share one hostname here (one Caddy site, two upstreams), so
# the site origin is the same string. It is threaded separately because
# metadataBase/canonical/robots/sitemap ask a different question than "where do
# I call the API" — see web/lib/brand.ts.
WEB_SITE_URL="$WEB_API_URL"

GCE_DIR="$(cd "$(dirname "$0")" && pwd)"        # agentray/infra/gce
SERVICE_ROOT="$(cd "${GCE_DIR}/../.." && pwd)"  # agentray/
cd "$SERVICE_ROOT"
command -v 2server >/dev/null || { echo 'ERROR: install 2server CLI 0.2.0 or newer' >&2; exit 1; }
# Fail before building when the source config or VM target is unavailable.
for config in 2server/api.yaml 2server/web.yaml; do
  2server validate -f "$config" >/dev/null
  2server get -f "$config" >/dev/null
done

if [ -z "$TAG" ] && [ "$SKIP_BUILD" = false ]; then
  TAG="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)-$(date +%s)"
fi

echo "==> agentray (${ENV}) → ${TAG:-images declared in config} (web API URL: ${WEB_API_URL})"

if [ "$SKIP_BUILD" = false ]; then
  # Resolve private Go source on the authenticated host, never in Cloud Build.
  BUILD_STAGE="$(mktemp -d "${TMPDIR:-/tmp}/agentray-build.XXXXXX")"
  trap 'rm -rf "$BUILD_STAGE"' EXIT
  python3 "${SERVICE_ROOT}/infra/prepare_build.py" "${BUILD_STAGE}/source" >/dev/null
  echo "==> Cloud Build (api + web)"
  gcloud builds submit "${BUILD_STAGE}/source" \
    --project "$PROJECT_ID" \
    --config "${SERVICE_ROOT}/infra/cloudbuild.yaml" \
    --substitutions "_API_IMAGE=${API_IMAGE},_WEB_IMAGE=${WEB_IMAGE},_TAG=${TAG},_WEB_API_URL=${WEB_API_URL},_WEB_SITE_URL=${WEB_SITE_URL}" \
    --timeout 30m --quiet
  echo "==> Re-tag :latest-${ENV}"
  gcloud container images add-tag --quiet "${API_IMAGE}:${TAG}" "${API_IMAGE}:latest-${ENV}"
  gcloud container images add-tag --quiet "${WEB_IMAGE}:${TAG}" "${WEB_IMAGE}:latest-${ENV}"
fi

# Each unique build tag is resolved to a digest by the CLI on the VM.
# Without an override, the file's image remains authoritative.
API_ARGS=(deploy -f 2server/api.yaml --apply)
WEB_ARGS=(deploy -f 2server/web.yaml --apply)
if [ -n "$TAG" ]; then
  API_ARGS+=(--image "${API_IMAGE}:${TAG}")
  WEB_ARGS+=(--image "${WEB_IMAGE}:${TAG}")
fi
2server "${API_ARGS[@]}"
2server "${WEB_ARGS[@]}"
