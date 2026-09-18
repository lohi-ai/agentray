# agentcore compared with pi agent

Source reviewed: [`earendil-works/pi/packages/agent`](https://github.com/earendil-works/pi/tree/main/packages/agent), commit `46c9de402bddf46b03c3b9f46487b777aaa41861` (2026-09-17).

## Decision

Keep agentcore as a flat, provider-neutral Go kernel with ejectable plugins.
Adopt pi's durable-runtime contracts and conformance-test discipline as Go
interfaces and reducers. Do not port its TypeScript harness or copy its folder
tree: agentcore already has equivalent or stronger composition, policy,
recovery, and plugin boundaries.

pi is MIT licensed. Any future direct code reuse must retain its attribution and
license; the work described here reimplements architectural ideas rather than
copying source.

## pi technology and architecture

- TypeScript ESM on Node 22.19+, TS 5.9/tsgo, Vitest 4, and Biome.
- Model abstraction in `@earendil-works/pi-ai`, telemetry in
  `@earendil-works/pi-telemetry`, and transport-neutral primitives in
  `@earendil-works/chord`.
- Public loop in `src/agent.ts`, `src/agent-loop.ts`, and `src/types.ts`.
- Durable harness split into typed session records, a serialized per-session
  lane, drive procedures, and a pure snapshot reducer.
- Built-in execution/tools, compaction and branch summaries, skills, prompt
  templates, telemetry, and proxy/RPC support.
- SQLite kept in a separate package, leaving the core free of native storage
  dependencies.
- Reusable storage/runtime conformance suites and session benchmarks.

## Comparison

| Concern | agentcore | pi agent |
|---|---|---|
| Provider seam | Narrow Go `LLMProvider` | `pi-ai` models plus `StreamFn` |
| Loop | `loop.go` + `turn.go` | `agent-loop.ts` + `agent.ts` |
| Composition | Explicit seams, plugins, and optional extension interfaces | Harness configuration, resources, and hooks |
| Tools | Permission gate, validation, idempotency, retries, circuit breaker, mixed parallel/sequential batches | Preflight/after hooks and parallel batches |
| Durability | Append-only tree log, checkpoints, side records, recovery reducer | Transactional operation state, lane serialization, entries/values/lists |
| Streaming recovery | Text frames and tool progress | Typed assistant-frame encoder/reducer |
| Compaction | Model-window budget, pinned goals, deterministic fallback | Summary and branch-summary operations |
| Tests | Boundary, unit, stress, plugin, and integration suites | Strong generic storage/runtime conformance and benchmarks |

agentcore already covers the basic pi agent API's important behavior: steering
and follow-up queues, parallel tools, compaction, retry/escalation, usage and
cost accounting, prompt caching, structured output, skills, durable branches,
parked human questions, and plugin isolation.

## Adopted now

The first durability increment is an optional `SessionBatchStore` contract:

- a turn's settled transcript entries commit atomically and in order;
- side records remain immediately appendable while a turn is in flight;
- legacy `SessionStore` implementations retain ordered-prefix behavior;
- the memory and Postgres stores implement atomic batches;
- Postgres serializes all writers per session, so a side record cannot split a
  save point;
- a reusable conformance suite checks ordering, concurrent sequence assignment,
  and batch contiguity;
- `make test-agentcore-race` makes the concurrency gate explicit.

## Next increments

1. Persist completed parallel-tool outcomes separately from their source-order
   transcript placement, preventing replay of already-finished effects after a
   crash.
2. Add a first-class operation state (`starting`, assistant pending, tool
   pending, retry wait, terminal) reduced from durable facts.
3. Add repository/catalog behavior above `SessionStore` for list/open/delete
   and branch ownership; keep backend implementations outside the kernel.
4. Preserve structured thinking and tool-call blocks in reconnect frames if the
   UI needs richer mid-turn hydration than text snapshots.
5. Split `loop.go` into same-package lifecycle, recovery, durability, and
   streaming files only when the current uncommitted work settles. The flat
   package is an intentional encapsulation boundary; new folders would export
   machinery without creating a useful independent package.
