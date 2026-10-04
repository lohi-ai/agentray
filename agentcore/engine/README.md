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

`Run`, `Continue` and `AgentEventStream.Result` return `*MessageList`.
`Turn.NewMessages`, `Turn.ToolResults`, `Event.Messages` and `Event.ToolResults`
also retain live collections. A saved `NewMessages` sees later turns append to
the same list; slot/length edits survive into the returned result and terminal
event. Context history has separate membership, with shared message objects.
Each turn owns its tool-result list. For error/aborted responses, Pi constructs
a separate empty list for `turn_end`, so hook edits to that turn's `ToolResults`
do not change the event list. `Values()` adapts these collections to native slices.

Another 360 original-source cases compare membership, sparse slots, lengths,
item mutations and identity through finish/next/request hooks, turn/end events,
return and settlement in low-level Run/Continue and the stateful Agent. They
include two-turn accumulation, prepared messages and error/aborted exits.
Twelve stream-wrapper cases verify that `Result`, repeated result reads and
the queued terminal event share the same list after mutation.

`Run` and `AgentLoop` accept a live `*MessageList` prompt. `Agent.Prompt` also
accepts that list, alongside text, individual messages and native slice inputs.
Native slices copy membership; use `MessageList` when callbacks retain and edit
the input array. With unchanged tool declarations, initial message events iterate
the original list with its current length on each step. Context and result
membership were already copied before those events, so later prompt appends can
appear in events and Agent history without entering the provider's context.
When tool declarations require an inserted or rewritten system message, initial
event iteration instead uses the newly declared list.

`GetSteeringMessages`, `GetFollowUpMessages` and `TurnUpdate.Messages` also use
`*MessageList`. Pending/prepared lists remain live through next-turn preparation,
steering re-polls and `turn_start`; the loop then spreads their current membership
into a new collection before emitting messages. Changes after that spread do not
alter its membership, while edits to shared messages remain visible. Another
594 original-source cases compare these boundaries, including append, replacement,
shrink/truncation, local variable reassignment, nested edits and post-settlement
edits in Run, Continue and the Agent wrapper.

`Context.Messages` and `Context.Tools` are live `*MessageList` and `*ToolList`.
Transformation and conversion callbacks receive and return `*MessageList`.
Retained references observe appends, slot replacements and length changes;
replacing a context field detaches its previous collection. Run copies message
membership through a dense spread; Continue shares its input history. Agent
admission copies both lists, preserving sparse slots and shared item objects.
Another 504 original-source cases compare these behaviors across request,
transform, convert, credential, stream, tool and turn callbacks and settlement.
Six cases verify that default conversion filters sparse/custom-role entries
into a new dense list while retaining selected message objects.

Lists also expose enumerable own data properties through `GetProperty`,
`SetProperty`, `DeleteProperty` and `PropertyKeys`. Clearing history during a
partial stream preserves Pi's subsequent assignment to the ordinary `"-1"`
property without growing the list. Thirteen oracle cases cover key order,
non-index names, truncation, JSON and slice behavior. An invalid own
`constructor` fails cloning; two Agent cases verify the failure event sequence,
idle settlement and a successful subsequent prompt. Custom JavaScript
prototypes, accessors and Symbol.species constructors have no native mapping.
Provider normalization still projects a value snapshot at the `StreamFn`
boundary, after transformation, conversion and credential hooks settle.

`FinishTurn`, `PrepareNextTurn` and `PrepareNextTurnWithContext` receive a
shared `*Turn`. Each completed turn allocates a new object, which remains live
for its next-turn callback and any retained references. Replacing `Message`,
`ToolResults`, `Context` or `NewMessages` updates that object without rebinding
the loop's own message/context/result variables. A returned `TurnUpdate.Context`
explicitly selects a replacement context. Reassigning a callback's local turn
pointer remains local. Concurrent edits to retained turns require synchronization.
Another 471 original-source cases compare these identities and field replacements
in Run/Continue and the Agent wrapper through finish, turn-end, steering,
next-turn, request and post-settlement callbacks, including error/aborted exits,
nil fields and explicit context adoption. Host callbacks still receive serialized
snapshots at their transport boundary; a nil turn context serializes as `null`
without dereferencing it in the adapter.

`NewAgent` constructs the stateful wrapper. `Prompt`, `Continue`, `Steer`,
`FollowUp`, queue mode/clear/preview methods, `Abort`, `WaitForIdle`, and `Reset`
follow Pi's lifecycle. State is reduced before subscribers run. Subscribers
are awaited in subscription order, including `agent_end`: a second prompt,
continuation or reset is rejected until the last end subscriber settles.
Run failures produce the original assistant error/aborted event sequence;
admission errors and failures in that recovery sequence are returned to callers.

Continuation admission retains Pi's distinction between the wrapper and the
low-level loop. Agent rejects an absent/null tail or an all-system history before
starting; its `every` scan skips holes and stops at the first non-system message.
The low-level loop only validates length and the tail role. Replay later visits
every slot, so a hole and an explicit null fail at the same lifecycle stage as
the pinned source, with its corresponding error text. Admission failures leave
queues intact; failures after queue selection preserve the already-consumed state.
Another 408 original-source cases cover sparse/null histories, both queues,
edits during `agent_start`, constructor failures and a subsequent successful run.
Six cases verify synchronous stream-wrapper validation. Another 102 compare
system-prompt reads, state JSON and reset, including queue/history preservation
when replay fails. `SystemPrompt()` panics on those source getter failures;
`MarshalJSON()` and `Reset()` return errors. These indexed reads do not invoke an
array constructor; admission's later context clone still does.

The Go API uses `State()` snapshots for scalar state and explicit setters.
Its `Messages` and `Tools` fields retain live `*MessageList`/`*ToolList`
collections, and `StreamingMessage` retains the current event object.
`Get`, `Set`, `Append`, `Delete`, `SetLength`, `Has` and `Keys` operate on
those collections. `Values()` returns a detached dense slice of shared items;
`Clone()` preserves sparse slots and drops named properties, following the
default Array.slice constructor behavior. Container operations are synchronized;
concurrent message/tool field edits still require caller synchronization.
`SetMessages`/`SetTools` copy native slices; `SetMessageList`/`SetToolList`
copy live collections, including holes. Replacing a collection detaches old
retained lists; mutating a list through a state getter remains visible to the
agent. Model JSON should be treated as immutable.
`State.SystemPrompt()` replays that view's messages when called, and JSON state
export includes the computed string. Simply obtaining a state view does not
eagerly replay an incomplete history.
`PendingToolCalls` is an immutable `*ToolCallSet` with `Len`, `Has` and
`Values`. Repeated getters share the set until a tool start/end, reset or run
settlement replaces it. Even a duplicate start or an unmatched end creates a
new set; retained sets preserve their previous insertion order and membership.
Direct JSON serialization produces `{}`, matching Pi's Set. The host transport
explicitly exports `Values()` as an array, preserving its existing protocol.
`Listener` pointers provide stable subscription identity without a new
interface. The live listener set supports removal/addition during delivery.
`Configure` replaces wrapper options atomically. Most callbacks are captured
when a run starts; an installed next-turn preparation wrapper reads its latest
callback, matching Pi. Context-aware preparation takes precedence over the
legacy signal-only preparation callback.

The loop and wrapper retain shared `*ai.Message` objects across contexts,
turns, queued messages, lifecycle events and results. Initial/replaced histories
copy the caller's pointer slice and preserve its message objects. A callback
editing a retained message also changes that message in `context.messages`,
`newMessages`, `toolResults`, subsequent requests and the wrapper's history.
Assistant start/update events retain Pi's shallow top-level copy behavior.
`State()` retains the wrapper's live collections; provider normalization projects values only
after transformation, conversion and credential callbacks have settled.
`MessagePointers` adapts existing value slices, and `MessageValues` creates
top-level value snapshots. `Prompt` also accepts individual message pointers or
pointer slices; `Steer`, `FollowUp` and `SetMessages` use references directly.

Twenty-eight original-source cases compare constructor/setter copies, retained
list identity, slot replacement, append/delete/grow/shrink, sparse assignment,
item mutation and reset. Ninety run cases compare edits before/during/after a
prompt, context snapshot isolation, live history publication and streaming
message identity for terminal-only and partial/update streams. State edits made
after admission do not replace already-snapshotted context slots; edits to their
shared message/tool objects remain visible. Native race checks exercise
concurrent list reads, cloning and appends. The outer `State` value still
snapshots scalar fields; this is not a claim of full JavaScript
state-object/array identity everywhere.

Another 135 original-source cases compare pending-tool set identity, membership,
insertion order and direct JSON through two awaited subscribers, tool hooks,
execution updates and settlement. They include duplicate/empty IDs, ID mutation,
sequential and deterministically ordered parallel completion, missing/invalid/
blocked/truncated calls, abort and subscriber failures. The Go set exposes Pi's
public read-only contract; mutation through a JavaScript type cast, prototype
extensions and native JavaScript iterator objects are not represented.

Another 392 pinned-source cases cover sparse/null tool lists, invalid own array
constructors, programmatic overrides/default context tools, and mutations at
assistant completion or tool start. Tool declaration checks constructor/species
before mapping, skips holes during mapping, and rejects holes when forming the
declaration lookup. Tool lookup visits holes as undefined and stops at the first
match; its failures reject the run rather than creating an `isError` result.
Execution-mode selection stops at the first sequential tool, and truncated
responses bypass selection entirely. The fixtures compare errors, execution,
requests and ordered events in both batch modes. In `RunToolCall`, nil tools use
the context list; an explicit empty list disables all tools for that call.

Ninety-four additional original-source cases compare object identity, complete
events, requests, returned messages, original prompts and wrapper history.
They cover retained-pointer edits in finish/end/next/request/transform/convert/
credential/steering callbacks and edits through context, new-message and tool
result lists, including error and aborted turns. The earlier twenty-one loop
and twelve Agent mutation cases still cover pending/prepared messages, both
tool modes, partial/result-only streams and recovery. Native slice convenience
inputs copy membership; mutable content-block collections and provider
stream-object aliasing remain outside this coverage. Message edits belong to awaited callbacks; callers must
synchronize concurrent access to shared payloads or retained pointers.

Go contexts carry the active abort signal and parent values. `Abort` or parent
cancellation cancels it. Successful settlement detaches from the parent without
cancelling a saved subscriber signal. Cancelling a `WaitForIdle` wait only stops
that wait. Hooks/listeners must not synchronously wait for the run they belong to.

Tool lists use `*ToolList` in `Context`, `State` and `RunToolCall`, and
`[]*Tool` in `InitialState` and `SetTools`. The wrapper copies assigned membership while retaining each
tool object. Preparation selects that object before argument preparation and
before-call hooks: mutating its executor changes execution, but replacing the
list entry cannot switch an already-selected call to a different tool. This
also keeps external edits visible after the wrapper copies or grows its lists.
Sixty-seven original-source cases compare selected executors, returned results
and retained/current tool identity across argument preparation, before-call
hooks, list replacement/growth, sequential/parallel calls, direct `RunToolCall`,
initial Agent tools and `SetTools`. Live context and wrapper state collections
are covered above.

Tool preflight is ordered. Parallel tools finish independently and emit end
events in completion order; result messages are appended in original call
order. Any sequential tool serializes the whole batch. Every finalized result
must request termination to stop the batch. Truncated assistant messages never
execute their tool calls. Provider errors and aborts remain hard exits.
`BeforeToolCall.Args` and the argument passed to `Tool.Execute` hold
`*Object`, `*Array`, or a primitive value. The engine builds the validated graph
directly from decoded values; Before, execution and After retain the same object
graph. Nested edits through `Object.Set/Delete` and `Array.Set/Append/SetLength` remain visible
across these callbacks without schema revalidation. Replacing a hook's
`Args` field or the executor's local argument stays local. Replacing a nested
property detaches its old value from that property; retained references still
point to the old value. `ToolCall.Arguments` stays the separate raw input used
in lifecycle events and traces.

Seventy-two original-source cases distinguish argument edits from field
replacement in before/execute/after/update callbacks, including primitive
arguments. Another 54 cover nested object edits, array growth, child replacement
and edits through retained references during updates and after settlement in
programmatic, sequential and parallel modes. Earlier validation fixtures still
cover numeric/object edits and required-key deletion. Callers must synchronize
concurrent access to shared values. The concrete containers and JSON
codec live in `internal/jsonjs`, shared with telemetry; no interface hierarchy
or runtime TypeScript dependency is introduced. Host callbacks serialize the
graph only at their JSON transport boundary. The shared `Array` also retains
enumerable own named properties through `GetProperty`, `SetProperty`,
`DeleteProperty` and `PropertyKeys`; these fields do not enter array JSON.
An own `toJSON` property containing an explicit `JSONMethod` runs only on export,
with the receiver and containing field key; recording and cloning remain passive.
The details/structured-content serializers preserve those keys and omit undefined
hook results. Raw model arguments still use serialized JSON. Arbitrary object
prototypes and accessors remain outside this representation.

Validation now uses the shared JSON.parse value domain for both arguments and
schema strings. Its working values preserve binary64 overflow, negative zero,
signed underflow and lone UTF-16 surrogates. Union candidates use recursive
container copies, and successful validation constructs the ordered execution
graph directly; neither step serializes live values. Unconstrained nonfinite
values survive into hooks and tools, finite number/integer schemas reject them,
and string coercion produces `Infinity`/`-Infinity`. Numeric constraints use
finite binary64 comparisons; string lengths count Unicode code points, including
lone surrogates. Regex subjects use the same code points, and literal surrogate
patterns are lowered without changing their diagnostic spelling.

An additional 884 original-source cases compare argument JSON, in-memory
float64 bits before and during execution, validation messages (including their
UTF-16 contents before and after text-block export), and unchanged raw input.
They cover root/nested inputs,
unions, numeric bounds/divisibility, string bounds, literal/escaped/property
regexes, surrogate object keys, required/dependency errors, enums and array
checks. Object-field scanning preserves raw field values and distinct UTF-16
keys instead of decoding names through `encoding/json`. These cases extend
validation coverage; they do not prove the entire TypeBox schema surface or
all JavaScript object/prototype behavior.

`ToolResult.Details`, `ToolResult.StructuredContent` and `ai.Message.Details`
now carry live JSON-shaped values. Nested objects and arrays retain identity
through updates, after hooks, execution-end events and published tool messages.
An override copies the result's outer struct and keeps unchanged nested values;
replacing details detaches that field, while a content override removes stale
structured content unless it also supplies non-null structured content.
Use `NewObject`/`NewArray` and primitives for native construction. At these
optional fields, nil or `Undefined` means omitted and `Null` means explicit
JSON null; decoding preserves that distinction. Both null and undefined
overrides fall back to the prior value, including when it is false or zero.

Another 160 original-source cases cover shared and JSON-decoded graphs, retained
nested edits, field replacement, null/content/empty overrides, update callbacks,
events, history messages and edits after settlement in all three execution modes.
Sixty-six JSON-value cases compare exact result JSON, optional-field state and
in-memory number bits across missing/null/falsy/scalar/container/function inputs.
Execution does not serialize results: cycles and function values can remain live,
while explicit JSON export rejects cycles and omits function-valued properties.
Native checks verify passive export without user serialization methods.
Stream snapshots clone these graphs with memoized concrete containers, keeping
cycles/repeated references inside a detached copy and retaining sparse arrays,
nonfinite numbers and signed zero. The existing cross-event/provider identity
limitation of those snapshots is unchanged.

Tool-call hooks, preparation, execution and outcomes retain a `*ai.ContentBlock`
instead of copying it. Editing its ID/name/raw arguments affects later lifecycle
events and transcript publication; the already selected tool and validated
arguments stay selected. Replacing a hook's `ToolCall` pointer affects that hook
context only. `RunToolCall` takes a pointer and preserves it in its outcome.
Fifty-three original-source cases cover sequential, parallel and programmatic
calls, edits before/during/after execution, blocks/failures and later callbacks
editing an earlier call. Sequential messages already published keep their old
IDs; parallel messages use the call's fields at publication. Twelve cases replace
content slots or grow the list in before/after hooks: retained calls remain
separate from replacement blocks and keep their identity after growth.

`MessageContent.Blocks` and tool-result content use `[]*ai.ContentBlock`.
`Agent.Prompt` takes images as `...*ai.ContentBlock`: text prompts copy image-list
membership but retain each supplied image object, including repeated references.
Editing an image through the caller, an event, history or a request stays visible
through the other references. Replacing/appending entries in the caller's image
slice does not alter prompt membership. Images are ignored for message/list input.
A nil image argument becomes an explicit null content entry, not an array hole;
`ai.NullContentBlock` constructs that same sentinel for native callers.
The JSON host adapter constructs owned block references at its decoding boundary.
Another 240 pinned-source cases compare image identity and edits through admission,
events, transforms, conversion, provider, finish and settled state; nine cover
explicit null images and ignored image arguments.

`ai.BlockContent` constructs blocks from values; `ai.BlockReferences` keeps
existing objects. Provider accumulators and the proxy publish their block lists
directly, removing the per-event loop that copied every block. Three concurrent
stream checks also verify that `toolcall_end` and its partial transcript refer
to the same block. Eighteen original-source transformation cases verify which
blocks remain shared and which are copied for cross-model signature/ID changes
or image replacement, while keeping the input transcript unchanged. Retained
slice headers do not automatically track list-length changes; live provider
messages are still isolated by the engine's stream snapshots.

Within content arrays, nil block pointers represent holes and decoded null
blocks retain a distinct sentinel (`ContentBlock.IsNull`). Tool-call filtering
skips holes, including gaps reconstructed from proxy content indices, but rejects
explicit null entries with the pinned source's property-read error. Missing/null
content and string assistant content fail at the filter read; error/aborted
responses bypass this read as in Pi. `Message.HasContent` reads retained field
presence without serialization. Another 270 source cases verify these reads,
content replacement at message start/end, completion hooks, events and wrapper
failure cleanup. Thirty source-backed local HTTP proxy cases cover sparse text
and tool blocks across successful, truncated, failed and aborted responses.

System-prompt replay uses the same sparse/null distinction. `ContentText` skips
holes and throws the pinned property-read error for null entries or null/missing
content. State JSON and `Reset` return those errors through their Go error
boundary; failed reset preserves history and both queues. Replay retries timestamp
selection after missing/null values; a final missing timestamp with no tools
produces no baseline, while null defaults to zero. Another 201 source cases cover
prompt reads, state JSON, reset/recovery and timestamp selection; 36 compare the
text/render/replay helpers directly. Content absence travels with decoded content
through copies, and replacing it with explicit null makes the field present.

`ai.TransformMessageReferences` also preserves message/model identity while
transforming replay history. Its normalizer receives `*ai.Model` and
`*ai.Message`; `ai.TransformMessages` remains a value adapter for provider
conversion. Content normalization and image downgrade complete for the entire
history before any ID callback. Signature removal copies a tool call before
the callback, but ID comparison and result-ID mapping read the original call
afterward. Fifty-six source cases cover callback edits to IDs, call fields,
message fields/content, model identity/modalities and earlier/later messages,
checking output, mutated inputs, callback snapshots and object identity.

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

`Tool.Execute` returns `*ToolResult`; progress callbacks and `ToolOutcome.Result`
also use pointers. Without an override, the tool, after hook, end event and public
outcome share the same result object. Progress callbacks receive the actual
partial object, including nil, and retained pointers see subsequent tool edits.
Returning any after-hook override creates a shallow copy, even when the override
is the executed object itself. A nil execution result becomes Pi's caught null
property-read error after admitted updates settle. Thirty-seven original-source
cases verify these identities and their serialized events/results across
sequential, parallel and programmatic calls. Callers must synchronize concurrent
access to retained result pointers.

Awaited `tool_execution_end` sinks receive the finalized result object before
transcript construction and batch termination. Mutating its content, metadata or
`terminate` field affects the subsequent message/request and continuation.
Replacing the event's result pointer or `isError` field affects only that event;
the outcome's previously computed error flag stays unchanged. When the after
hook returns no override, its retained result pointer sees the same event
mutations; an override creates a separate result, matching Pi. Eighteen source
scenarios cover sequential/parallel execution, immediate blocks, truncation,
termination changes, pointer replacement and retained after-hook results. Two
Agent subscriber scenarios additionally verify state snapshots, next provider
requests and termination through the stateful wrapper.
Twelve further scenarios cover a later callback mutating an earlier retained
result, including immediate unknown-tool results. Sequential messages already
published are not rebuilt; parallel batches read the current result when each
message is constructed. Both modes read termination from the shared results
after message events finish, so a later callback can clear an earlier tool's
termination flag. The batch no longer stores a stale result snapshot alongside
the live result object.
Twenty-four additional scenarios preserve literal tool names in validation,
truncation and unknown-tool diagnostics, including quotes, backslashes, control
characters and Unicode; Go quoting no longer changes the diagnostic text.

Caught Go errors retain their identity. JSON-shaped panic values use Pi's
`Error.message` / `String(value)` error text, including null, arrays, objects,
numeric formatting and nonfinite values. A tool update callback's panic reaches
the executing tool synchronously and can be recovered there; otherwise it becomes
a tool error and runs the after hook. A returned update error represents a rejected
promise: execution settles, then the call rejects without the after hook. Tools
that start their own goroutines own the panic boundary in those goroutines.

Go integer/float widths are formatted as JavaScript numbers, including binary64
rounding, nonfinite values and signed zero. Typed slices and fixed arrays use
JavaScript array joining, and typed maps use the ordinary object string. Array
cycle detection tracks only the active join path: a self/mutual reference becomes
an empty element, while a shared child is included at every separate occurrence.
This prevents an error-reporting stack overflow on cyclic thrown arrays. Eighteen
additional oracle cases cover these Go representations and cyclic/shared arrays
through the Agent and all four tool failure stages.


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

Observation snapshots copy the typed Go graph directly; they do not serialize
and parse JSON while consuming an event or final result. Nonfinite numbers and
negative zero therefore retain their in-memory values, and unreadable extension
JSON cannot interrupt the loop merely by being copied. Public mutable fields
are detached; private, immutable transcript encoding metadata remains shared.
Snapshots preserve repeated message, content-block and usage references within
the detached graph. In particular, a `toolcall_end`
tool call remains the same block as its partial transcript entry; equal but
distinct source objects remain distinct. Eight original-source loop/Agent cases
check streamed block identities and full event/message JSON, including repeated
blocks and a detached tool-call object. Three native provider concurrency tests
check this relationship in both raw events and snapshots. Twenty-four additional
original-source loop/Agent cases check NaN, positive/negative infinity and signed
zero through progressive, terminal-only and ignored events, including exact usage
JSON. Usage exports nonfinite numbers as `null` and negative zero as `0`, while
retaining the original live numbers. Another 48 source-runtime cases check usage
decoding, sparse/null counters, nested cost, numeric metadata and subsequent
mutations. Go tests mutate all
populated public snapshot fields to check producer isolation and verify that
copying can succeed independently of explicit JSON export. Cross-event/provider
identity remains separated by observation snapshots; this is not full live
stream-object parity.

## Verification

```sh
make test-agentcore-native
bun agentcore/engine/testdata/generate-pi-fixtures.ts --check
bun agentcore/engine/testdata/generate-loop-stream-fixtures.ts --check
bun agentcore/engine/testdata/generate-agent-fixtures.ts --check
bun agentcore/engine/testdata/generate-message-reference-fixtures.ts --check
bun agentcore/engine/testdata/generate-tool-reference-fixtures.ts --check
bun agentcore/engine/testdata/generate-tool-definition-fixtures.ts --check
bun agentcore/engine/testdata/generate-argument-reference-fixtures.ts --check
bun agentcore/engine/testdata/generate-result-reference-fixtures.ts --check
bun agentcore/engine/testdata/generate-stream-alias-fixtures.ts --check
bun agentcore/engine/testdata/generate-stream-number-fixtures.ts --check
bun ai/testdata/generate-usage-json-fixtures.ts --check
bun ai/testdata/generate-block-reference-fixtures.ts --check
bun ai/testdata/generate-transform-reference-fixtures.ts --check
bun agentcore/engine/testdata/generate-proxy-fixtures.ts --check
bun ai/testdata/generate-json-stringify-fixtures.ts --check
bun ai/testdata/generate-json-prefix-fixtures.ts --check
bun telemetry/testdata/generate-status-json-fixtures.ts --check
bun agentcore/engine/testdata/generate-argument-fixtures.ts --check
bun agentcore/engine/testdata/generate-format-patterns.ts --check
bun agentcore/engine/testdata/generate-unicode-properties.ts --check
bun agentcore/engine/testdata/generate-failure-fixtures.ts --check
```

The Go tests consume checked-in fixtures and require no JavaScript runtime.
The optional fixture generator runs the unchanged upstream implementation after
verifying its source hashes. It compares complete JSON events, request contexts,
messages, error strings, and hook ordering. Concurrent scenarios use explicit
gates to make completion order deterministic in both runtimes. JSON object-key
ordering is not treated as semantic; message content strings remain exact.
There are 143 low-level loop scenarios and 52 stateful Agent scenarios. The
latter record state at every event and action, including queue previews,
subscriber failures, busy admission, reset baselines and forwarded controls.
Additional race tests exercise concurrent queue/state access, blocking end
subscribers, abort propagation, live subscription identity and callback changes.
Another 137 proxy scenarios compare retained events, final messages, and HTTP
request envelopes against Pi, including post-terminal mutations and tool object
identity. Terminal records preserve sparse/null usage, provider usage/cost
extensions, optional thinking-level/error-message presence, and nullable text
and thinking signatures. Every proxy fixture also compares detached snapshots
with the settled live payload. Go HTTP/race tests cover cancellation, concurrent engine reads, frames
over 64 KiB, and UTF-8 split into single-byte reads. Invalid JSON and transport
errors use Go's diagnostic wording; exact JS engine/fetch error text is not
claimed. Malformed frames outside the typed protocol are not fully equivalent.
Sixteen proxy failure scenarios verify transport/reader panics become terminal
error events, including null, primitive, array and object panic values. Cancellation
changes the stop reason to `aborted` while retaining the failure's message. Reader
panics still close the response body; they do not escape the producer goroutine.
Fifteen further proxy scenarios cover request serialization: panics and returned
errors from Go JSON marshalers settle through the error stream without making an
HTTP request. Nested `encoding/json` marshaler wrappers are removed so the
callback's original message survives. Cancellation during serialization records
`aborted` without replacing that message. Initialization captures model identity,
then the timestamp, then serializes the body, matching Pi even when clock or
serializer callbacks mutate request inputs. Snapshots retain the same terminal
payload as the live stream.

Another 33 cases cover proxy option serialization. Thirty compare the literal
HTTP body with Pi, including non-finite numbers as null, negative zero, binary64
rounding of native integer/float widths, nested values, unescaped HTML and line
separators, and the protocol's option/callback order. Map callbacks can replace
or delete later captured keys; newly inserted keys wait until a later export.
Repeated shared children serialize normally. Three cyclic graph cases verify
Pi's error text, no HTTP request, and no repeated callback after an object loses
a field during serialization.

Ordinary Go maps use deterministic lexical order for non-index keys because they
do not preserve insertion order; array-index keys use JavaScript ordering. The
fixed protocol option order is preserved independently. Go byte slices retain
Go's base64 representation. JSON emitted by raw messages and custom marshalers
is normalized after callbacks finish: numbers use binary64, duplicate keys keep
the last value at the first insertion position, index keys enumerate first, and
strings use well-formed JSON.stringify escaping. The normalizer reads UTF-16 code
units directly so lone surrogates and distinct surrogate object keys survive.
Twenty-two further end-to-end cases compare exact request bytes for these paths,
including overflow/underflow, duplicate escaped keys, mixed surrogate sequences,
and nested objects. A separate pinned-runtime oracle checks all 65,536 single
UTF-16 units and all 1,048,576 valid surrogate pairs using per-block SHA-256
hashes; Go tests recompute every output without a JavaScript runtime.
Struct-specific non-finite numeric inputs, exhaustive binary64 decimal-format
parity, insertion order already lost in Go maps, and other non-serializable-value
diagnostics remain unproven. UTF-16 wire checks here do not establish surrogate
parity for the rest of the message/validation/provider APIs.

Thirty-four source-generated failure values exercise the Agent failure lifecycle and
all four tool prepare/before/execute/after catch sites. Three update scenarios
compare synchronous throws, tool-caught throws and rejected update promises,
including continuation, after-hook admission and original error identity.
Seven pending-update scenarios compare successful waits, rejection before and
after execution settles, concurrent execution/update failures, and selection
among already-rejected updates.

Tool declarations now share `ai.StringifyJSON` with complete JSON parsing and
proxy requests. Valid parameter and constrained-sampling JSON is normalized
before publication, rather than copied byte-for-byte from RawMessage. The old
separate declaration serializer was removed. Twenty-four source-generated cases
verify declaration output, equality and tool-state changes; four loop cases
verify declaration insertion for distinct surrogate titles/property names and
no redundant insertion for equivalent rounded numbers or duplicate keys.
Malformed/absent schema error propagation still follows the existing Go behavior
and is not established as equivalent to Pi's dynamic JSON-round-trip failure.

Partial tool-argument parsing uses the same object-field serializer as complete
JSON parsing. Canonical JSON keys retain distinct lone UTF-16 surrogates, avoid
HTML/separator over-escaping, and preserve duplicate replacement and numeric
index ordering. The partial parser retains Pi's `__proto__` setter behavior;
complete JSON parsing retains that key as an own property. Another 848
source-generated prefixes compare exact serialized results for Unicode keys,
nested objects, repaired controls/escapes, numbers and malformed punctuation.
These cover Unicode scalar boundaries; arbitrary raw lone-surrogate input
strings and all malformed-input diagnostics remain unproven.

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
Completing the port also requires removing the TypeScript reference source,
fixture generators, development worker/bridge, and their Bun build/configuration
dependencies from the port's scope. Retain Go implementations, Go regression
tests, verified JSON fixtures, and upstream provenance/license notices. The
reference tree is temporary migration tooling, not the final architecture;
passing Go tests alone does not complete this cleanup or the default switch.
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
the existing Go JSON Schema library. Another 1812 source-generated programmatic
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

Format assertions now cover UUID, IPv4/IPv6, email and IDN email, date, time,
date-time, duration, URI/reference/template, JSON pointer/URI fragment/relative
pointer, and regex. The checks use the locked TypeBox expressions and date/time
rules, including leap-second offset normalization and NFC for IDN email. Another
113 source scenarios verify valid/invalid values, coercion, sibling error order,
unions, references and unknown formats. Unknown format names remain annotations.
Hostname and IDN hostname add 190 cases covering ASCII labels, punycode,
UTF-16/domain and encoded-label length limits, fullwidth/NFC/ignored-character
mapping, contextual joiners, Pi's bidi rules, and raw-decoder UTF-16 surrogate
pair joining. Raw RFC 3492 encoding/decoding
runs in Go; Pi's permitted punctuation and partial contextual checks are retained
instead of substituting a stricter IDNA policy. WHATWG URL-based `url`, `iri`
and `iri-reference` run through the pinned pure-Go whatwg-url parser and translated
TypeBox guards. Another 258 source cases cover relative references, Unicode hosts,
IPv4/IPv6, ports, file/custom schemes, whitespace, percent encoding, and IPvFuture
narrowing with the source's 2048 UTF-16-unit threshold. All 21 default format names
have native checks. Another 145 regex-format scenarios verify a native syntax
gate before the matcher: Unicode escape restrictions, capture references,
duplicate names across disjoint alternatives, scoped modifiers, assertion
quantifiers and class range endpoints. This rejects the legacy/.NET constructs
that regexp2's ECMAScript mode otherwise permits. The pinned oracle's acceptance
of non-ASCII identity escapes is retained. Another 42 matching cases cover named
and numbered capture ordering, nesting, forward/optional references, disjoint
duplicate names including repeated alternatives, and escaped/Unicode identifiers.
Named captures are lowered to preserve JavaScript's opening-parenthesis numbering;
diagnostics retain the original pattern. Another 101 tool-path cases verify
Unicode property syntax, aliases, binary properties, general categories, scripts,
script extensions, complements and mixed classes. Property sets use generated
Go data extracted from the pinned Bun 1.3.14 / Unicode 15.1 oracle, rather than
the Go toolchain's tables. `TestPiUnicodePropertyOracle` checks all 419 property
sets using 86,903 boundary/interior probes in four positive/complement/class
forms and compiles every one of the 1,627 accepted aliases. Generation enumerates
all 1,114,112 code points and guards the oracle runtime version. Unicode alias
inputs and their license are retained under `testdata/unicode-15.1/`.
Another 277 tool-path cases cover Unicode admission for hostnames, ACE labels,
IDN email, capture names and escaped name delimiters. Hostname category/script
checks and Unicode format patterns share the pinned property tables. Capture
names follow the observed JavaScriptCore grammar: Letter_Number and the
Other_ID_Start/Other_ID_Continue exceptions are rejected. The Unicode oracle adds
4,640 capture-name probes, each checked at the start and continuation positions
in literal and escaped form. Escaped `>` closes a capture name or named reference
in the pinned runtime; the scanner preserves that behavior and the following
pattern text. Direct property lookups also run against all 86,903 oracle probes.
Another 205 cases verify scoped dot-all and multiline behavior, including all
four JavaScript line terminators, inherited/disabled/restored modifiers, CRLF,
lookarounds, named captures and literal metacharacters. Dot and anchor lowering
uses the parser's effective group flags; the former unconditional dot-rewrite
pass has been removed.
Another 145 tool cases cover repeated capture resets, optional descendants,
duplicate names, bounded/lazy repetition, backtracking, lookarounds and empty
iterations. `TestPiRegexpRepetitionOracle` also checks all 364 strings over
`a/b/c` up to length five against 32 source patterns (11,648 comparisons).
The lowering keeps capture history at one value, resets descendants per
iteration, and uses backtrackable guards to reject optional empty iterations
while preserving mandatory ones. Lookbehind uses the same operations in reverse
execution order. Nullable-iteration guards now use backtrackable progress flags
set by consuming atoms, including nonempty backreferences. Consumption inside
a lookaround does not advance its enclosing repetition. The previous suffix/
prefix scans have been removed. `BenchmarkPiNullableRepetition` measures
`^(a?)*\1$` on 128–8,192 characters; on an Apple M1 Pro with three measured
iterations, the 8,192-character case improved from 160.8 ms to 2.4 ms per match
(about 67×). This is one focused benchmark, not a general regex performance bound.
Word escapes and boundaries use Pi's Unicode-mode word set directly. Scoped
ignore-case adds long s and Kelvin sign without admitting dotted capital I.
Word escapes inside classes become separate union branches with their own
folding policy; negated classes reject the union before consuming a scalar.
Word boundaries compare the same set on both sides, including inside lookbehind.
Thirteen tool-path cases check admission and original-pattern diagnostics.
`TestPiRegexpCharactersOracle` covers 71 patterns over 255 inputs (17,340
comparisons for the 68 valid patterns), including digit/space controls, mixed
classes, complements, scoped flags, lookbehind and nullable repetition. It also
checks both word sets over every representable Unicode scalar (2,224,128
comparisons), using independently enumerated source results. Regenerate with
`bun agentcore/engine/testdata/generate-regexp-character-fixtures.ts --check`.
These changes pin word membership; general literal/property case folding still
uses the backend and remains outside the established parity coverage.
Full WHATWG/ECMAScript syntax, case folding, exhaustive lone-surrogate format behavior,
arbitrary regex/backtracking edge cases and normalization/case-mapping parity
remain unproven; these fixtures do not establish full format parity.

The upstream MIT license is retained in `LICENSE.pi`; translated TypeBox format
expressions and rules retain their MIT notice in `LICENSE.typebox`. The development
pattern generator checks TypeBox 1.3.27 and emits Go literals. Native builds and
tests do not read or execute TypeScript or depend on `node_modules`.

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
143 loop and 52 stateful Agent fixtures. Integration scope and evidence are tracked
in [native-fallback-plan.md](native-fallback-plan.md); this does not establish full
provider parity or complete the broader Go migration.
