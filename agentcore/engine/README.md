# Native Go agent engine

This package ports `packages/agent/src/agent.ts`, `agent-loop.ts`, `proxy.ts`, and `stream-fn.ts` from Pi
at `eeac84ca92498ac18b6832754d01aef1d3c5f654`. It uses Go structs and callbacks,
the lossless `ai.Message` transcript, and Go providers through `StreamFn`.
Production code neither imports TypeScript nor starts a worker process.

`Run` and `Continue` implement the low-level loop: tool declarations in system
messages, streaming message events, per-request credentials and context
preparation, turn hooks, steering, follow-up, and explicit continuation.
`RunToolCall` applies the same argument preparation, validation, before hook,
execution, and after hook to programmatic/nested calls.
Replacing a pending system message's tool declarations removes obsolete fields,
including explicit nulls retained from decoded history. An unchanged loadout keeps
the caller's original shape. `ai.WithToolChanges` performs this copy without
changing the source message or unrelated wire metadata; native provider capability
filtering uses the same operation.

`AgentLoop` and `AgentLoopContinue` expose the public event-stream wrappers.
`Next` consumes the FIFO events; `Result` independently returns new messages at
`agent_end`. Neither requires a concurrent event reader. Continue rejects an
empty/assistant tail before starting. `AgentEventStream.Wait` observes producer
settlement and Go errors/panics. As in Pi, a rejected producer does not close the
queue or fabricate a terminal result; callers can cancel pending reads after
observing that failure. Thirteen differential cases execute the original public
wrappers, including their rejected-promise behavior. Additional Go tests cover
reader cancellation, synchronous first-event admission and panic handling.

`NewAgent` constructs the stateful wrapper. `Prompt`, `Continue`, `Steer`,
`FollowUp`, queue mode/clear/preview methods, `Abort`, `WaitForIdle`, and `Reset`
follow Pi's lifecycle. State is reduced before subscribers run. Subscribers
are awaited in subscription order, including `agent_end`: a second prompt,
continuation or reset is rejected until the last end subscriber settles.
Run failures produce the original assistant error/aborted event sequence;
admission errors and failures in that recovery sequence are returned to callers.

The Go API uses `State()` snapshots and explicit state setters instead of a
mutable JavaScript state object. These copy top-level arrays; nested message
payloads, tool declarations and model JSON should be treated as immutable.
`Listener` pointers provide stable subscription identity without a new
interface. The live listener set supports removal/addition during delivery.
`Configure` replaces wrapper options atomically. Most callbacks are captured
when a run starts; an installed next-turn preparation wrapper reads its latest
callback, matching Pi. Context-aware preparation takes precedence over the
legacy signal-only preparation callback.

Go contexts carry the active abort signal and parent values. `Abort` or parent
cancellation cancels it. Successful settlement detaches from the parent without
cancelling a saved subscriber signal. Cancelling a `WaitForIdle` wait only stops
that wait. Hooks/listeners must not synchronously wait for the run they belong to.

Tool preflight is ordered. Parallel tools finish independently and emit end
events in completion order; result messages are appended in original call
order. Any sequential tool serializes the whole batch. Every finalized result
must request termination to stop the batch. Truncated assistant messages never
execute their tool calls. Provider errors and aborts remain hard exits.
`ToolHooks.Before` receives `*BeforeToolCall`: replacing its validated `Args`
updates the JSON passed to execution and the after hook, without another schema
validation. `ToolCall.Arguments` remains the original provider input in lifecycle
events and traces. This is the Go equivalent of mutating Pi's validated argument
object, covered by numeric/object replacements and required-key deletion.

Decoded tool results retain extension metadata and explicit null fields in
execution/update events. After-hook replacement preserves original result
extensions and applies only Pi's supported override fields. Missing/null content
becomes `[]` when constructing a transcript tool-result message; nullable usage
and details keep their source presence. Native Go field updates override retained
wire values without modifying copies of the result.

`AfterToolCall.Result` points to the executed result. Mutating its fields affects
the outcome even when the hook returns nil; an override merges onto the mutated
value. The outcome's `isError` was computed before the hook and changes only via
an explicit override. Replacing the context's result pointer has no effect on the
executed object. Overrides and hook failures leave any retained result pointer
with the hook's mutations, matching Pi's object ownership.

Caught Go errors retain their identity. JSON-shaped panic values use Pi's
`Error.message` / `String(value)` error text, including null, arrays, objects,
numeric formatting and nonfinite values. A tool update callback's panic reaches
the executing tool synchronously and can be recovered there; otherwise it becomes
a tool error and runs the after hook. A returned update error represents a rejected
promise: execution settles, then the call rejects without the after hook. Tools
that start their own goroutines own the panic boundary in those goroutines.

`EventSink` is awaited and can be called concurrently by parallel tools. A sink
must synchronize its own mutable state. Tool updates are scoped to an execution;
late updates are ignored. Successful finalization waits for all admitted updates;
a rejected update returns an error without waiting for unfinished siblings, which
continue running. Update errors take precedence over execution errors; already
rejected updates are considered in admission order.
Provider stream implementations must honor cancellation and emit a final
error/aborted message. The loop drains that message even after cancellation.
Contract errors from hooks, streams, or sinks are returned without fabricating
a successful `agent_end`.

`StreamProxy` implements the compact HTTP protocol directly with `net/http`.
It filters request controls, reconstructs text/thinking/tool calls, retains
terminal tool metadata, and settles early EOF and cancellation as error events.
It continues draining after a terminal event, matching Pi's live result behavior.
Use `ProxyStreamOptions.Client` to supply transport policy and `Options` for
the serializable provider controls.

Go producers must wrap changes to published assistant payloads in the stream's
`Synchronize` callback. Raw `Next`/`Result` retain live pointer identity;
`SnapshotEvent`/`SnapshotResult` give isolated copies at observation time. The
engine uses these copies for its transcript and subscribers. `WaitForEnd` waits
for producer settlement, which can happen after the terminal result becomes
available. Read raw payloads inside `Synchronize` or after producer settlement.
These synchronization methods adapt JavaScript's single-threaded execution to
Go; they do not impose event-time snapshots on the underlying stream.

## Verification

```sh
make test-agentcore-native
bun agentcore/engine/testdata/generate-pi-fixtures.ts --check
bun agentcore/engine/testdata/generate-loop-stream-fixtures.ts --check
bun agentcore/engine/testdata/generate-agent-fixtures.ts --check
bun agentcore/engine/testdata/generate-proxy-fixtures.ts --check
bun agentcore/engine/testdata/generate-argument-fixtures.ts --check
bun agentcore/engine/testdata/generate-failure-fixtures.ts --check
```

The Go tests consume checked-in fixtures and require no JavaScript runtime.
The optional fixture generator runs the unchanged upstream implementation after
verifying its source hashes. It compares complete JSON events, request contexts,
messages, error strings, and hook ordering. Concurrent scenarios use explicit
gates to make completion order deterministic in both runtimes. JSON object-key
ordering is not treated as semantic; message content strings remain exact.
There are 64 low-level loop scenarios and 38 stateful Agent scenarios. The
latter record state at every event and action, including queue previews,
subscriber failures, busy admission, reset baselines and forwarded controls.
Additional race tests exercise concurrent queue/state access, blocking end
subscribers, abort propagation, live subscription identity and callback changes.
Another 51 proxy scenarios compare retained events, final messages, and HTTP
request envelopes against Pi, including post-terminal mutations and tool object
identity. Terminal records preserve sparse/null usage, provider usage/cost
extensions, optional thinking-level/error-message presence, and nullable text
and thinking signatures. Every proxy fixture also compares detached snapshots
with the settled live payload. Go HTTP/race tests cover cancellation, concurrent engine reads, frames
over 64 KiB, and UTF-8 split into single-byte reads. Invalid JSON and transport
errors use Go's diagnostic wording; exact JS engine/fetch error text is not
claimed. Malformed frames outside the typed protocol are not fully equivalent.
Sixteen source-generated failure values exercise the Agent failure lifecycle and
all four tool prepare/before/execute/after catch sites. Three update scenarios
compare synchronous throws, tool-caught throws and rejected update promises,
including continuation, after-hook admission and original error identity.
Seven pending-update scenarios compare successful waits, rejection before and
after execution settles, concurrent execution/update failures, and selection
among already-rejected updates.

## Migration status

This is not yet the default production agent. The native host now has a direct
Go `openai-completions`, `openai-responses` and `anthropic-messages` providers, including `streamSimple`, HTTP/SSE, callbacks,
retry and usage accounting. Other provider APIs and removal of the TypeScript
bridge remain pending. Differential fixtures and local HTTP integration tests
cover the implemented paths; they do not establish parity for every API or all
SDK transport and numeric/Unicode edge cases.
The native Responses provider now has 83 transcript/tool, 119 request/compat,
98 event-time stream and 55 HTTP/callback/error differential cases. Its Go
HTTP/SSE entry point covers reasoning backfill, unfinished tool rejection,
service-tier usage, retry, timeout and abort. Another 103 cases cover
`streamSimple` options and synchronous credential admission. Host HTTP tests
cover both model-selected and explicit Responses endpoints, child/summary
routing, and rejection of unfinished calls before any tool effect. Recovery
retains failed/aborted partial calls without waiting for nonexistent results;
unsettled physical effects and incomplete executable batches still block resume.
Anthropic transcript/tool conversion and request construction have 156
differential cases, including strict schemas, managed effort and native tool
changes. Another 86 cases cover its native stream accumulator and SSE reader:
thinking signatures, partial tool arguments, fallback and 1h cache pricing,
transformation diagnostics, callback mutations, aborts, incomplete streams and
split UTF-8/CRLF input. Concurrent snapshot tests preserve live message and tool
identity. Its direct Go HTTP provider has another 62 differential cases for
API-key/header auth, OAuth-token identity, Copilot headers, payload replacement,
callbacks, HTTP errors and retry behavior. Local HTTP tests cover header timeout,
streaming beyond that timeout and body cancellation. A concrete
`AnthropicMessageClient` callback supports custom authentication and endpoint
adaptation through `StreamAnthropicWithClient`. Another 20 differential cases
cover this injection boundary, raw timeout forwarding, payloads, callbacks and
retry ownership; Go tests cover body closure and cancellation propagation.
Concrete alternate-provider adapters and user OAuth token acquisition/refresh
remain pending. The ordinary injected Go HTTP client replaces fetch only. Another 122 cases run
the original Anthropic `streamSimple` against the Go entry point, covering
adaptive effort, custom budgets, context limits, filtered provider options and
synchronous credential rejection. The stream entry points also support workload
federation through scoped/process environment settings. Another 33 cases cover
identity-file exchange, auth headers, redacted errors, response limits, file
rotation and 401 invalidation; 16 SDK cache cases cover advisory/mandatory
refresh and backoff. Concurrent tests cover coalescing and forced overlapping
refreshes. The host's `ModelTier` binding admits federation from scoped/process
settings when no API key or explicit key-refresh callback is supplied. Empty or
failed refresh callbacks still fail. Local HTTP tests cover scoped binding,
shared token caching across parent/child/summary calls, durable effects and
credential exclusion from state/telemetry. File/network/invalid-URL diagnostics
use Go wording, and malformed token types are not fully equivalent.
Anthropic host HTTP tests cover refreshed credentials, generation controls,
signed reasoning replay, durable tools, child isolation and summary calls. A
stream missing `message_stop` rejects even a finalized tool block before any
effect; durable recovery and resume retain the failure without replaying the call.
The imported `ai` package still contains legacy provider adapters that depend
on the old root `agentcore`; this engine does not import that root directly.
Those adapters must be migrated before making the root a facade over this engine.

The host's `internal/runtime.NativeCallbackStream` now connects its existing
lossless stream callback directly to this engine. It preserves immediate
progress, independent callback settlement (including rejection after a terminal
event), request telemetry, cancellation, and late-progress fencing. Seventeen
migration fixtures compare it with the unchanged worker adapter using pinned Pi;
additional Go tests run its failure paths through `engine.Agent`.

`internal/runtime.NewNativeAgent` binds tool execution, request/turn hooks,
state, settings, queues, cancellation, awaited events, and telemetry directly
to this engine. Thirty-four complete host scenarios compare callbacks, events,
state, spans and trace packets with the worker. These exposed missing serializable `model` and
`toolExecution` provider options; the loop now preserves them, and its earlier
fixtures now record every serializable option instead of a selected subset.

`PiSessionConfig.NativeGo` selects this adapter in the existing session and
`RunPi` paths. Its Go-only tests cover tool policy, lossless durable round trips,
unsettled-effect rejection, and composed ask/park/answer/resume with exactly-once
answer delivery. Tests deliberately supply nonexistent worker paths.
`PiRuntimeConfig.NativeGo` now propagates that selection through the runner,
self-delegated children, and auxiliary compaction summaries. Go-only runner tests
cover overlapping children, isolated histories and provider sessions, depth/tool
limits, usage and trace attribution, completion receipts, and reattachment
without repeated work. Child ask/answer recovery retains the original signatures
and delivers the answer once. Wrapped child-question errors now retain their
session, physical question ID and lossless question in the parent's settled
tool receipt (`PiToolOutcome.ChildQuestion`). Go integration tests compare this
route to the child's pending durable question. The parent now parks on its own
physical question ID while exposing the child's question. Reattachment does
not run either model. Answer recording forwards under the child's lease before
acknowledging the parent; tests cover nested routes, conflicts, invalid routes
and retry after a failed parent append. Resume re-enters the governed retry-safe
call with its original physical identity and current policy. An append-only
`pi_delegation` receipt binds each answered question to the next question or to
the final result; completed output reaches the parent as one recorded native
user message, without rewriting its original tool result. Tests cover repeated
questions, nested and parallel children, corrective output-schema retries, and
crashes before the continuation receipt and before/after native delivery. A
parallel turn's displayed question is reconciled with the durable pending ID.
Direct session callers without a composed host still receive
`ErrPiChildResumeRequired` until a host resumes the delegation.
Tool additional contexts are recorded alongside each completion and delivered
once with their own identities. Terminal tool outcomes append their recorded
delivery and checkpoint native state without scheduling a model request; retry
after a failed checkpoint does not re-execute the child or its hook. Native Go
continuations record a separate `agentray.delegation.resume` root and a governed
`agentray.tool.execute` span. Their busy/cancellation lifetime prevents Close
from returning before tool cleanup. Continuations now emit host tool start,
progress, end and result frames, and include governed tool/nested-call traces in
the run projection. A failed continuation receipt produces an error projection;
a delivery-only retry emits no fresh tool activity. These host frames do not
fabricate native events or rewrite provider history. The governed host exposes
`FinishPiDelegationBatch` to apply the original batch policy after child resume,
without rerunning turn-end observers or stop guards. It shares policy with normal
turn completion, retains source call order, isolates callback mutations, and
discards contexts for parked, terminal, or cancelled batches. The runtime binds
the full original batch (including ordinary and local-ask siblings) to all final
child receipts and commits a `pi_delegation_batch` decision before delivery.
Batches containing only local questions and ordinary tools use the same receipt:
all answers must be durable before the hook runs, the original tool outcomes are
checked against their settled receipts, and ordinary sibling contexts survive
resume without reexecuting their tools.
Tool and batch contexts then arrive once, in source order; a terminal sibling
suppresses every additional context and ends the run without another request.
Recovery validates the call digest, predecessor identities, receipt ordering,
and recorded messages. Tests cover mixed and local-only parked batches, delivery prefixes, corrupt
receipts, direct resume without a host, and crashes before the batch receipt,
before/after delivery, and at a terminal checkpoint. A committed batch decision
does not rerun its hook. Failure before the receipt commits can rerun it; external
hook side effects are not exactly-once. Fully delivered older sessions without
batch metadata keep their existing history and do not retroactively run hooks;
partially delivered batches without that metadata fail closed.
The server exposes this path with `AGENTRAY_AGENT_NATIVE_GO=true`. Its production
image no longer ships Bun or the worker bundle. The legacy Go driver remains the
server default pending the remaining provider/OAuth migration and parity audit;
the explicit TypeScript adapter is retained for development/test comparison.
`PiRuntimeConfig{NativeGo: true}` uses the built-in
`NativeProviderStream` for `openai-completions`, `openai-responses` and `anthropic-messages`;
`NativeStream` remains an optional
override. Unported APIs fail explicitly. Worker paths are unused in this mode.
Local HTTP tests exercise refreshed credentials, host payload controls, tool
effects and their durable receipts, retained reasoning/history, streamed output,
child isolation, auxiliary summaries, usage and trace attribution. The app's
default runtime has not switched while the other providers are being ported.

Native request tracing now records both callback and Go-provider requests,
including provider child spans, through the Go telemetry module. `OnTrace`
automatically enables tracing and reuses the existing host sink, preserving raw
request/response data and session/delegation attribution. Packets exclude stream
options, delivery is queued in order with a five-second logical timeout, and
sink failures do not alter the agent result. The run flushes traces before its
span settles. Go-provider streams retain their original identity; tests cover
live descendants, detached snapshots, sink panic/timeout, and durable-session
sink delivery. The native HTTP provider now uses this request tracing path;
provider-specific descendant spans remain to be audited.

Argument normalization ports Pi's serialized JSON-schema path. Validation uses
the existing Go JSON Schema library. Another 323 source-generated programmatic
tool cases compare numeric argument coercion, hook/execution admission, exact
argument/error text, and preserved original input. These include 26 constraint
and path cases: array/object bounds, uniqueness, contains, forbidden tuple tails,
false schemas, negation, numeric bounds/multiples, and property names containing
slashes, tildes, or empty strings. The error projection preserves Pi's first
forbidden tuple index and JavaScript number formatting. Eight additional cases
cover schema property order (including integer keys), required-property order,
keyword order, allOf branch order, additional-property child diagnostics, and
the eight-error limit. Eight more cover pattern-property traversal, excess-property
error caps, nested arrays, shared/chained/recursive local references, and escaped
reference pointers. Reference diagnostics retain the caller's traversal position
while reading property order from the target schema. Ten additional cases cover
schema-valued additional properties (including nested and referenced schemas),
parent summaries, anyOf/oneOf branch diagnostics, multiple matching oneOf
branches, valid extra properties, and error caps across failed branches. Twelve
more cover conditional branches and dependencies, including nested/reference
conditions, schema/key dependencies, traversal order and successful validation.
The port preserves Pi's asymmetric conditional errors: a failing then branch
emits only its summary, while a failing else branch also emits child errors. Ten
property-name cases cover exact child paths and parent summaries, insertion and
integer-key order, nested/reference/conditional schemas, false schemas, successful
validation, and error caps. Name validation restores the enclosing object path
from the Go validator's standalone string errors without mutating its error tree.
Fourteen more cover contains bounds, zero-match admission, bound/array-keyword
ordering, references, dependentRequired/dependentSchemas and sibling keywords
beside references. The default compiler dialect is Draft 2019 so these bounds
are enforced while legacy tuple items remain supported. Twelve dialect cases verify that $schema annotations do not select a different
keyword set, matching TypeBox: legacy and unknown annotations, nested resources,
legacy tuples, reference siblings, and schema-shaped const/enum data. The compiler
projection visits only schema-bearing keywords and preserves property names such
as $schema. Five resource cases cover local pointers, absolute/relative IDs, root
IDs and nested resource fragments. Seven error-collection cases verify that
type/const/enum failures do not suppress sibling checks, including existing allOf
branches and references into them. The compiler isolates these early-return
checks in appended allOf branches and restores source locations before rendering
diagnostics. Ten type-list cases verify original array order and single-element
wording, including references, property-name checks and primitive coercion.
Sixteen multipleOf cases cover floating-point tolerance, positive/negative
remainders, near-zero values, integer shortcuts, rejection beyond tolerance,
references, coercion, conditionals and contains. A concrete validator extension
replaces exact rational divisibility with Pi's remainder calculation and 1e-10
absolute tolerance. Eleven prefixItems cases cover prefix rejection, scalar-tail
offsets, simultaneous legacy tuple checks, coercion, nested/reference schemas,
empty/valid arrays and evaluated-item tracking. A concrete array extension checks
prefixes, scalar items, legacy tuples and additional-item tails. Seven more
tuple-tail cases cover schema-valued additionalItems, first-failure stopping,
nested/reference errors, successful evaluation tracking and ignored non-tuple
additionalItems. The extension emits the first failing tail item directly,
replacing the old aggregated-tail error workaround. The complete newer-keyword
surface still needs parity coverage. Twenty-four pattern cases check raw diagnostic
text and ECMAScript/Unicode matching: lookarounds, backreferences, control/code-point
escapes, ASCII digits, astral characters, line terminators, whitespace and property
patterns. The Go regexp2 adapter preserves source text and excludes JavaScript
line separators from unescaped dots. Full RegExp syntax and malformed-pattern
errors remain unproven. Nine unevaluated-data cases cover object/array summaries,
schema-valued checks, allOf evaluation, nested/reference schemas, valid data and
more than eight invalid children collapsing to one summary. Complete evaluated-key
and evaluated-index propagation across failing schemas remains to be audited. Twelve
additional object-evaluation cases cover failed properties/patterns/additional
properties, overlapping successful checks, allOf/reference failures and successful
extra properties. The extension preserves schema/instance key order and discards
prior local marks after failed child checks, matching Pi's failure-frame behavior
in these cases. A concrete object extension marks keys only on success; direct
child errors replace the previous synthetic false-property error workaround. Nine
array-evaluation cases verify failing scalar/tuple/prefix checks, marks created
after a failure, references, successful arrays, and ordering between additional
items, tuple items and prefixes. Array checks now share one extension that marks
indices after successful checks and resets local marks on a child failure. Nine
contains-evaluation cases cover matching index tracking, mixed element order,
zero minimum, references/allOf, and marks cleared by items then restored by an
explicit minContains. Contains checks do not expose errors from unmatched items. `ai.ParseJSNumber` supplies
the shared Number(string) behavior used by tool coercion and Anthropic token
expiry parsing: decimal grammar, ECMAScript whitespace, arbitrary-width
hex/binary/octal integers, rounding, signed zero, underflow and overflow. Tool
coercion separately rejects empty/nonfinite inputs, matching Pi. Go-only hex
float syntax and numeric separators are rejected. The complete TypeBox keyword/error-order
and JavaScript schema-extension surface is not yet proven equivalent; current
fixtures establish common coercion, optional-null, missing-property and type
failure behavior. Do not infer full parity or production readiness from this
subset. Models/options remain raw until the full Go AI provider contract lands.

The upstream MIT license is retained in `LICENSE.pi`.

Native session and runner configuration now selects Go when no worker path is
provided. Explicit worker paths remain available for development/test comparison.
`go test -race -tags pi_native ./internal/runtime` runs the shared integration
suite through Go, including lifecycle, child sessions, goals, request compaction,
model binding and telemetry. PostgreSQL-backed cases still require the test DB.
Three source Agent fixtures verify sparse/nullable initial history and replacement
history across a prompt. Decoded message/tool JSON retains absent and null fields
while unchanged; modified values serialize normally. This preserves original
history through request-only compaction and session recovery.

The native Codex Responses port has 68 request-body, 112 event-time,
17 SSE framing, 44 auth/URL/header, 112 retry/error, 24 full HTTP and 44 simple-mode fixtures
against the pinned source. `ai.StreamCodexResponsesSSE` executes explicit SSE
in Go, with zstd compression, cache identity, callbacks, retries and header-only
timeouts. Local HTTP tests verify body lifetime and cancellation. HTTP fixtures
compare decompressed JSON, not exact compressed bytes; malformed diagnostics
and nonstandard JavaScript date formats remain unproven.
`ai.StreamCodexResponsesSimpleSSE` adds simple-mode option projection, reasoning
clamping and synchronous credential admission. Automatic/WebSocket transport,
session reuse and OAuth account-pool integration
are covered by the later WebSocket and pooled native ports described below.
The existing pooled CodexProvider remains for legacy callers.

24 Codex WebSocket continuation fixtures verify cached delta selection, request
setting and input/output prefix identity, key-order sensitivity, empty deltas,
ignored old previous-response IDs and invalidation on mismatch. 16 additional source fixtures verify session-cache ownership, reuse, busy-session
isolation, idle/age expiry and explicit closure. A concurrent Go test checks
connection completion races and stale-release ownership.

22 Codex WebSocket parser fixtures cover terminal events, early closes, error
precedence, close metadata, binary UTF-8/BOM, cancellation and idle timeout.
Go concurrency checks verify queue delivery and cancellation wake-up. The native
Gorilla WebSocket adapter now attaches the parser per request and reads throughout
the connection lifetime. Local real-socket tests cover cached reuse, binary frames,
early close, idle timeout, cancellation and blocked HTTP-upgrade cleanup. 110 source fixtures now cover internal request orchestration, first-event start,
callback failures, unfinished-tool rejection, output and continuation ownership.
A local real-socket test verifies that the second request sends only its input
delta with previous_response_id and retains the full request for the next turn.
19 original-stream fixtures verify retry/fallback decisions and session failure
memory. The internal policy is wired to request/cache ownership and verifies a
fresh socket and one start event after continuation retry. The public StreamCodexResponses and StreamCodexResponsesSimple entry points now
share request preparation across both transports. Local lifecycle tests verify
reuse, fallback, sticky session bypass, explicit SSE and a single payload callback
per logical request. Transport diagnostics retain structured error and phase
details but do not reproduce JavaScript stacks. Malformed URL/JSON diagnostics and timer overflow edge parity remain unproven.

Codex debug counters now record requests, created/reused connections, full/delta
context, transport failures and SSE fallbacks. Request and policy source fixtures
include these snapshots. Public reset clears counters and fallback memory without
closing sockets; socket cleanup preserves both. Go tests verify detached snapshots,
concurrent counter updates and separate reset/close ownership.

The host native dispatcher now accepts openai-codex-responses and uses the Go
simple-mode combined transport. NativeAgent closes sockets for the nonempty
Codex sessions it used after requests settle. A real WebSocket host test verifies
per-request credential refresh, two-account socket isolation and cleanup while
preserving debug counters. Native runner Codex pool binding and fallback ladders
are integrated as described below; other OAuth vendors still need migration.

Codex failures now retain HTTP status/retry headers separately from Pi message
JSON. The internal pool error bridge preserves the host typed-error contract and
403 concurrency exception; source SSE/simple fixtures still verify unchanged
wire messages. StreamCodexResponsesPooled now binds Acquire/Report to native
simple streams and rotates typed authentication failures only before visible
output. It preserves live partial/result identity through a shared payload lock,
keeps the existing attempt/credential-cycle guards and excludes callback failures
from auth classification. Tests cover rotation, quota, concurrency caps, repeated
tokens, callback errors and progressive output. The native runner now binds Codex tiers with a TokenSource to this pooled stream.
Model/endpoint checks remain enforced, getApiKey returns null without touching
the pool, and credentials stay outside JSON configuration/history. A full runner
test verifies 401 rotation, account reporting, final answer and clean native
artifacts. Public worker pool bindings and other OAuth vendors remain guarded
until separately integrated.

Native fallback uses the same resolved-rung configuration as legacy provider
construction. Each concrete binding owns its model, provider row, callbacks,
dispatcher, capabilities, context window and pricing knowledge. Credentials and
pool handles stay outside durable JSON. The built-in native runner constructs
the full configured ladder. Vendor-only custom refresh is rejected when distinct
rows/routes share a callback vendor; row-aware refresh binds each rung separately.
Explicit custom stream overrides and the worker path still reject configured
fallback.

The host tool catalogue is retained across rungs. A tool-disabled model receives
no tool declarations in its provider transcript and cannot execute hallucinated
calls; a later tool-capable model can use the original governed catalogue.
Explicit empty catalogues and host deny policies remain authoritative. Unsupported
forced tool choices are rejected during binding.

Native credential refresh binds provider row, vendor and endpoint for each
parent, child and auxiliary-summary request. Known rows refresh from their
workspace book; rowless host configurations require an unambiguous route/key.
Changed or revoked bindings fail without falling back to another row's key.
Hosted defaults no longer inherit IDs from credential-less workspace selections.
HTTP retry/fallback/resume tests prove independent same-vendor key rotation; the
runner compaction test verifies the summary uses its own provider row.

A session owns its active rung and generation. Selection is journaled under the
session lease before any non-replayable provider event is released, then applied
to Agent state. Recovery validates all historical identities and generations;
changed model/endpoint bindings require explicit migration. Children receive
independent ladders at the primary rung and restore only their own journals.
Auxiliary summaries construct independent ladders with no business tools. Their
selection never changes the parent model. Both runner compaction and ChatService
summary calls bind refresh to the provider row.

Attempts use owner/rung/generation-scoped contexts for preparation, transform,
conversion, credential lookup, payload controls and dispatch. Tool callbacks use
only committed selection. Each attempt starts from a fresh native request view;
provider-only transforms do not replace tool context or persistent history.
Installed Go argument preparers/executors survive request cloning. Compaction
clamps the original host budget against the scoped model's window and reuses
saved summaries only when their native prefix matches.

The Go-only optional engine AdmitRequest callback lets the host return a selected
prepared context, model, thinking level and live stream before the loop consumes
provider events. A nil callback retains the pinned Pi request path. Native session
admission runs the ladder and returns as soon as selection commits, so live text
is not buffered until completion. The main runner uses this path for its supported
native bindings and forwards it to children. Explicit custom streams and the
worker path retain their existing adapter behavior.

Per-rung retries retain typed transport failures and Retry-After. Visible content
prevents replay/escalation. Host preparation, credential, observer and protocol
failures stop orchestration; a host callback's HTTP-shaped error is not a provider
failure. Cancellation drains the provider's terminal aborted message and usage,
without committing a speculative candidate or selecting another rung. Streams
share payload synchronization while retaining independent queues and live pointers.

Each provider attempt has one request trace with its model, row, generation and
pricing status; there is no duplicate outer logical-request trace. Every settled
attempt contributes usage. The final terminal is handed to message_end exactly
once, validated against provider content/usage with only the engine-owned
thinkingLevel annotation excluded. Discarded failures never enter conversation
history, and identical later responses remain separate billable attempts.

Tests cover real HTTP retry/escalation, credential refresh and pool ownership,
selection crash/lease failures, child isolation, request/engine comparison,
selected tool context and reasoning, live streaming, exact-once usage/traces,
and abort before/after selection. Full runner HTTP tests combine summary fallback,
parent escalation, tool execution, journal resume, independent child selection,
and selection-write failure before effects. The pinned source checks still verify
64 loop and 38 stateful Agent fixtures. Integration scope and evidence are tracked
in [native-fallback-plan.md](native-fallback-plan.md); this does not establish full
provider parity or complete the broader Go migration.
