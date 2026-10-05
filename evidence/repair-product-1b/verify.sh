#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
export AGENTRAY_TEST_DATABASE_URL='postgres://long@127.0.0.1:5433/agentray_test?sslmode=disable'
export AGENTRAY_LIVE_PG="$AGENTRAY_TEST_DATABASE_URL"
go build ./...
go vet ./...
go test ./internal/app ./internal/runtime ./internal/dataplane/store ./internal/workloads \
  -run 'Test(AgentRunCredentialPrecedenceOverOwnerSession|AgentRunHonorsReadOnlyGuardWithAuthorGrant|EveryAgentTurnUsesCredentialAwareReadOnly|InstalledDataAnalystBoardAuthorJourney|ADemoViewersQuestionRunsReadOnly|ReadOnlyRunKeepsReadsAndDropsWrites|PermittedToolNamesUnderReadOnly|ReadOnlyPolicyBlocksAWriteTool)$|TestLohiEvidence|TestQueryAccess' \
  -count=1 -v
go test -overlay=evidence/repair-product-1b/reviewer-after-overlay.json ./internal/app \
  -run '^TestReviewMixedCredentialChatRemainsReadOnly$' -count=1 -v
go test ./internal/dataplane/store \
  -run '^(TestAgentMemoryFoldInLive|TestAgentMemoryUpdateLive|TestRecommendationDedupeLive|TestDemoSignupCreatesOnlyTheCallersOwnProject|TestDemoAbsentSignupGrantsNoExtraMembership|TestDemoSignupJoinsTheSharedDemoAsViewer|TestDemoBackfillIsIdempotentAndNeverDowngrades|TestDemoMisconfiguredIDDisablesTheDemoInsteadOfFailingBoot|TestDemoQuotaCountsPerUser|TestDemoQuotaIsPerDay|TestDemoQuotaHoldsUnderConcurrency|TestRepairSeededChartsRewritesOnlyTheSeededQuery)$' \
  -count=1 -v
go test ./... -p 1 -count=1 -timeout=30m -v
git diff --check
