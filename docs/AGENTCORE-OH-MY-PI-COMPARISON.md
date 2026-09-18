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
| Compaction | Provider-aware pruning, tool protection, cache-aware strategies | Token/window-aware compaction with tool/result guards | Add cache-aware pruning and richer strategies later |
| Providers | Very broad registry, dialects, auth broker, session state | Four native families, OpenAI-compatible routing, pooled OAuth, bounded conversation state, adaptive hint fallback | Keep generic wire; add dialect contracts and Responses chaining incrementally |
| Model capabilities | Generated, resolved model/compat records | Previously one provider-wide tools boolean | Adopted tri-state per-model capabilities with live discovery and rung snapshots |
| Local inference | Ollama and other local endpoints, optional credentials | Compatible wire existed but runtime required a key | Adopted explicit optional-auth local vendors |
| Edit conflict safety | Hashline edits bind model-selected lines to a file snapshot | Exact/fuzzy replacement previously trusted only matching text | Adopted snapshot-bound edits; defer the full hashline grammar |
| Tool surface | Coding-heavy suite: AST, LSP, DAP, eval, GitHub, browser/computer, memory | Portable filesystem/edit/LSP/eval/shell/browser/computer/web/MCP plus product tools | LSP read paths and persistent Python eval adopted; add DAP and richer eval incrementally |
| Native acceleration | Rust edit/search/shell/VCS layer | Go implementation and external sandbox | Measure before adding native code; server simplicity wins today |
| Pause/step | Process-level pause plus session controls | Per-run step gate | Keep per-run scope; a global pause is unsafe for multi-tenancy |
| Speculative execution | Harness-native safe speculation | Parallel tool chunks and async jobs | Consider read-only turn speculation only with cancellation/cost evidence |
| Catalog/policy | Generated catalog and KDL policy data | Live model lists plus small compatibility/window tables | Adopt generated capabilities when provider breadth makes it necessary |

## Improvements landed from this review

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

`openai-responses` is now an explicit first-party provider alongside the
existing `openai` Chat Completions wire. It maps the neutral transcript onto
Responses input items, streams text and function calls, normalizes cached-token
usage, supports reasoning effort and JSON-schema output, and advertises native
stateful-response capability. The explicit provider identity avoids silently
changing existing gateways or opting an existing workspace into OpenAI's
server-side response retention.

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

### Persistent Python eval

oh-my-pi's eval stack retains Python and JavaScript runtimes, streams rich
display output, bridges tools and subagents into cells, backgrounds long work,
and includes speculative execution. The foundational behavior is smaller: one
cell per call, retained state keyed to a logical session and working directory,
exclusive execution, explicit reset, bounded output, and no replay after an
ambiguous kernel failure.

AgentRay now implements that foundation as a configurable `eval` tool with a
self-contained Python 3 runner. A bounded host-owned registry carries the
kernel across the short-lived tool instances built for consecutive chat turns.
Keys include tenant/workspace, project, agent, conversation, workspace path,
runtime, and config fingerprint; idle/capacity eviction is explicit, and a
replica miss simply starts clean. Python exceptions preserve mutations made
before the error, while cancellation, timeout, transport failure, and protocol
failure discard the kernel without replaying the cell.

The runner separates NDJSON control frames from fd 1/2, so Python and child
process output cannot spoof the protocol or fill an unbounded host buffer. It
supports final-expression display, `display(...)`, and top-level `await`.
Host mode receives only allowlisted environment variables; Docker mode reuses
the hardened no-network process envelope with a writable workspace and
read-only root. Configuration and lifecycle details live in [`EVAL.md`](EVAL.md).

JavaScript, MIME images, cell-to-tool/subagent bridges, auto-backgrounding, and
speculative eval were not copied in this increment. Those features need native
AgentRay cancellation, artifact, and delegation contracts rather than a direct
desktop-runtime port.

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

1. Benchmark typed `edit_lines` against oh-my-pi's syntax-block hashline mode;
   add syntax-aware blocks only if they materially improve edit success.
2. Add rich-display/artifact support to persistent eval, then evaluate
   transactional rename/code actions over snapshot-checked multi-file writes.
3. Make compaction prompt-cache-aware and add transcript shake/pruning policies.
4. Add per-model wire selection so an OpenAI provider row can choose Chat or
   Responses from discovered/catalog metadata without changing provider identity.
5. Evaluate read-only speculative execution behind a budget and cancellation
   gate; keep mutating tools strictly replay-safe.
6. Expand native provider families only where the generic OpenAI-compatible
   wire cannot represent required behavior.

The target is behavioral parity where it improves correctness and agent quality,
not package-for-package parity. AgentRay should remain one portable harness with
different deployment backends, rather than separate laptop and server agents.
