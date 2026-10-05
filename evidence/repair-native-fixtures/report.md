# Repair 4: observer native fixtures

## Base and reproduction

- Starting HEAD: `0b56ca4fe73d9e46bd2e2287c73becdea41f62ca`.
- The starting branch did **not** contain `db33b8b`; all three targeted tests passed on its legacy engine (`pre-rebase.log`).
- Rebased the branch onto local current main, `d4b333970e86db664873c884e10cd0ff9a1951a0`, which contains `db33b8b`. No rebase conflicts.
- Repeated the same command on the native engine: both provider-fixture tests failed with `agentcore: native provider is required`, and all four scheduled-run identifier cases failed with `Pi run: Stream ended without finish_reason` (`failing-before.log`).

```sh
export AGENTRAY_TEST_DATABASE_URL='postgres://long@127.0.0.1:5433/agentray_test?sslmode=disable'
go test ./internal/runtime -run '^(TestObserverSkillGateAcceptsEveryReadSkillIdentifier|TestOrdinaryAgentSubmitRecommendationKeepsTerminalBoundary|TestLohiObserverScheduledRunPersistsDeliveryFailureAndRetry)$' -count=1 -v
```

## Repair and diagnosis

`internal/runtime/lohi_observer_test.go` now binds an `ai.FallbackProvider` to the same `nativeChildResponse` / `nativeChildCall` stream fixtures used elsewhere in the runtime suite. Request counting replaces `FauxProvider.Recorded`; all skill acceptance, unrelated-skill rejection, single terminal-boundary write, delivery failure/retry, deduplication, persisted status/summary and exact tool-order assertions remain intact.

The scheduled-run failure was a fixture artifact. Its HTTP stub returned `application/json` with `choices.message` for a native request with `stream=true`; the native SSE parser never received any delta or finish event. The stub now asserts streaming, returns `text/event-stream` with indexed `choices.delta`, `finish_reason`, and `[DONE]`, and correlates tool results using the native `tool_call_id` and preceding assistant calls rather than the optional tool-result `name`.

`Runner.Run` already persists model failures with `status=error`, `FinishedAt`, and an observer `data_quality:availability` summary that leaves readiness/business conditions unverified (`internal/runtime/runner.go`). The native completions accumulator correctly rejects streams without `finish_reason` (`ai/openai_native_stream.go`). Scheduled deadline tests exercise terminal persistence both during the model call and before it starts. A repeated selection exposed a separate real race: native setup can return generic `context.Canceled` for a scheduler deadline; the old runner preserved the cause only for error-free aborted results. `Runner.execute` now joins the caller cancellation cause with an existing failure, retaining the native error and deadline in its persisted terminal summary while leaving successful completed answers complete. Reproduction is in `scheduled-deadline-cause-before.log`; ten repetitions of both timeout cases are in `scheduled-deadline-cause-after.log`.

## Deviations

- **Scheduled trace fixture** — Plan said: migrate the three observer fixtures · Found: the full suite also failed `TestScheduledProviderTimeoutPersistsTerminalRun`, whose trace assertion required the legacy deadline error text · Chose: require exactly one native `aborted` / `Request aborted` trace while retaining the persisted deadline, availability, terminal-state, no-tools and pre-model-no-trace assertions.
- **Deadline cause preservation** — Plan said: investigate terminal stream honesty · Found: a repeated scheduled run lost its deadline cause when native setup returned `context.Canceled` · Chose: preserve the caller cause alongside an existing failure; never relabel a successful completed result because cancellation arrived later.
- **Chained ask fixture** — Plan said: full runtime suite passes · Found: `TestPostgresChainedAskResumeKeepsDurableSession` fabricated a non-UUID conversation and legacy question/answer entries with no native checkpoint · Chose: create a real conversation, seed its native history and durable state, and use the existing native parked effect receipt shape; retain root-session identity and exactly-one-answer-per-question assertions with native answer records.

The initial broader failures are preserved in `runtime-before-extra-fixture-repairs.log` and `observer-scheduled-before-extra-fixture-repairs.log`; repaired cases are in `additional-fixtures.log`.

## Verification

The three commands are separately recorded in identically named test logs:

```sh
go test ./internal/runtime -run '^TestObserverSkillGateAcceptsEveryReadSkillIdentifier$' -count=1 -v
go test ./internal/runtime -run '^TestOrdinaryAgentSubmitRecommendationKeepsTerminalBoundary$' -count=1 -v
go test ./internal/runtime -run '^TestLohiObserverScheduledRunPersistsDeliveryFailureAndRetry$' -count=1 -v
```

Combined before/after comparison: `failing-before.log` / `passing-after.log`. All focused identifier cases execute; zero focused skips.

Final commands all exited **0**. The full runtime suite passed **467 top-level tests**, with the four environment-only exclusions below; the observer/scheduled selection passed **9 top-level tests with zero skips**. Each of the three individual regression commands passed with zero skips, and the two scheduler timeout cases passed **10 repetitions**. `go build ./...`, `go vet ./...`, and `git diff --check` all passed (build/vet produce no output on success). Results are in matching logs and `verification.json`:

```sh
go test ./internal/runtime -count=1 -v
go test ./internal/runtime ./internal/workloads ./internal/app -run '(Lohi.*Observer|ObserverSkillGate|OrdinaryAgentSubmitRecommendation|Scheduled|CronMatches)' -count=1 -v
go build ./...
go vet ./...
git diff --check
```

## Environment-only exclusions

Only the four existing real-provider tests in the full runtime suite skip: `TestReal_Advisor_CatchesAnAnswerTheTranscriptContradicts`, `TestReal_Advisor_StaysSilentOnACleanRun`, `TestReal_Advisor_HonorsTheOperatorsReviewPriorities`, and `TestReal_Reflection_ProposesImprovementFromRun`. They require `AGENTRAY_TEST_OPENAI_BASE_URL`, `AGENTRAY_TEST_OPENAI_API_KEY`, and `AGENTRAY_TEST_OPENAI_MODEL`, which are not supplied. The focused observer/scheduled selection has zero skips; all PostgreSQL tests use the supplied live database.
