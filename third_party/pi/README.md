# Pi reference for agentcore alignment

The implementation target is now a **native Go port**, in `agentcore`, `ai`,
and `telemetry`. This directory is the pinned development reference and test
oracle. The worker bridge documented below remains available during the
transition, but extending or deploying it is not the port strategy. Remove its
runtime wiring after Go replacements cover the existing callers and durable
session behavior. The first Go telemetry runtime is in [`telemetry`](../../telemetry/).

`upstream/` preserves the complete `packages/agent`, `packages/telemetry`, and
required `packages/ai` trees from [earendil-works/pi](https://github.com/earendil-works/pi)
at `eeac84ca92498ac18b6832754d01aef1d3c5f654`, including tests and the MIT license.
`UPSTREAM.json` records every file's Git blob ID, SHA-256, and executable mode.
Changes to the reference belong in a new upstream revision, never local patches.

`agentcore/pi.ts` and `agentcore/telemetry.ts` expose these original APIs.
`agentcore.NewPi` runs the original Agent in a child process and connects Go
providers, tools, and event listeners through JSON callbacks. The existing Go
`Agent` is still a separate implementation. The server runner can explicitly
select Pi as described below; the default remains Go while migration is in
progress. Source identity and the new runtime's tests do not prove that the old
Go implementation matches Pi.

## Build and use the runtime

From the repository root:

```sh
make pi-build
make test-pi
```

The build verifies the source before and after bundling. Its aliases resolve
Pi imports to `upstream/`, never a registry package or the test copy. No model
catalog hydration is needed to build the runtime. Deploy the entire generated
`third_party/pi/dist/` directory together; its entry points share ESM chunks.
`dist/build.json` records the pinned commit and the actual build input graph.

Native JavaScript/TypeScript hosts import `dist/agentcore.mjs` and
`dist/telemetry.mjs`. These expose the complete upstream executable APIs;
`dist/telemetry-testing.mjs` exposes the original adapter conformance suite.
The bundles run under Node 22.19+ or Bun. They include their runtime dependencies.
`dist/stream.mjs` exports the host's `nativeStream` dispatcher, which lazily
loads the unchanged Pi API implementation named by `model.api`.

Go hosts call `agentcore.NewPi(ctx, agentcore.PiConfig{Worker: workerPath, ...})`.
The worker path is `dist/worker.mjs`. Go hosts use Bun by default, following the
repository's backend runtime convention; set `PiConfig.Runtime` to `node` to
select Node 22.19+ explicitly. The lifetime context or `Close()` stops
and reaps the process. Each instance owns one independent Pi Agent. Initialization
returns the worker's upstream commit, available through `UpstreamCommit()`.

`PiConfig.Options` is native Pi `AgentOptions` JSON, with serializable tool
definitions under `initialState.tools` and optional callback names under
`callbacks`. `PiConfig.Callback` receives `stream`, `tool`, and registered hook
requests. It may run concurrently; honor its context cancellation.

Set `Options.streamMode` to `"native"` to call providers directly through Pi's
original API implementations. Supply a native `initialState.model` (including
`api`, `provider`, `id`, `baseUrl`, input/context limits, and cost metadata) and
register `getApiKey` to resolve credentials per request. Credentials belong in
that callback, outside durable model/state JSON. Native mode needs no `stream`
callback and keeps provider signatures, rich content, usage, and streaming
frames in their original native representation. It supports all ten API names
in the pinned Pi `KnownApi` type, including OpenAI completions/responses,
Anthropic, Google, Bedrock, Mistral, and Pi Messages. Unknown APIs fail through
Pi's error lifecycle; they never fall back to a Go provider. The default
`"callback"` mode remains available for a host-supplied model runtime.

`Options.streamOptions` supplies serializable provider controls such as
`maxTokens` and `temperature`. Per-request Pi values take precedence. Credentials,
signals, telemetry, and function hooks cannot be supplied through this object.

- In callback mode, `stream` receives `{model, context, options}`. Return a native Pi assistant
  message. For streaming, use `emit` with native `AssistantMessageEvent` frames.
- `tool` receives `{toolCallId, toolName, args}`. Return a native `AgentToolResult`;
  use `emit` for partial results. Pi performs argument validation and scheduling.
- Supported hooks are `convertToLlm`, `transformContext`, `getApiKey`,
  `beforeToolCall`, `afterToolCall`, `finishTurn`, `prepareRequest`,
  `prepareNextTurn`, and `prepareNextTurnWithContext`. Hook results retain Pi's
  native vocabulary. Replacement tools are attached to the Go tool callback.
- In native provider mode, `onPayload` receives `{payload, model}`. Return the
  replacement payload, or no value to keep Pi's payload. Cancellation follows
  the active native run; hook failures use Pi's provider failure lifecycle.
- `OnEvent` receives each unchanged `AgentEvent` and is awaited by Pi, including
  `agent_end`. It can query state or queue steering, but must not await completion
  of the run whose event it is handling.

`Call` also exposes `setState`, `configure`, queue clearing and inspection,
`waitForIdle`, `reset`, and `telemetry`. `setState` accepts only model, thinking
level, messages, and tools; Pi owns streaming and pending-call state. `configure`
accepts queue modes, session ID, thinking budgets, transport, retry-delay cap,
and tool execution mode. Context cancellation of a prompt aborts that specific
run. Wait for idle before submitting another prompt after cancellation.

The JSON boundary cannot preserve function identity or arbitrary JavaScript
objects. Synchronous `prepareArguments` needs a bundled native implementation:
the ask plugin supplies `ask-v1` through `PiArgumentPreparer`. Unknown identifiers
are rejected, and arbitrary Go preparers still require a native implementation.
Provider-internal function hooks `onResponse` and
`onProviderStreamEvent` belong in the host provider or the native API.
`pendingToolCalls` is serialized as an array. Responses above 64 MiB fail the
Go transport explicitly. No old Go Message or StreamEvent conversion is applied.

The worker uses the unchanged in-memory telemetry adapter for
`agentray.agent.run`, `agentray.ai.request`, and `agentray.tool.execute` spans.
These are application-owned names, not invented Pi schema names. Snapshots
are available with `Call(ctx, "telemetry", nil)`. They are process-local and
grow for the process lifetime, as upstream's in-memory recorder does.

`PiConfig.OnTrace` optionally receives each settled native model request,
response or failure, and the original request span subtree. The server runner
wires this callback to its existing trace sink. PostgreSQL stores native context
messages with lossless prefix compression scoped to the run and session; opaque
provider fields participate in that comparison. The Lab API reconstructs the
original JSON values as `native_trace`, with `session_key` and `depth` for child
attribution. Display fields and usage totals are projections; legacy replay
rejects native records. Native gate display uses the same session's subsequent
tool-result receipts, without run-wide provider-call-ID overlays.

Trace delivery is best-effort: callback failures and panics do not change native
results. Delivery is serialized per worker and drained at run completion, so a
slow sink can delay completion. Each delivery and database write has a five-second
timeout. Packets over 60 MiB are skipped to stay below the bridge's 64 MiB limit.
Stream options, including provider credentials, are not included in trace packets.

The bridge itself adds no Go-specific permissions, budgets, retries, compaction,
or durable session policy. `internal/runtime.NewPiSession` supplies a host adapter
for permissions and durable native sessions:

- A nil policy denies all tools. Advertisement and execution both consult the
  supplied `agentcore.Policy`; replacement tool definitions are filtered too.
- The session owns a store lease until `Close()`. The existing session store
  retains native event and state JSON in `SessionEntry.Content`, without a
  database schema change or conversion to the older Go message model.
- Each physical tool invocation gets a unique durable intent and completion
  receipt. A failed intent write prevents execution. A persistence fault stops
  further work, and an interrupted effect prevents automatic resume.
- Resume restores native messages, model, and thinking level. Tool definitions
  and callbacks come from the current host configuration. State checkpoints
  record the worker revision; missing or changed revisions require migration.
- Recovery can finish placement of an exact tool-result `message_start` after
  a crash, retaining Pi's original fields and timestamp. An execution receipt
  alone cannot reconstruct a native message. In that earlier crash window,
  resume returns `ErrPiUnsettledEffect` without replaying the tool.

Supply `PiSessionConfig{Pi, Policy, Store, SessionID}` for a new session, and set
`Resume: true` to reopen it. Always close the returned session. An existing legacy
Go session is rejected rather than converted silently. The adapter offers
`Prompt`, `Continue`, `State`, and `PendingQuestion`. A native goal contract is
stored separately and must match the resumed host. The server runner restores
that contract under the session lease before composing the new host.

Goal revision remains explicitly opt-in (`BuildParams.ReviseGoal`, or
`PiSessionConfig.ReviseGoal` for a directly composed run). The goal plugin commits
`pi_goal_revision` before changing its condition. Each record retains the previous
condition, new condition, reason, physical effect ID, and direct or nested call ID.
No-op or denied updates create no revision. A failed commit leaves the gate
unchanged and stops the run. The next turn appends a named native system section;
original messages remain intact. Resume restores the revised condition even when
further revision is disabled. A revision record never substitutes for a missing
native tool result or makes an unsettled effect safe to replay.

The ask plugin completes a normal native tool result saying it is waiting,
then the host ends the run as parked. Its settled effect receipt is the durable
question; the effect UUID is its workflow ID, so repeated provider call IDs do
not reuse old answers. `RecordSessionAnswer` records one `pi_answer` under the
session lease. Resume appends its exact native user message through Pi's prompt
lifecycle, preserving the already emitted tool result. Recovery recognizes
delivery from native history and rejects duplicate or changed answers. Reopening
an unanswered question emits the host question event without a model request.

`Agent.OpenPiTools(ctx)` connects an existing composed server tool registry to
Pi. Its `Definitions(ctx)` returns native tool declarations; route `tool`
callbacks to `Execute(ctx, params, emit)`. Execution reuses the existing Go
validation, permission, credential, result-bound, and nested-invocation boundary.
Pi retains ownership of scheduling and transcript placement. The returned
`PiToolOutcome` and native result `details` carry audit records, additional
context, and parked-workflow metadata; the enclosing host must handle those
workflow decisions. Tool execution alone does not run turn/context/stop hooks;
the `RunPi` host integration below connects turn and stop policies.

The tool host owns the composed Agent's busy slot and extension resources.
Close the Pi session/worker first, then close the tool host. Extension context
contributions live for the entire host lifetime, so background jobs survive
the callback that launched them. Direct and nested tool calls share its atomic
execution budget. For a durable tool host, `PiSession` stamps the already-recorded
effect identity on each callback using `WithToolInvocationScope`; a raw caller
must supply its own durably recorded invocation scope. Provider call IDs can be
reused without reusing an unrelated effect's idempotency key. Arbitrary Go
`ArgPreparer` implementations are rejected during setup because Pi requires
argument preparation to run synchronously before native schema validation.

`internal/runtime.RunPi` wraps a native session as one server run. It returns
the unchanged final `State` and `Telemetry` together with a `Projection` in the
server's existing `RunResult` vocabulary, and accepts its existing `StreamSink`.
The projection is for display, traces, and accounting; it is not a replayable
transcript. Seed and resume from native state/session records. Usage counts only
assistant messages emitted during this invocation, so reopening a session does
not bill its historical messages again. Set `PricingKnown` only when the host
supplied known model prices; otherwise nonzero usage is marked unpriced.

The run wrapper surfaces Pi's state-carried provider errors as Go errors. On
cancellation it aborts the native run, allows up to five seconds for terminal
events and the final snapshot, then closes the worker and releases the lease.
Callers can route tool callbacks through `PiToolHost.Execute` and retain its
audit metadata in the projection without converting provider messages through
the legacy Go model. Production default selection and the remaining plugin
lifecycle/workflow integration are still pending.

Set `PiRunConfig.Host` to the run's open `PiToolHost` to connect composed prompt,
step, batch, and stop policies. `Task` is the memory-recall query. The adapter
adds the composed persona, skill headers, recalled memory, and extension prompt
as a named native system section; a resumed run replaces that section through
an ordinary system message. Native provider messages stay untouched. Step and
stop injections travel through Pi's prompt and `prepareNextTurnWithContext`
hooks, so Pi emits the normal message lifecycle and the session logs them.
`finishTurn` applies the existing batch/stop extensions, including the server's
goal gate. Tool outcomes retain source order for their extra context, and
parked or terminal outcomes end the native run. Turn/tool/spend ceilings allow
one tool-free wrap-up and suppress further stop-guard continuations. The tool
boundary also denies physical execution during that wrap-up.

Native request-view hooks use `agentcore.Hooks.PiContext`: they receive isolated
raw-message copies, so signed provider content and extension fields need no Go
message conversion. Errors, panics, and invalid output are reported through
`OnError` and retain the previous valid view, including under `HookThrow`, in
accordance with Pi's non-throwing `transformContext` contract. The todo plugin
registers this hook alongside its legacy hook. It pins one fresh bounded plan
reminder per request and restores the plan from successful native tool-effect
receipts; denied, unexecuted, or failed updates do not replace the plan.

Consumer steering is drained before each request and again before accepting a
final answer, so corrections arriving during the last response are honored.
Follow-ups drain after stop guards accept the answer. Both become native
messages through the normal lifecycle and survive session recovery. Initial
draining waits until the worker and session lease exist; startup failure does
not consume queued corrections. The consumer's sources still own input not yet
delivered to the native lifecycle.

Each host serves one run. `RunPi` closes its run resources before folding child
spend and emitting terminal observers; an additional `Close` is safe. The tool host and native
session must agree on the session ID and whether persistence is enabled. A conflicting native
turn-preparation, finish, or context-transform hook is rejected rather than overwritten. This
integration currently covers prompt contributions, step gates and extensions,
turn-start/end observers, batch/stop extensions, and tool workflow outcomes.
Legacy context/request rewriting, the legacy durable inbox, and
remaining observers still need native integration. Native child-runtime selection
is described below.
Child usage already contributes to the parent's budget and final totals.

`ModelTier.BindPi` binds a server-resolved API-key model to the original Pi
provider implementation. It preserves the configured endpoint and API dialect,
applies output caps, and derives native cost metadata from the host pricing
table. Its returned pricing flag can be passed to `RunPi`. Credential refresh
runs before every provider request and responds to cancellation; a failed
refresh never falls back to an old key. The request hook validates the complete
bound model before resolving credentials, including after a host hook changes
request settings. Restoring a session with a different model descriptor or
endpoint requires explicit migration, so a previous endpoint cannot receive a
new binding's credential. OAuth account pools and fallback ladders are rejected
until their native lifecycle integration is implemented.


## Select Pi in the server runner

The server selects the in-process Go port with `AGENTRAY_AGENT_NATIVE_GO=true`,
which wires `WithPiRuntime(PiRuntimeConfig{NativeGo: true})` for parent runs,
children, and auxiliary summaries. The Docker image contains only the Go server;
it no longer builds or ships Bun or the Pi worker bundle. The old
`AGENTRAY_AGENT_PI_WORKER` and `AGENTRAY_AGENT_PI_RUNTIME` settings are no longer
read by the application. The legacy Go driver remains the default while native
OAuth account pools and fallback lifecycles are being migrated. Native selection
never silently falls back to either that driver or the TypeScript worker.

The TypeScript bridge remains available to explicit development/test callers
through `WithPiRuntime(PiRuntimeConfig{Worker, Runtime})` for differential testing.

Selected runs use the same resolved workspace model, composed tool host, budget
admission, run rows, terminal persistence, and tool traces. Their `RunResult`
also carries `native_state`, `native_revision`, and `native_telemetry`; `messages`
is a display projection. Supply unchanged Pi message arrays through
`RunOptions.NativeHistory` and pin their worker revision with
`RunOptions.NativeHistoryRevision`.

Conversation routes store native transcript deltas on the active branch and
reuse the original messages for subsequent turns. The selected Pi chat path
also owns ordinary replies without a separate Go classification call. Provider
blocks, signatures, tool results, and extension metadata remain server-side;
the public conversation entries expose only the native node's branch metadata.
Rendered assistant replies and human-answer display entries are never replayed.
Input IDs distinguish consumed steering messages from late or undrained inputs,
which remain in the next turn's context. Atomic leaf checks reject stale turns
after a fork, clear, or newer native completion. PostgreSQL row locks serialize
conversation appends and sequence assignment. Regeneration follows the last
input actually consumed by the native turn, excluding later pending corrections.

Existing legacy assistant history or compaction requires explicit migration or
`/clear` before Pi can use the conversation. Conversely, the Go history reducer
and compactor reject an active native path. Native `/compact` and automatic
between-turn compaction use the original Pi Agent and provider on the workspace's
cheap tier. An immutable `pi_compaction` checkpoint replaces an older prefix with
a running summary, while preserving recent message blocks and all native system
and tool-declaration deltas. Earlier branches remain recoverable. The summary
call treats historical system instructions as source material and has no business
tools; empty, failed, or truncated summaries do not publish. A leaf comparison
rejects a summary if input, a fork, or another completion changed the branch while
it was being generated. Unanswered human questions must be answered first.
The conversation API maps the checkpoint to the existing compaction divider.

Long-running native loops also compact their request view through Pi's original
`transformContext` hook. This can cut at a completed assistant/tool batch even
when the run has only one user prompt. The full native transcript stays unchanged
in memory and durable storage. A separate `pi_context_summary` record binds the
summary to the exact native prefix and upstream revision; resumed workers reuse
it without another summary call or another usage charge. Changed opaque fields
invalidate that cached view. Recent blocks, tool-result batches, system sections,
and tool declarations remain intact.

The runner resolves the agent's compaction task tier into `PiCompactionTier` and
uses the original provider for auxiliary summaries. Forks inherit the configured
policy but own independent summary state. Summary usage, including usage reported
with a failed summary, contributes to run and child totals; trace sessions keep
these auxiliary requests separate from conversational turns. Failed, empty,
cancelled, or larger summaries retain the last safe view. A summary checkpoint
must persist before that view can reach the next provider call. The policy uses
approximate token counts and needs a completed prefix plus a recent tail; it does
not guarantee that a single oversized message fits the model's window.

Parked answers resume from the last native checkpoint after
checking it against the durable run state. A parked checkpoint containing
undrained conversation input currently fails that check and requires explicit
reconciliation rather than silently discarding that input.

Tool choice, parallel-tool generation, and structured-output hints use Pi's
native `onPayload` hook for Chat Completions, Responses, and Anthropic Messages.
Host schema validation rejects invalid text even if the provider ignores the
hint, while native history retains the original response and failure event.
Self-delegation uses the same original Pi runtime. The subagent plugin first
creates its governed `Agent.Fork`, then invokes the native consumer through
`RunFork`; child permissions, hooks, tool limits, and nesting caps remain
inherited. Each child has its own worker, provider session, native transcript,
and durable session lease. The parent receives its answer and current usage.
Schema-correction retries seed the original native child messages, including
opaque provider fields, rather than the Go display projection.

Native child session IDs use the physical invocation's idempotency key, so a
provider reusing a tool-call ID cannot accidentally reattach to an earlier
child. An immutable invocation contract binds each child to its task, prompt,
and native retry seed. Completed children return their recorded answer with
zero new usage. The completion receipt is verified against the original native
message digest and worker revision. Interrupted children may continue from a
recoverable native prefix without repeating settled tool effects. An ordinary
ended child with no host completion receipt requires explicit reconciliation:
native `agent_end` alone cannot prove host validation and finalization succeeded.

A parked child is recognized from its settled question receipt and original
native tool result in the last invocation's final batch. Reattachment returns
`PiChildQuestionError` with its child session ID, physical question ID, and
question, without starting a worker or charging historical usage. After
`RecordSessionAnswer` under that child's lease, the same fork request resumes
with the exact recorded answer as a new native user message. Reused provider
call IDs cannot reuse prior answers. A physical receipt without the original
native tool result still requires recovery; no replacement message is invented.
Parent cancellation cancels native child work; cancelled or parked children
cannot publish a successful completion receipt. Routing that child workflow
through the parent UI and reconciling an unsettled parent delegation effect
remain separate work.

The runner rejects legacy history and legacy turn-preparation overrides. Model binding also rejects OAuth pools and fallback
ladders. Migrating existing legacy transcripts
and native delegation recovery across unsettled parent
effects remain unfinished.
The selection is an intermediate migration path, not completion of the default
server migration.

## Verify source identity

```sh
python3 third_party/pi/verify.py
python3 third_party/pi/verify.py --upstream /path/to/pi-checkout
```

The second command checks the manifest against the actual pinned Git tree.
Both commands reject missing, added, changed, or executable-mode-drifted files.

## Run the upstream tests

Requires Python 3.10+, Bun, and Node 22.19+.

```sh
cd third_party/pi
bun install --frozen-lockfile --ignore-scripts
bun run prepare:reference
bun run test
bun run typecheck
```

Preparation copies the verified source into ignored `.work/`, then runs Pi's
own strict model-data hydration command. Hydration fetches public catalogs from
the network; these generated, untracked AI model values are not part of the
pinned Git tree and can change on subsequent runs. They are required by Pi's
compatibility entry point, including its agent tests.

The test and typecheck commands verify the working source against the pinned
manifest before executing. Only generated JSON under the AI provider data
directory is exempted in `.work/`; no additions or edits are allowed in the
agent or telemetry package. The six upstream test files run without provider
credentials, including Pi's faux-provider end-to-end cases and telemetry
adapter conformance suite.
