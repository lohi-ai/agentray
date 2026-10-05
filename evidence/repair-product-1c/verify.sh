#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
export AGENTRAY_TEST_DATABASE_URL='postgres://long@127.0.0.1:5433/agentray_test?sslmode=disable'
export AGENTRAY_LIVE_PG="$AGENTRAY_TEST_DATABASE_URL"
go build ./...
go vet ./...
go test -overlay=evidence/repair-product-1c/after-overlay.json ./internal/app -run '^TestReviewLiveControlUsesCurrentAuthority$' -count=1 -v
go test ./internal/app ./internal/runtime ./internal/dataplane/store ./internal/workloads \
  -run 'Test(AgentRunCredentialPrecedenceOverOwnerSession|AgentRunHonorsReadOnlyGuardWithAuthorGrant|EveryAgentTurnUsesCredentialAwareReadOnly|InstalledDataAnalystBoardAuthorJourney|ADemoViewersQuestionRunsReadOnly|ReadOnlyRunKeepsReadsAndDropsWrites|PermittedToolNamesUnderReadOnly|ReadOnlyPolicyBlocksAWriteTool)$|TestLohiEvidence|TestQueryAccess|TestLiveRegistry|TestPiLiveInput' \
  -count=1 -v
go test ./... -p 1 -count=1 -timeout=30m -v
git diff --check
