# AgentRay vs oh-my-pi

Source reviewed: [`can1357/oh-my-pi`](https://github.com/can1357/oh-my-pi), commit
`62a4aa98a4b52f829a3ae9a5247ca8db4e5f810c` (2026-09-18). The repository is
MIT licensed. This review compares behavioral architecture rather than raw
feature count: AgentRay must remain deployable as one Go service on a laptop or
as an isolated multi-tenant server runtime.

## Architecture and technology

oh-my-pi is a Bun/TypeScript monorepo with Rust native acceleration, plus
Python and web packages around its hosted worker. Its important boundaries are:

- `packages/agent`: stateful agent loop, append-only context, compaction,
  replay policy, stream rules, speculative execution, pause control, and
  telemetry.
- `packages/ai`: provider registry, wire adapters, dialect conversion,
  structured provider errors, auth broker/rotation, provider session state,
  and retry policies.
- `packages/catalog`: generated provider/model capabilities and policy data.
- `packages/coding-agent`: session orchestration, tools, MCP, LSP/DAP,
  persistent eval, browser/computer control, subagents/hubs, memory, skills,
  and terminal UI.
- `packages/natives`: Rust-backed edit, diff, shell, VCS, and filesystem paths.

AgentRay is a Go service with a Next.js control plane. Its equivalent boundaries
are deliberately smaller:

- `agentcore`: provider-neutral loop, plugins, durable session log, compaction,
  steering/follow-up, stream interception, retries/escalation, budgets, goals,
  tool idempotency, and subagent usage accounting.
- `ai`: OpenAI, Anthropic, Google and subscription OAuth wires, plus arbitrary
  OpenAI-compatible endpoints and live model discovery.
- `internal/runtime`: workspace model tiers, per-agent composition, credential
  resolution, provider construction, scheduling, and local/hosted policy.
- `tools` and `internal/runtime/toolregistry.go`: portable Go tools behind a
  workspace/sandbox boundary; local single-user mode can opt out of isolation,
  while hosted multi-tenant mode requires it.
- `internal/dataplane/store`: durable PostgreSQL control-plane and session
  adapters rather than a desktop-local session manager.

## Capability comparison

| Area | oh-my-pi | AgentRay | Decision |
| --- | --- | --- | --- |
| Loop and extension seams | Evented agent loop with capabilities and session orchestration | Small core plus Go plugin interfaces and runtime composition | Keep AgentRay's boundary; it is easier to isolate and host |
| Context durability | Append-only context; completed parallel results persist in completion order | Append-only session entries, atomic save points, assistant frames, pre-effect intent commits, completion journal with source-order recovery, renewable ownership/fencing | AgentRay combines earlier completion durability with deterministic transcript order and multi-replica safety |
| Backend conformance | Targeted agent/session tests plus provider/catalog conformance checks | Memory and PostgreSQL session stores previously had separate tests | Added one reusable suite for ordering, isolation, snapshots, batches, recovery, branches, and checkpoint windows; this also carries forward the earlier pi-harness review's strongest testing practice |
| Streaming recovery | Retries only before meaningful output commits | Previously retried after visible output | Adopted replay-safe commit boundary |
| Compaction | Provider-aware pruning, tool protection, cache-aware strategies | Window-aware summary compaction plus durable, protected, cache-aware batched pruning | Adopted deterministic pruning, warm-prefix protection, and a savings floor; aggressive shake remains optional future work |
| Providers | Very broad registry, model-scoped API dialects, structured in-band/HTTP failures, auth broker, session state | Four native families, model-scoped OpenAI Chat/Responses routing, OpenAI-compatible gateways, pooled OAuth, bounded conversation state, adaptive hint fallback, governed retry metadata, and replay-safe stream failures | Keep generic wires; normalize failure semantics centrally and add native families only where their behavior cannot be represented |
| Model capabilities | Generated, resolved model/compat records including output limits and reasoning dialect | Previously one provider-wide tools boolean | Adopted tri-state per-model capabilities, live discovery, output ceilings, and rung snapshots; keep wire-specific schema/thinking adaptation in adapters |
| Local inference | Ollama and other local endpoints, optional credentials | Compatible wire existed but runtime required a key | Adopted explicit optional-auth local vendors |
| Edit conflict safety | Hashline edits bind model-selected lines to a file snapshot | Exact/fuzzy replacement previously trusted only matching text | Adopted snapshot-bound edits; defer the full hashline grammar |
| Tool surface | Coding-heavy suite: AST, LSP, DAP, rich eval, GitHub, browser/computer, memory | Portable filesystem/edit/LSP/rich Python/JavaScript eval with governed host-tool/subagent calls, shell/browser/computer/web/MCP, plus product tools | LSP read paths, persistent dual-language eval, provider-native image results, and the eval bridge adopted; DAP remains a measured follow-up |
| Rich tool results | Typed text/image outputs, provider image budgets, content-addressed blob persistence, and MIME bundles flow to capable model wires | Tool contract was text-only | Added an optional rich-result seam, bounded eval MIME capture, per-wire image budgets, server-side content-addressed persistence, and explicit degradation for text-only paths |
| Native acceleration | Rust edit/search/shell/VCS layer | Go implementation and external sandbox | Measure before adding native code; server simplicity wins today |
| Pause/step | Process-level pause plus session controls | Per-run step gate | Keep per-run scope; a global pause is unsafe for multi-tenancy |
| Speculative execution | Harness-native safe speculation | Parallel tool chunks and async jobs | Consider read-only turn speculation only with cancellation/cost evidence |
| Catalog/policy | Generated catalog and KDL policy data | Live model lists plus small compatibility/window tables | Adopt generated capabilities when provider breadth makes it necessary |

## Improvements landed from this review

### Durable deterministic context pruning

The default compactor now runs a zero-LLM pruning stage after the transcript
crosses half of its effective context budget. It replaces obsolete tool output
only before the keep-recent window: superseded identical calls first, file
reads invalidated by later writes second, then bulky old results. Tool-call IDs,
names, order, and adjacency stay intact. Error results, loaded skill bodies, and
consumer-declared non-repeatable/artifact tools are protected from generic
age-based pruning. Spill-backed previews may be reduced, but their opaque
recovery reference is preserved in both message metadata and the replacement
notice, so archived output remains reachable.

When prompt caching is actually active for the current model rung, candidates
deeper than an 8k-token suffix are left in the warm prefix for full compaction
to reclaim; cheap tail rewrites still proceed. Explicitly unsupported caching
does not arm the guard. Generic age-only candidates are applied only when their
aggregate saving reaches the configured floor (20k tokens on large windows,
adaptively capped at 10% for smaller local models). Superseded and stale results
remain the higher-confidence path and bypass that savings floor.

Unlike the pre-plugin `contextedit.go`, this pass is an optional capability of
the active `Compactor`; replacing the strategy replaces its pruning policy too.
The loop applies `BeforeCompact`, one durable start/completion bracket, and one
observer rebase around the combined prune/summary rewrite. A pruning-only pass
therefore survives restart through `Retained`, while a transcript still over
budget feeds the already-pruned form into one summary call. Provider usage
observations remain immutable billable facts; a separate signed context-token
adjustment subtracts only removed message bytes while retaining system and tool-
schema overhead that the byte fallback cannot see. The implementation is pure
Go and uses only transcript data, so laptop and multi-replica server runs have
identical policy.

### Replay-safe streaming

`agentcore` now treats the first content token delivered to a stream sink as a
commit boundary. A transient error before that boundary retains same-rung retry
and model fallback. An error after it neither retries nor escalates, because a
replacement answer would be appended to already-rendered output. The wrapped
provider cause and partial usage are preserved.

This is intentionally narrower than “any delta”: accumulated tool calls have
not been shown or executed, so they remain safe to replay before visible text.
Streams governed by interceptors are held until the attempt is accepted, so a
rule that matches a later chunk cannot leak an earlier prefix into the visible
stream ahead of its retry. Ordinary streams remain token-by-token live.

### Provider failure normalization and credential concurrency

The OpenAI-compatible and Anthropic streaming adapters now recognize provider
errors carried inside an HTTP 200 SSE response. Nested and flat envelopes,
explicit auth/permission/429/5xx fields, known auth/throttle/overload codes, and
leading proxy status frames become one structured `ProviderError`;
account/billing limits remain terminal. This closes the failure mode where a
rate-limit frame with no choices was skipped and the stream ended as a
successful empty answer. Live streams must also reach a provider terminal event
(or an explicit finish reason on compatible protocols) before tool calls and
final success are released. Anthropic requires `message_stop`; Antigravity
requires a candidate finish reason. A failure after visible content still uses
the existing commit boundary and is never replayed.

Retry timing now takes the longest valid value from `Retry-After-Ms`,
`Retry-After`, `X-RateLimit-Reset-Ms`, and `X-RateLimit-Reset`, including
relative and epoch reset forms. The full hint reaches the OAuth pool so an
account is blocked for the provider's actual window; the agent's own sleep
remains capped by its retry policy. HTTP provider error bodies are capped at 64
KiB, while successful model output keeps its normal transport semantics.
Specialized Responses, Codex, and Antigravity in-band errors retain the same
HTTP retry/reset headers instead of reconstructing a lossy error.

A configured credential refresh failure now fails closed before the provider
request rather than silently using a stale key. Static OpenAI, Anthropic, and
Responses keys, plus the provider wrapper, synchronize official update/read
paths because parent and subagent runs can share provider instances on a server.
Identity and tracing decorators expose key rotation only when their wrapped
provider actually supports it, so an OAuth pool cannot be mistaken for a static
credential. Empty refresh results fail closed just like resolver errors. OAuth
vendors retain their stronger per-request clone design. These contracts use only
the Go HTTP/runtime layers, so local compatible endpoints and hosted
multi-replica workers follow the same error and concurrency rules.

Pooled OAuth calls now also recover inside the logical request. A typed 401 or
403 is reported to the account pool, then retried with a refreshed or sibling
credential only when that credential has not already served the operation.
Chat and pre-output stream failures share the same distinct-credential set and
64-attempt hard ceiling. Stream metadata is held until the attempt is accepted;
switching accounts resets account-scoped provider-session state before the new
wire call. Once a content token is visible, the account is still reported but
the failure is forwarded without replay. A 403 that explicitly identifies a
concurrency cap is neither rotated nor marked bad; it returns to the ordinary
same-rung backoff path. Cancellation is surfaced as an error rather than a
closed channel that could be mistaken for an empty successful turn. Conversely,
once the stream's terminal success event has arrived, a sibling cancellation
cannot erase that completed result; this keeps durable fan-out recovery from
re-running a child whose answer won the completion race.

### Model output dialect and ceilings

`ModelCapabilities` now carries an optional hard output-token ceiling. OpenAI-
compatible discovery accepts dedicated and capability-map spellings,
Anthropic/Google listings retain limits when supplied, and the active rung
clamps only an explicit request above the discovered ceiling. A zero request
still means “use the provider default,” so unknown local endpoints keep their
existing behavior and fallback rungs shape independent copies.

The OpenAI-compatible `Compat.MaxTokensField` setting is now real wire policy:
it selects `max_tokens` or `max_completion_tokens` and is exposed through the
normal provider `Spec` construction path. Previously the field was documented
but every request always serialized `max_tokens`. An explicit 400/422 rejection
also switches to the alternate field and remembers that per endpoint/model
provider session, covering workspace runtime calls where no static catalog row
exists. Antigravity honors a smaller configured request limit while retaining
its 64,000-token Claude and 65,536-token Gemini safety ceilings. These rules are
metadata/configuration, not a generated catalog, so the same local or hosted
provider can publish its own limits.

### Provider-safe tool schemas and Antigravity thinking

AgentRay now keeps two schema roles separate. The `ToolSchema` supplied by a
tool remains the canonical execution-time contract, while each adapter sends a
deep-cloned provider projection. OpenAI Responses and Codex rewrite `oneOf` to
their accepted `anyOf` form, normalize empty subschemas, drop unsupported regex
lookarounds, and ensure every object node declares `properties`. OpenAI Chat and
Anthropic receive cloned/defaulted generic schemas without stricter rewrites.

Antigravity uses a Cloud Code Assist projection: known snake-case schema keys
are canonicalized, unsupported meta/validation fields are removed (useful
constraints are retained in descriptions), `const` becomes a typed enum,
nullable properties become optional, and object properties are made explicit.
A reference or residual combiner that cannot be widened predictably falls back
to an open object on the provider wire. The original schema is never mutated,
so local argument validation still rejects values outside the tool contract.
This mirrors OMP's canonical-schema/provider-projection boundary without
copying its TypeScript schema engine or making a laptop depend on a server-side
catalog.

Antigravity reasoning effort is no longer a boolean alias. Gemini 3 requests
map `minimal`/`low`/`medium`/higher levels to the native thinking-level enum;
budget-based Gemini/CCA routes map the same neutral efforts to bounded token
budgets. When the caller sets an output limit, the budget is added above the
desired visible output and capped at the existing 65,536-token Gemini or
64,000-token Claude ceiling. Empty/`off`/`none` preserves thinking-off behavior.
Anthropic thinking activation remains deferred, but its replay prerequisite is
now present: the neutral transcript preserves signed and redacted reasoning
blocks durably across tool turns without mixing them into visible assistant
text. Activation is the next adapter step rather than a transcript redesign.

Tool routing is now provider-neutral as well. A request or agent may leave the
provider default untouched, allow automatic selection, forbid tools, require
some tool, or force one advertised name. OpenAI Chat, Responses, Codex,
Anthropic/Claude Code, and Antigravity translate that contract into their native
wire shapes; an absent control emits no new field for strict local gateways.
The optional parallel-call hint remains tri-state and maps to
`parallel_tool_calls` or Anthropic's inverted `disable_parallel_tool_use`.
AgentCore validates named choices against the filtered tool catalogue before
network I/O. A provider known not to support tool choice can still enforce
`none` by removing tools locally, while forced choices fail early and may
escalate to a capable rung. Local execution remains separately opt-in through
`ParallelTool`, so a provider hint cannot make a mutating tool concurrent.

### Per-tool strict generation without weakening execution safety

`ToolSchema.Strict` now carries a provider-neutral tri-state policy: omit the
vendor field by default, request strict generation explicitly, or explicitly
send `strict:false`. OpenAI Chat, Responses, and Codex place that policy in
their respective wire shapes. AgentRay-owned runtime and sandbox tools opt in;
host/MCP tools retain their declared policy. Strict projection operates on a deep copy,
normalizes the supported union dialect, strips generation-only constraints,
requires explicitly closed object shapes, and emits `strict:true` only when every declared property
is already required. Explicit or implicit open maps, optional properties, unconstrained
branches, and malformed required lists degrade to the ordinary non-strict
projection instead of silently changing the arguments a tool accepts.

This is intentionally more conservative than OMP's required-and-nullable
rewrite. AgentCore validates execution against the original schema, so forcing
an absent optional field to appear as `null` on the wire would create arguments
that the canonical validator may reject. Provider strictness remains a model
quality hint; complete local Draft 2020-12 validation remains the safety
boundary.

OpenAI-family adapters recognize only narrow 400/422 strict-tool/schema or
grammar rejection signatures, including statusless errors inside HTTP-200 SSE
when their message matches that narrow classifier. Before any output is visible they retry once
without every strict field, then remember that endpoint/model incompatibility
inside the bounded logical `ProviderSession`. Later turns bypass the rejected
feature, while another model, endpoint, or conversation can still use it. No
session is required for correctness, auth/rate-limit failures are never
weakened, and the state owns no transport, which keeps the same behavior viable
in both an embedded laptop process and a leased server session.

### Durable Anthropic reasoning replay

OMP's Anthropic adapter preserves `thinking` blocks with their signatures and
opaque `redacted_thinking` payloads because Anthropic may require the exact
blocks again when a tool turn continues. AgentRay previously discarded them,
which made enabling extended thinking unsafe: a response could look correct on
the first turn and then fail signature validation on the next request.

`agentcore.Message` now carries provider-neutral `ReasoningBlock` values beside
visible content. Non-streaming and streaming Anthropic paths capture only
complete signed/redacted blocks; `streamTurn`, session snapshots, cache-prefix
comparison, compaction sizing, faux/replay providers, tracing, and Lab replay
all preserve them. The PostgreSQL trace adds an idempotent JSONB column with an
empty historical default. Lightweight metrics queries deliberately exclude the
payload. The laptop `MemorySessionStore` needs no migration and deep-clones the
same field, so both deployment modes retain identical transcript semantics.

Replay is bound to a digest of provider identity, endpoint, and model. The
endpoint itself is not persisted, and a block from another endpoint/model is
dropped rather than forwarded. Unsigned thinking is also dropped. Capture is
bounded to 32 blocks, 1 MiB per block, and 4 MiB per response; an oversized
opaque block is discarded whole rather than truncated into an invalid
signature or ciphertext. Reasoning deltas never become user-visible stream
tokens.

This lands the durability prerequisite only. Anthropic thinking budgets/
adaptive mode and the selective strict-tool beta stay disabled until their
request dialect and pre-output invalid-signature fallback are implemented and
tested together.

### Durable tool intent and parallel outcomes

oh-my-pi emits and persists each parallel tool result as the call completes.
That protects a fast call while a slow sibling is still running, but stores
results in completion order. AgentCore previously waited for the whole group so
it could append the model's source order, leaving completed effects in memory.

AgentCore now commits the assistant tool-call message before execution and
fails closed if that intent cannot be made durable. Every settled physical call
then writes a bounded `EntryToolOutcome` side record immediately. The canonical
tool messages still commit atomically in source order; after a crash, recovery
uses journaled outcomes without replay and materializes them beside the issuing
assistant message in that same source order. Parked and cancellation-derived
results are not promoted to completed facts, canonical results supersede side
records, and each outcome names its exact intent anchor so interleaved writers
cannot accidentally attach it to another branch. Outcome writes retry an
immediate transient failure independently of progress telemetry.

This works through the existing in-memory and PostgreSQL session adapters, so
the semantic contract is identical on a laptop and server. Distributed resume
ownership and append fencing are described below.

### Renewable resume ownership and chained asks

The ask/resume path now retains a separate durable-session identity on every
continuation row. A run `R2` that resumes `R1` still writes and later reopens
`R1`'s event stream; if it parks on a second question, the answer route no
longer looks in an empty `R2` log. The predecessor is finalized after a
successful continuation even when that continuation parks again, leaving one
unambiguous waiting row.

Resumes use the optional `SessionLeaseStore` contract. The in-memory backend
serializes one session with a process-local lock, preserving the zero-service
laptop path. PostgreSQL uses a renewable 30-second lease keyed by durable
session, with an owner UUID, monotonically increasing epoch, and database-clock
expiry. Lease identity is carried in an internal context marker: an answer
handler may hold ownership across answer append and resume, while nested runner
and AgentCore layers reuse the same proof without a public boolean bypass.

Every append made by the resumed worker validates owner, epoch, and expiry
under a row lock in the same transaction that assigns sequence numbers and
inserts the batch. A stale replica therefore cannot append after takeover, and
renewal loss cancels provider/tool work. External steering/follow-up writers
remain intentionally lease-free side-record producers; they serialize through
the existing per-session append lock and cannot impersonate the resumed owner.
Answers are call-keyed and idempotent: an exact retry does not duplicate the
entry, while a different answer for the same call fails closed. Tool
idempotency keys remain the last defense for an external system that commits an
effect at the exact moment ownership is lost.

### Laptop/server local-provider parity

The OpenAI-compatible wire now omits Authorization when the key is blank.
Ollama, LM Studio, llama.cpp, vLLM, LocalAI, and LiteLLM are explicit
optional-auth provider identities. Workspace readiness, hosted-model fallback,
runtime admission, model listing, chat/stream, and embeddings all accept those
providers without manufacturing a credential. A supplied key is still sent for
secured server installations.

The UI exposes the major local/self-hosted engines directly. Arbitrary
`openai-compat` gateways still require a key, so an unlabelled remote endpoint
does not silently become unauthenticated.

### Provider-complete fallback rungs

A tier fallback is now a complete provider/model rung instead of merely a
second model name on the primary provider. It may select another configured
provider row and therefore receives that row's base URL, credential or OAuth
pool, provider adapter, and derived context window. The legacy same-provider
model shorthand remains compatible and still reuses the primary client.

Fallback remains inside the selected quality tier: failures do not silently
promote a cheap task to a more expensive tier. This separates resilience from
quality policy while allowing a laptop to fall back from a cloud model to a
local engine, or a server deployment to fail over between independently
credentialed providers.

### Typed model capability negotiation

The provider-neutral contract now describes native tools, tool choice,
reasoning effort, image input, structured output, prompt caching, and stateful
responses as tri-state values: supported, unsupported, or unknown. Unknown is
deliberate—arbitrary gateways and local engines keep the request behavior that
worked before rather than losing features because their `/models` response is
sparse.

OpenAI-compatible discovery reads explicit capability booleans, capability
maps, supported-parameter lists, and input modalities. The chosen primary and
fallback model snapshots are persisted with the workspace tier, then carried
through provider reconstruction into the exact runtime rung. An explicit
`tools: unsupported` therefore skips that rung without spending a provider
request and lets the existing fallback ladder try a capable model. Explicitly
unsupported reasoning, structured-output, or prompt-cache hints are removed
for that rung; local output-schema validation remains active.

This keeps capability data deployment-neutral: laptop-local Ollama/vLLM and
server-hosted gateways follow the same discovery, persistence, and request-
shaping path. It also avoids importing oh-my-pi's large generated model catalog;
adapter defaults plus live facts remain the source of truth.

### Provider session state and adaptive endpoint lessons

`agentcore` now carries a provider-private state bag on each model request,
separate from the durable run log. `internal/runtime` leases that bag from a
bounded, idle-expiring registry keyed by workspace, project, agent, and logical
conversation, so a new Agent instance on the next chat turn retains provider
lessons. Active leases are never evicted; overload temporarily exceeds the
soft capacity rather than closing a transport underneath a request.

The state contract distinguishes account-scoped values from endpoint-scoped
ones. When a pooled OAuth provider changes account, only values that explicitly
implement the account-reset seam are cleared. Deployment lessons remain warm.
This follows oh-my-pi's credential-rotation rule and prepares the correct
lifecycle for server-side response chains without pretending that AgentRay's
Chat Completions wire has a `previous_response_id`.

The first concrete state consumer is the OpenAI-compatible adapter. A 400/422
that explicitly names `reasoning_effort`, `prompt_cache_key`, or
`response_format` as unsupported is replay-safe because it arrives before any
output: the adapter removes only that optional hint, retries immediately, and
remembers the successful demotion for that model/session. An arbitrary 400—and
specifically an invalid JSON schema—is never weakened. Structured-output
fallback remains safe because agentcore still validates the final text locally.

The registries are deliberately in-process and owned by one explicit runtime
resource bundle. This ownership matters because HTTP handlers construct a
short-lived `Runner` per request: without the shared bundle, provider sessions,
persistent eval kernels, and retained LSP clients all vanished after one turn.
A laptop now benefits across local chat turns, while every server replica
shares bounded warm state across HTTP, scheduler, and Lab paths and closes it
during shutdown. A replica miss merely relearns state, so correctness does not
require sticky sessions or shared mutable provider state.

### Public OpenAI Responses chaining

`openai-responses` is a first-party provider alongside the existing `openai`
Chat Completions wire. The runtime also separates OpenAI provider identity from
model wire, as OMP does with `model.api`: an ordinary `openai` row whose stored
or discovered model metadata explicitly reports `stateful_responses:supported`
uses Responses for that model while retaining the `openai` identity for key
refresh, tracing, and policy. Unknown or unsupported metadata stays on Chat
Completions, so existing gateways and privacy expectations do not change.

The adapter maps the neutral transcript onto
Responses input items, streams text and function calls, normalizes cached-token
usage, supports reasoning effort and JSON-schema output, and advertises native
stateful-response capability. Primary and fallback models on the same OpenAI
row may select different wires; they share credentials and base URL but not a
stateful client. Live collection routing, runtime ladders, and connectivity
tests all honor the same positive-only selection rule.

When a logical provider session is available, the adapter retains the last
successful canonical wire input, canonical assistant output, and response id.
It sends `previous_response_id` plus only the new suffix when the next full
request has an exact structural prefix and identical controls. History edits,
compaction, option changes, missing state, or an API-key fingerprint change
force a complete replay. A stale, expired, or unsupported response id retries
once with full context before any output; three consecutive stale chains trip a
session circuit breaker. Account reset clears response handles without erasing
that endpoint lesson.

Calls on one conversation chain are serialized through terminal stream
completion so overlapping server requests cannot race the baseline. Different
conversations and models remain parallel. Without retained state, the adapter
sends `store:false` and the full transcript; this makes laptop restarts and
server replica misses behaviorally correct rather than affinity-dependent.

### Snapshot-bound file edits

`read_file`, `write_file`, `edit_file`, and `edit_lines` now return a compact 64-bit tag
derived from the file's raw SHA-256 digest. `edit_file` requires the latest tag
and refuses the mutation when any byte changed since the model read the file,
even when its `old_string` still happens to match. The tag includes BOM and line
ending bytes, is revalidated immediately before writing, and works over both
the direct laptop filesystem and the server sandbox substrate.

This borrows the core lost-update guarantee from oh-my-pi's hashline editor
without importing its Rust parser, tagged-line syntax, recovery heuristics, and
native build chain. The short tag is 16 hexadecimal characters instead of four:
four hex characters have roughly a 50% collision probability across 300
snapshots, while the 64-bit form makes accidental collision negligible. A
fully transactional guarantee against arbitrary external filesystem writers is
not portable through ordinary file APIs; the final revalidation narrows that
race to the last read/write pair.

### Hashline-style multi-hunk edits, without a native parser

oh-my-pi's hashline format combines a four-hex whole-file tag with original line
coordinates. Its high-value behavior is not the textual grammar itself: it is
the ability to apply several distant replacements, insertions, and deletions in
one call without making the model echo old source text or recalculate shifted
line numbers after every hunk.

AgentRay now exposes that behavior as the typed `edit_lines` function tool. A
call carries the latest 16-hex `content_hash` and up to 100 operations:
`replace`, `delete`, `insert_before`, `insert_after`, and `append`. Every line
coordinate addresses the original read snapshot. The tool rejects bad bounds,
overlapping ranges, duplicate insertion gaps, and insertions inside a modified
range; it prepares all hunks before writing and rechecks the raw file hash at
the write boundary. BOM and the existing line-ending convention survive the
edit. The same Go implementation runs above `workspaceFS`, so direct laptop
I/O and server-side sandbox I/O have one contract and test suite.

The JSON operation array is intentional. It remains an ordinary function tool
for Anthropic, OpenAI Chat/Responses, compatible gateways, and small local
models, while avoiding a tolerant patch-language parser in every provider path.
The existing `edit_file` remains available for exact/fuzzy single replacements.
Not ported yet are syntax-tree block selection, registers/moves, parser recovery,
and indentation repair; those should earn their complexity through evals rather
than by package parity.

### Portable read-only LSP

oh-my-pi's LSP subsystem is a mature coding-agent feature: root-marker and
binary discovery, persistent clients, diagnostics freshness, multiple servers
per file, navigation/refactors, write-through formatting, and an optional
shared mux. The central behavior worth adopting first is semantic read access:
compiler diagnostics and symbol-aware navigation catch errors and callsites
that grep cannot.

AgentRay now exposes a configurable `lsp` function tool with diagnostics,
document symbols, hover, definition, and references. Like oh-my-pi, it accepts a
1-based line plus a symbol substring (and `#N` occurrence) and performs the LSP
UTF-16 conversion itself. Results are capped; diagnostics that
do not arrive before the freshness wait are reported as unknown rather than a
false clean result.

The retained transport now correlates responses per request, drops late and
unknown responses without queue backpressure, and serializes physical writes
through one cancellation-aware pump. Server requests remain live while idle:
workspace configuration/folders, dynamic capability registration, progress,
refresh, and headless UI cases receive protocol-correct responses, while
unsupported methods receive `-32601`. Dynamically registered document
diagnostics are honored.

Diagnostic freshness follows the same conservative principle as oh-my-pi:
newer versioned publications cannot be overwritten by late older ones, while
unversioned streams must go quiet before their latest value is accepted. A
pull response is clean only when it is a valid `full` report with an empty item
array; `unchanged` without a cached `resultId` baseline remains unknown.

Shutdown now has the same proof boundary as oh-my-pi: only process-exit
confirmation permits resource cleanup or a same-key replacement. AgentRay
keeps shutdown itself bounded; a backend that does not exit after force-kill is
represented by a closing tombstone until its waiter eventually completes. This
prevents both a stuck registry call and two copies of one conversation's
language server running concurrently.

The host path/URI boundary also handles Windows drive and UNC forms explicitly,
while rejecting remote authorities on Unix and credential-bearing file URIs.
That keeps the local-laptop contract genuinely cross-platform without changing
the fixed `/workspace` path used by isolated server containers.

Supporting this correctly required an optional `ProcessSandbox` capability in
the agentcore boundary. `HostSandbox` and `DockerSandbox` both implement the
same live stdin/stdout contract. Docker reuses the exact hardened envelope from
buffered execution: no network, dropped capabilities, resource limits,
read-only workspace mount, isolated temporary home, and an operator-selected
image containing the server. Host mode runs the configured laptop binary with
an allowlisted environment. A bounded host-owned registry now retains one
initialized client per tenant/project/agent/conversation/workspace/server
configuration, serializes its calls, refreshes the target overlay from guarded
workspace bytes, clears stale diagnostics, and evicts only idle clients. It is
process-local, so a server replica miss starts clean without sticky routing or a
cross-tenant mux. Configuration and deployment examples live in
[`LSP.md`](LSP.md).

Mutation operations were deliberately not copied yet. Rename, file rename,
formatting, and code actions should land only with a snapshot-checked cross-file
transaction, so an LSP workspace edit cannot partially overwrite concurrent
changes.

### Persistent Python and JavaScript eval

oh-my-pi's eval stack retains Python and JavaScript runtimes, streams rich
display output, bridges tools and subagents into cells, backgrounds long work,
and includes speculative execution. The foundational behavior is smaller: one
cell per call, retained state keyed to a logical session and working directory,
exclusive execution, explicit reset, bounded output, and no replay after an
ambiguous kernel failure.

AgentRay now implements that foundation as a configurable `eval` tool with
self-contained Python 3 and Node.js runners. A bounded host-owned registry
carries language-separated kernels across the short-lived tool instances built
for consecutive chat turns.
Keys include tenant/workspace, project, agent, conversation, workspace path,
runtime, and config fingerprint; idle/capacity eviction is explicit, and a
replica miss simply starts clean. Python exceptions preserve mutations made
before the error, while cancellation, timeout, transport failure, and protocol
failure discard the kernel without replaying the cell.

The runner separates NDJSON control frames from fd 1/2, so Python and child
process output cannot spoof the protocol or fill an unbounded host buffer. It
supports final-expression display, `display(...)`, top-level `await`, rich
Python MIME representations, and automatic matplotlib PNG rendering. Markdown,
JSON, HTML/SVG/LaTeX fallbacks remain visible as bounded text; validated
PNG/JPEG data becomes typed message parts. Cells are capped at eight images and
768 KiB of decoded image data, with explicit notices for invalid or omitted
content.

JavaScript now uses Node's REPL evaluator for persistent lexical bindings and
native top-level await. A syntax-aware scanner rewrites static ESM imports while
leaving import-looking strings, templates, comments, and regular expressions
untouched. Node 22's dependency-free transform executes TypeScript cell syntax,
including enums and parameter properties, without pretending to type-check it.
A synchronous loader hook applies the same transform recursively to explicit
`.ts`/`.mts` ESM imports.
Console/process output, structured JSON, final values, and Buffer/typed-array
image displays enter the same bounded NDJSON and rich content paths as Python.
Kernels and resets are isolated by language. On POSIX laptops and Linux server
containers, protocol traffic travels on a dedicated descriptor while ordinary
stdout/stderr—including inherited child output—is redirected away from it.
Timeouts force-discard the Node process without replay, preserving AgentRay's
side-effect ambiguity rule. A small `Dockerfile.eval` ships Node 22 plus Python
3 for reproducible hosted use; local mode resolves the operator's installed
runtimes.

The next OMP slice is now adopted without copying its desktop trust model.
JavaScript cells expose an awaitable callable/property `tool` proxy, and Python
cells expose synchronous `tool(name, args)`. The Go host does not hand either
kernel a raw `ToolSet`: it installs a run-owned `ToolInvoker` only while a live
Agent dispatches the outer eval call. Each nested request re-enters
`Agent.runToolCall`, so schema validation, policy and hooks, credential
resolution, output/image bounds, interceptors, idempotency, cancellation,
circuit breaking, and durable traces are identical to direct calls. Directly
constructing EvalTool has no authority to call host tools.

Nested results stay inside the eval value and do not synthesize provider tool
messages, preserving call/result adjacency. Their traces and control metadata
are nevertheless carried in the outer bounded durable outcome and restored on
resume. JavaScript supports concurrent calls for tools that explicitly opt into
parallel execution and deterministic request-order accounting; Python is
synchronous. Recursive active-tool calls are rejected,
parked human-input tools are direct-only, a per-cell cap complements the shared
run cap, and `MaxToolCalls` now reserves atomically before every physical call—
closing the pre-existing race where a parallel batch could exceed the limit.
Cancellation gets a bounded settlement window so cooperative nested failures
remain auditable without letting a tool that ignores context defeat the cell
deadline. Floating JavaScript rejections are attributed to the cell rather than
crashing its retained kernel.

The optional `RichTool` contract is additive: existing `Tool` implementations
and text-only consumers do not change. Rich parts are snapshotted by durable
sessions, included in cache-prefix identity and context pressure, preserved for
capable escalation rungs, and removed with an explicit breadcrumb during
pruning/compaction. OpenAI Chat hoists tool images after the complete tool-result
batch to preserve call/result adjacency; Responses and Codex emit native input
image items; Anthropic nests images inside `tool_result`. Explicitly text-only
model paths receive an omission notice, while unknown capability metadata stays
optimistic for backward compatibility.

The full outgoing request is also bounded at the provider seam, not merely each
tool result. Known wire families advertise conservative limits (Anthropic and
Claude Code 90, OpenAI/Responses/Codex and Google 200, OpenRouter 90); live model
metadata may override them. Unknown image-capable endpoints use a portable floor
of five. If a transcript exceeds the active rung's budget, the oldest images are
removed copy-on-write with an explicit per-message breadcrumb. Escalation can
therefore retry the untouched canonical transcript against a rung with a larger
budget instead of inheriting the first provider's loss.

Large images no longer multiply inside PostgreSQL JSON session rows. The server
adapter externalizes base64 payloads of at least 1 KiB into the existing
session-fenced spill table under a content-derived locator and stores only that
reference in the log. Repeated content deduplicates, reads validate the fence,
size, MIME signature, and completeness, and hydration restores the original
provider-neutral message before agentcore sees it. Missing/corrupt artifacts
remove only the affected image and leave a visible notice, so one bad artifact
cannot make a run permanently unresumable. The in-memory laptop store remains
dependency-free and inline. This is a PostgreSQL representation optimization,
not yet raw-byte object storage: the artifact row currently holds base64 text.

Before any rich image becomes canonical history, the loop now decodes it rather
than trusting a file signature, corrects mislabeled MIME metadata, rejects more
than 16 megapixels or 16 MiB of compressed input, and normalizes it with a
portable pure-Go pipeline. The longest edge is capped at 1568 px, degenerate
short edges are raised to 200 px, PNG and white-composited JPEG encodings compete
for the smallest result, and quality/dimension ladders target at most 500 KiB
while respecting the 768 KiB aggregate tool-result budget. A resize adds an
explicit coordinate-mapping note for screenshot-style tools. WebP is decoded
but converted, avoiding a common llama.cpp/local-backend incompatibility. This
rebuilds OMP's `Bun.Image` policy without adding a platform-specific runtime or
external image process to either laptop or server deployments.

Host mode receives only allowlisted environment variables; Docker mode reuses
the hardened no-network process envelope with a writable workspace and
read-only root. Configuration and lifecycle details live in [`EVAL.md`](EVAL.md).

Raw-byte/object-store backing, parser-backed JSX/TSX and local-module reload
semantics, OMP's background handle/work-pool API, timeout pausing around host
calls, and speculative eval were not copied in this increment.
Those features need native AgentRay cancellation, artifact, and delegation
contracts rather than a direct desktop-runtime port.

### One session contract for laptop and server backends

The earlier pi-harness review's reusable storage suites, reinforced by
oh-my-pi's targeted concurrency and session tests, highlighted a deployment
risk that feature tests alone do not cover: two stores can satisfy the same Go
interface while disagreeing on ordering, aliasing, batch visibility, or
resume-window safety. AgentRay now runs one reusable `internal/agentcoretest`
contract against the always-available `MemorySessionStore` and, when
`AGENTRAY_TEST_DATABASE_URL` is set, the PostgreSQL adapter used by the server.

The suite proves session isolation, total ordering under concurrent appends,
atomic and contiguous batches under competing writers, typed side-record
round-trips, branch filtering, completed-outcome recovery, reverse-completion
parallel recovery in canonical source order, exactly-once reattachment, and
equivalence of a checkpoint window to the full fold. It also pins the two cases
that must defeat a windowed resume: a branch and an unsettled pre-checkpoint
inbox item.

The shared contract found a real laptop-only bug: the memory store copied only
the outer `SessionEntry`, leaving messages, usage records, retained transcript,
checkpoint state, raw question bytes, and tool outcomes aliased to caller
memory. A caller could mutate an already-appended log through its original
value or a `Log` result, while PostgreSQL remained immutable through
serialization. The memory backend now deep-snapshots on both writes and reads,
so append-only means the same thing on both deployment substrates.

`make test-session-conformance` runs the portable contract (and the PostgreSQL
half when its test URL is present), while `make bench-session` tracks allocation
and throughput for immutable batch appends, 100/1,000/10,000-entry folds, and a
checkpoint-window read over a 10,000-entry memory log. The benchmark is a trend
guard, not a cross-backend contest: the in-memory window intentionally scans,
whereas PostgreSQL answers the checkpoint and suffix through indexes.

The first measured fold exposed avoidable tree-building cost on ordinary linear
logs. `ActivePath` now validates parent continuity and ID uniqueness in one
forward pass and returns that path directly; only a real rewind, fork, duplicate
ID, or malformed parent pays for the general leaf-to-root tree reconstruction.
The branch algorithm remains authoritative, while the common laptop/server
resume path allocates far less on long histories. On the same Apple M1 Pro run,
the production-shaped 10,000-entry fold moved from about 6.7 ms / 29.5 MB
allocated to 1.7 ms / 10.1 MB; these are directional local measurements, not a
portable CI threshold.

## What not to copy

- A Bun/TypeScript/Rust port would add three operational toolchains and duplicate
  working Go seams. Borrow contracts, not implementation language.
- A process-global pause can stop unrelated tenants. AgentRay's gate must stay
  run-scoped.
- Desktop-native shell/edit behavior cannot bypass the hosted sandbox policy.
  The same tool interface may use a direct laptop backend or isolated server
  backend, but authorization and workspace path checks stay above both.
- Provider/model string conditions spread through runtime code do not scale.
  New differences should enter through `ai` capabilities or provider adapters.

## Next adoption order

1. Enable Anthropic budget/adaptive thinking and selective strict-tool support
   on top of the durable signed-block representation, with a bounded pre-output
   fallback that drops rejected stale signatures rather than weakening an
   already-visible turn.
2. Benchmark typed `edit_lines` against oh-my-pi's syntax-block hashline mode;
   add syntax-aware blocks only if they materially improve edit success.
3. Evaluate raw-byte/object-store backing for server artifacts, then evaluate
   transactional rename/code actions over snapshot-checked multi-file writes.
4. Measure an aggressive transcript-shake policy; keep it optional because
   artifact durability and the acceptable loss profile vary by deployment.
5. Evaluate read-only speculative execution and background eval handles behind
   a budget and cancellation gate; keep mutating tools strictly replay-safe.
6. Expand native provider families only where the generic OpenAI-compatible
   wire cannot represent required behavior.

The target is behavioral parity where it improves correctness and agent quality,
not package-for-package parity. AgentRay should remain one portable harness with
different deployment backends, rather than separate laptop and server agents.
