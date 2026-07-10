#!/bin/bash
set -euo pipefail

# Fetch AgentRay secrets from Secret Manager into <env>/secret.env and
# infra/secret.env (mode 600). Runs ON the VM; gcloud authenticates as the VM
# service account via metadata.
#
# Usage: ./fetch-secrets.sh <dev|prod>

PROJECT_ID="lohi-dev-lohi"
ENV="${1:?usage: fetch-secrets.sh <dev|prod>}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

case "$ENV" in
  prod) DB_SECRET="agentray-db-url-prod" ;;
  dev)  DB_SECRET="agentray-db-url-dev" ;;
  *) echo "ERROR: env must be dev or prod" >&2; exit 1 ;;
esac

POSTGRES_URL="$(gcloud secrets versions access latest --secret="$DB_SECRET" --project="$PROJECT_ID")"
CH_PASSWORD="$(gcloud secrets versions access latest --secret="agentray-clickhouse-password" --project="$PROJECT_ID")"
# Master AES key encrypting BYO provider keys (workspace model tiers). Shared by
# both envs — must stay stable, or already-encrypted keys can't be decrypted.
ENC_SECRET="$(gcloud secrets versions access latest --secret="agentray-agent-key-enc-secret" --project="$PROJECT_ID")"
[ -z "$POSTGRES_URL" ] && { echo "ERROR: empty value for $DB_SECRET" >&2; exit 1; }
[ -z "$CH_PASSWORD" ] && { echo "ERROR: empty agentray-clickhouse-password" >&2; exit 1; }
[ -z "$ENC_SECRET" ] && { echo "ERROR: empty agentray-agent-key-enc-secret" >&2; exit 1; }

umask 077

OUT="${SCRIPT_DIR}/${ENV}/secret.env"
mkdir -p "$(dirname "$OUT")"
TMP="$(mktemp)"
printf 'POSTGRES_URL=%s\nCLICKHOUSE_PASSWORD=%s\nAGENT_KEY_ENC_SECRET=%s\n' "$POSTGRES_URL" "$CH_PASSWORD" "$ENC_SECRET" > "$TMP"
mv "$TMP" "$OUT"
chmod 600 "$OUT"

# Shared infra (ClickHouse container) needs only the password.
INFRA_OUT="${SCRIPT_DIR}/infra/secret.env"
mkdir -p "$(dirname "$INFRA_OUT")"
TMP="$(mktemp)"
printf 'CLICKHOUSE_PASSWORD=%s\n' "$CH_PASSWORD" > "$TMP"
mv "$TMP" "$INFRA_OUT"
chmod 600 "$INFRA_OUT"

echo "Wrote $OUT and $INFRA_OUT"
