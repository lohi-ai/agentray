# Repair 5: native observer rehearsal fixture and stream sweep

Starting HEAD: `67ed98e1be7cf87f01156afe09e11605cf2d7807`.

## Reproduction and repair

`before-rehearsal.log` records the requested command failing because the configured observer reaches terminal `status=error` with `Pi run: Stream ended without finish_reason`.

`internal/app/observer_rehearsal_test.go` now asserts `stream=true`, emits OpenAI-compatible SSE (`text/event-stream`, indexed `choices.delta`, indexed tool calls, `finish_reason`, and `[DONE]`), and correlates tool results via `tool_call_id` and preceding assistant tool calls. Embedding and HTTP-error responses retain their JSON formats. This follows the repaired runtime observer fixture.

All existing rehearsal assertions are identical: real marketplace install and operator configuration, readiness-gated data-quality outcome, exact tool history, one finding across two runs, suppressed business analysis/unauthorized delivery, terminal provider failure, monitor/Operations visibility, and pause refusal. `after-rehearsal.log` shows two completed honest observations and a finished provider-error run; no run remains hung. The additional streaming assertion guards the fixture's transport contract.

## Complete app/runtime fixture sweep

Searches cover every `*test.go` in both directories, including build-tagged tests: HTTP servers, `NewFauxProvider`, other fake/mock/stub provider names, legacy `ChatResponse`/`ChatDelta`, native stream bindings, callbacks, and prompt entry points. Raw matches are in `fixture-inventory.log`, `faux-provider-inventory.log`, and `faux-and-prompt-inventory.log`.

| Fixture group | Reviewed files / result |
| --- | --- |
| App agent HTTP streams | `observer_rehearsal_test.go` was the remaining unconditional legacy agent response and is repaired. `agent_delegation_e2e_test.go` already branches on the request's stream flag and emits SSE for agent turns; its real delegated run passes after repairing harness startup/configuration. |
| App non-streaming connection checks | `workspace_providers_test.go` intentionally uses JSON for OpenAI/Anthropic `Chat` connectivity probes through `testOwnedProvider`/`NewTierProviderForModelWithSource`; those probes do not enter the native run engine. Its Responses fixture is already SSE, and model listing is ordinary JSON. All pass. |
| Runtime OpenAI/Pi HTTP integration | `pi_children_integration_test.go`, `pi_compaction_integration_test.go`, `pi_conversation_integration_test.go`, `pi_goal_integration_test.go`, `pi_model_integration_test.go`, `pi_questions_integration_test.go`, `pi_request_compaction_integration_test.go`, `pi_runner_integration_test.go`, `pi_trace_integration_test.go`, `lohi_observer_test.go`, and `complete_test.go` already emit SSE directly or via `piChildSSE`. All execute successfully. |
| Runtime provider/fallback/credential HTTP integration | `native_antigravity_test.go`, `native_azure_test.go`, `native_claude_test.go`, `native_codex_test.go`, `native_credentials_test.go`, `native_ladder_capabilities_test.go`, `native_ladder_run_test.go`, `native_pi_messages_test.go`, `native_provider_test.go`, `native_retry_http_test.go`, and `native_runner_fallback_test.go` already use their native vendor event shapes. JSON responses are deliberate error, credential-exchange, or metadata responses; intentional malformed streams test rejection. All execute successfully. |
| Timeout fixture | `scheduler_terminal_test.go` deliberately blocks the model request until cancellation; only embeddings return JSON. Both deadline cases pass and persist terminal runs. |
| Remaining faux providers | All `NewFauxProvider` uses in lifecycle, goal, question, child, delegation, plugin, model, session, and fork tests are tool-host/composition placeholders. Actual model execution uses `RunPi`, native `Callback` messages, or `NativeStream` event streams; none uses a legacy faux provider as its native model stream. The native callback message shape (`content` blocks and `stopReason`) is distinct from legacy HTTP `choices.message` JSON and is valid. All selected cases execute successfully. |
| Other provider stubs | `build_parity_test.go`'s `stubProvider` is explicitly a no-call composition provider, also used by `tool_execution_test.go` for direct tool invocation; neither executes a model stream. |
| Unrelated HTTP servers | App query-security, board/adapter parity and analytics E2E servers exercise app routes/egress, not model streams. Runtime `mcptool_test.go` serves the MCP protocol, not a model stream. |

No further legacy fixtures were found on native execution paths.

## Deviations

- **Delegation E2E readiness** — Task said: sweep other agent HTTP fixtures · Found: its existing SSE fixture could not be exercised because bare TCP readiness allowed PostgreSQL initialization resets (`delegation-e2e-before-readiness.log`) · Chose: reuse existing `waitForPostgres` and `waitForNATS` helpers, preserving all delegation assertions.
- **Delegation E2E connector subject** — Task said: prove other agent fixture compatibility · Found: after readiness, app startup rejected an empty connector ingest subject (`delegation-e2e-before-subject.log`) · Chose: populate `IngestConnectorSubject` using the existing analytics E2E config pattern; `delegation-e2e.log` proves the native delegated run completes.

## Verification

The supplied disposable PostgreSQL test environment was set for all database-backed suite commands. No software was installed. All final commands exited 0:

```sh
go test -count=1 -v ./internal/app -run 'Test(AgentMonitor|ConfiguredObserverOperationsRehearsal)'
go test -count=1 -v ./internal/app ./internal/runtime ./internal/workloads -skip '^(TestReal_|TestQueryCapacityHTTPRefusalEnvelope$)'
go test -count=1 -v ./internal/app ./internal/runtime ./internal/workloads -run '(Lohi.*Observer|ObserverSkillGate|OrdinaryAgentSubmitRecommendation|Scheduled|InstalledAgentMonitor|ConfiguredObserverOperationsRehearsal|CronMatches)'
go test -count=1 -v -tags=e2e ./internal/app -run '^TestAgentDelegationE2E$'
go build ./...
go vet ./...
go vet -tags=e2e ./internal/app
git diff --check
```

The suite log records **550 top-level passing tests**, the focused observer/monitor/scheduled log records **11**, and the delegation E2E records **1**, all with **zero selected skips**. The exact requested regex selects only the rehearsal because the monitor test is named `TestInstalledAgentMonitorRoutesResolveSetupAndMonitoring`; the focused selection and full app suite explicitly execute that monitor test. Counts and package results are saved in `verification.json`; empty build/vet logs indicate successful commands with no diagnostics.

## Environment-only exclusions

Four live-model tests are excluded explicitly using `-skip`: `TestReal_Advisor_CatchesAnAnswerTheTranscriptContradicts`, `TestReal_Advisor_StaysSilentOnACleanRun`, `TestReal_Advisor_HonorsTheOperatorsReviewPriorities`, and `TestReal_Reflection_ProposesImprovementFromRun`. They require unsupplied `AGENTRAY_TEST_OPENAI_BASE_URL`, `AGENTRAY_TEST_OPENAI_API_KEY`, and `AGENTRAY_TEST_OPENAI_MODEL`.

`TestQueryCapacityHTTPRefusalEnvelope` is excluded because it requires an explicitly declared disposable Linux capacity host and `AGENTRAY_QUERY_CAPACITY=1`; this worker runs on macOS. All PostgreSQL-backed app/runtime tests are included. The separately run build-tagged delegation E2E passes; the analytics-only `TestAnalyticsServiceE2E` is outside the default build and this stream-fixture repair's selected E2E scope.

The intermediate rehearsal log retains an implementation-stage failure from missing underscore JSON tags, corrected before the final runs. There are no unresolved repair issues.
