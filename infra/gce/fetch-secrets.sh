#!/bin/bash
set -euo pipefail

# Fetch AgentRay secrets from Secret Manager into <env>/secret.env (mode 600).
# Runs ON the VM; gcloud authenticates as the VM service account via metadata.
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
# Master AES key encrypting BYO provider keys (workspace model tiers). Shared by
# both envs — must stay stable, or already-encrypted keys can't be decrypted.
ENC_SECRET="$(gcloud secrets versions access latest --secret="agentray-agent-key-enc-secret" --project="$PROJECT_ID")"
[ -z "$POSTGRES_URL" ] && { echo "ERROR: empty value for $DB_SECRET" >&2; exit 1; }
[ -z "$ENC_SECRET" ] && { echo "ERROR: empty agentray-agent-key-enc-secret" >&2; exit 1; }

umask 077

OUT="${SCRIPT_DIR}/${ENV}/secret.env"
mkdir -p "$(dirname "$OUT")"
TMP="$(mktemp)"
printf 'POSTGRES_URL=%s\nAGENT_KEY_ENC_SECRET=%s\n' "$POSTGRES_URL" "$ENC_SECRET" > "$TMP"
mv "$TMP" "$OUT"
chmod 600 "$OUT"

echo "Wrote $OUT"
