# oh-my-pi → AgentCore → Soot

Inspected upstream commit: `898b09d32f147887a2242cf5ec9a1967bcac8873` (2026-10-04).
This comparison uses source, not README feature claims. Upstream is MIT, with
copyright notices for Mario Zechner, Can Bölük and Stencil Labs. The working
checkout is outside the product tree; no TypeScript runtime is being added.

## Comparison and recommendation

| Capability | oh-my-pi at the inspected revision | AgentCore today | Decision for Soot |
| --- | --- | --- | --- |
| Memory | Exclusive `off/local/hindsight/mnemopi/sharpshooter` backends. Local memory consolidates rollouts and persists learned lessons; local structured search is explicitly unavailable. Backend hooks stage recall before committing it to a session. | Scoped automatic and explicit `memory_recall`, `learn`, soft curation, and bounded rollout consolidation. A native distiller proposes lessons/merges; the host atomically checks snapshots, retains revision history and consumes pending rollouts. | **Implemented in Go plugins + Soot Bolt adapter.** Consolidation failures retain pending evidence for retry; no Bun or external memory service is required. |
| Compact | Ordered `remote → snapcompact → handoff → shake → soft` methods; native provider compaction and vision bitmap archives are optional strategies. | Native `host.Compactor` summarizes a completed prefix into a request view, preserves original native messages and checks a checkpoint digest. Model windows cap the budget. | **Reuse** the native compactor. Keep checkpoint and summary in the consumer's completion transaction. Defer image archives and remote compaction: they change provider contracts and need separate fidelity/cost evidence. |
| Advisor | Multiple configured reviewers, turn or agent-end review, intervals, bounded backlog, quota cooldown, deduplication and severity-aware admission. | Bounded pre-finish and periodic turn-boundary review, up to four named reviewers, independent quotas/cooldowns, bounded evidence and checkpointed emission guards. Native review accounts all attempts. | **Implemented.** Soot configures review every four turns (three periodic reviews) plus two finish reviews. Reviews use the existing turn scheduler; there is no second watchdog loop. |
| Todo | Phased tasks, delta operations, blocked/abandoned states, persisted successful mutations, eager creation and bounded reminders. | Run-scoped phased items with stable IDs, blocked/abandoned states, compatible replacement `update_plan` and atomic delta `patch_plan`. One active step, bounded context, checkpoint state and successful-receipt recovery remain. | **Implemented additively.** Existing full-list callers still work; phases and IDs are optional. Delta batches validate the entire resulting plan before mutation; child plans remain isolated. |
| Goal | Structured active/paused/budget-limited/complete/dropped state, token and wall-time accounting, persisted lifecycle, activity-based continuation guard; create/get/complete/resume/drop tool operations. | Completion contract, stall breakers and audited revision plus opt-in active/paused/budget_limited/complete/dropped lifecycle, token/active-time accounting, explicit host commands and native checkpoints. | **Implemented.** Pause stops at a settled boundary with no synthetic model wrap-up. Resume retains spend and requires a larger grant when a goal cap is exhausted; completion follows all finish guards. Host lifecycle authority remains separate from model objective revision. |
| Jobs | Session job manager for bash/task/eval, running cap, owner-scoped visibility, delivery receipts/retry/dead letters, result retention and cancel/reap. | Run-owned async launcher, status/list/wait/cancel tools and completion notices. Jobs cancel with their owning run; existing Soot background cases are separately durable. | **Reuse/refactor** in-run jobs for granted asynchronous work. Bound concurrency and ensure cleanup/ownership. Keep durable Soot background tasks on the existing case queue; do not relabel in-memory jobs as restartable. |
| Subagent | Job-backed task execution with structured result/schema handling and owner-scoped cleanup. | Governed fork inherits capabilities, bounds depth/count, folds usage, validates/corrects structured answers. Native correction now resumes an opaque checkpoint. | **Reuse** fork/delegation, with fresh child plan state and unchanged Soot tool grants/effect fencing. |

Sources at the pinned commit:

- [Memory contract](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/memory-backend/types.ts), [local backend](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/memory-backend/local-backend.ts).
- [Compaction methods](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/session/compaction-methods.ts).
- [Advisor runtime](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/advisor/runtime.ts), [admission guard](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/advisor/emission-guard.ts).
- [Todo tool](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/tools/todo.ts), [tracker](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/session/todo-tracker.ts).
- [Goal state](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/goals/state.ts), [runtime](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/goals/runtime.ts).
- [Async jobs](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/async/job-manager.ts), [ownership controls](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/packages/coding-agent/src/async/job-control.ts).
- [License](https://github.com/can1357/oh-my-pi/blob/898b09d32f147887a2242cf5ec9a1967bcac8873/LICENSE).

## Applied integration

Soot's case worker and conversation service use `RunNative` with AI fallback.
Both supply config and host bindings to `preset.Full`; AgentCore owns plugin
composition and execution. Skills are passed as `AgentDefinition.Skills` and
loaded through AgentCore's `read_skill`. Soot's tool declarations now use
`agentcore.StringTool`; the former public `extension/tool` package is only a
source-compatible alias, with no separate decoder or dispatcher.

| Capability | Applied behavior |
| --- | --- |
| Goal | Completion gate and structured lifecycle on both compositions. Conversation `/goal pause|resume|status|drop|create` commands checkpoint without a provider request. New user tasks after complete/dropped start fresh accounting; paused/budget-limited goals require explicit resume. |
| Todo | Pinned phased plan, stable IDs and retired count in the native checkpoint, restored before hooks; atomic `patch_plan` and child isolation. Successful administrative turns refund the work-turn budget while tool limits still apply. |
| Memory | Host-owned Bolt adapter. Conversation memory is scoped to tenant/user/Soot; case memory to case/Soot. Bounded fuzzy recall (automatic and model-requested via `memory_recall`), learning, update/retraction and retained revision history. Completed runs stage bounded rollouts; native consolidation merges/retracts lessons in one scope-fenced compare-and-swap transaction, retaining failed batches for retry. |
| Compact/retrieval | Native compactor and original transcript checkpoint; `session_query` retrieves original history that request compaction hides. Multiple imported host facts can compact without summarizing a lone initial task prematurely. |
| Jobs/subagent | `spawn_subagent(async:true)` uses the core launcher and jobs plugin. Owner fencing, 15 running jobs per owner, wait/status/cancel and completion notices. Soot limits delegation to four children and one level; child workload effects/publication are denied, so children return findings to their parent. Normal completion waits for pending work; cancellation tears it down. |
| Advisor | Native AI finish review (two rounds) and periodic review (every four turns, at most three), no tools, bounded transcript and all-attempt usage attributed to the owning agent. Errors retain the existing fail-open reviewer contract. |
| Other plugins | Repeat guard, host finish guard, durable scoped spill, native history retrieval; shared telemetry for run/request/tool tracing. Chat also supplies `ask`, publishes bounded questions, and checkpoints them for the next user message. Native question receipts remain immutable. |

Memory and spill are persistence adapters in Soot, not additional agent loops.
Artifacts are scoped to the conversation even when memory is shared by the same
tenant/user/Soot. Existing operation grants, credential delivery, takeover and
uncertain-effect journals remain host-owned. Soot exposes fixed configured
operations, not an arbitrary shell; no OS sandbox backend or global credential
resolver is fabricated. The sandbox plugin remains available to hosts that
actually supply such a backend.

In-run jobs are not durable background tasks. Soot's existing case-backed task
queue remains responsible for restartable work. Native model/tool execution,
fallback, plugin state and summaries are Go; no upstream TypeScript source was
copied into the product tree.

Validation covers native fallback/checkpoint behavior, plan isolation and
bookkeeping, goal revision recovery, advisor correction, async child ownership,
question publication/resume, memory isolation/restart and existing Soot effect
fences. Verification uses race tests for the plugin, engine, host, AI and telemetry
packages, native root/runtime/integration tests, and Soot
`internal/agents`, `internal/runtime` and `internal/conversation`. The complete
AgentRay suite is not claimed as passing: historical typed-provider fixtures
still need migration, alongside separate environment-dependent failures.

## Pi extension boundary and applied improvements

Pi's executable `ExtensionAPI` belongs to `packages/coding-agent`, not
`packages/agent`. Its loader, slash commands, shortcuts, terminal rendering and
MCP process management are application facilities. The reference agent package
supplies `Agent.subscribe`, context/turn preparation and tool hooks; the Go
engine ports those contracts. Importing a TypeScript extension loader into the
engine would add a different application runtime to the requested Go port.

Sources: [Pi extension API](https://github.com/earendil-works/pi/blob/eeac84ca92498ac18b6832754d01aef1d3c5f654/packages/coding-agent/src/core/extensions/types.ts),
[extension runner](https://github.com/earendil-works/pi/blob/eeac84ca92498ac18b6832754d01aef1d3c5f654/packages/coding-agent/src/core/extensions/runner.ts),
[agent hooks and events](https://github.com/earendil-works/pi/blob/eeac84ca92498ac18b6832754d01aef1d3c5f654/packages/agent/src/types.ts).

AgentCore uses its Go `ExtensionFactory.BeginRun` bridge over those engine
hooks. Capabilities contribute tools, prompts, native context transforms,
checkpoint state and stop/step/tool policies. Host configuration still enters
through `Plugin.Register`; registration must not create a second turn loop.

Applied in the extension cleanup:

- Removed the `observe` plugin, legacy provider decorator registration and
  typed-journal `LogInvariant`. Shared telemetry records model attempts,
  compaction, advisor review, tool execution and nested agent runs. Native
  checkpoint/effect validation owns recovery correctness.
- Added `NativeContextContributor` to the existing extension mechanism. Failed
  transforms retain the previous valid native view and cannot corrupt signed
  provider history. No additional extension loader or event bus was introduced.
- Todo now contributes its tool, context transform and checkpoint from one
  run-scoped extension. Parent/child plan isolation no longer depends on a
  context-value override of a globally registered tool and hook.
- Memory now contributes `memory_recall`: host-pinned scope, bounded query and
  result count, memory IDs for later curation, and bounded entry content. Soot
  supplies its indexed store and grants the core tool; it has no recall loop.
- Soot's conversation and case paths supply telemetry backends and share the
  AgentCore preset. Skills, tool decoding, plan state, memory operations,
  compaction, review and in-run delegation stay in AgentCore/plugins.

## Remaining differences and migration work

The retired high-level driver has been removed. Some historical root and
integration tests still construct its typed provider/session fixtures and need
native migration; passing native/plugin suites does not make that full suite
passing. The four previously missing capabilities now have native plugin implementations
and Soot bindings. This is behavioral coverage of those gaps, not a claim of
complete OMP feature equivalence: extra compaction backends, background/in-flight
advisor interruption and OMP's CLI/config discovery remain different.
Durable workload jobs belong to Soot's runtime/orchestration; in-run jobs and
subagent execution belong to AgentCore. Those two lifetimes must stay explicit.

## Native lifecycle extension contract

`RunController` checks before turn one and after settled turns, returning an
explicit stop reason without another model call. `RunCommandHandler` handles
host-authorized commands after plugin checkpoint restoration.
`NativeRun.ControlOnly` applies controls without interpreting model/user text;
`CommandResults` returns structured receipts to the host. Commands are ordered
by extension name. `RunFinalizer` runs in reverse registration order before the
checkpoint is serialized, with the final display transcript and accounted usage.
It is distinct from `RunCloser`, which still cleans up every exit path. Core
imports no capability package, and plugins do not import sibling plugins.

Goal controls belong to the host (`NativeRun.Commands["goal"]`, or concurrent
`goal.Store.ApplyCommand` for an active run). The model gets `get_goal`; optional
objective revision remains separately opt-in. Pausing does not cancel/rollback
an in-flight effect. It waits for that turn's receipts to settle. Soot conversation
commands are serialized through its existing run queue; they do not interrupt
an already running conversation turn. A resume command updates state; the next
user request continues work. Generic per-run turn/tool ceilings grant a fresh
run on explicit resume; absolute goal token/time caps need an increased grant.

Memory consolidation uses at most four pending rollouts and 32 live memories per
pass, and commits at most 16 changes. Soot bounds pending rollouts at 64, active
memories and revision history at 512 each. Consumed rollout digests are retained
as indexed idempotency markers. The primary run remains successful when the
secondary reviewer/consolidator is unavailable. Rollout staging/consolidation
are separate memory transactions, like `learn`; transcript publication remains
in Soot's completion transaction. Memory consolidation currently runs only for
successful root runs, not children, paused questions or failed/aborted runs.

Verification added for pause/resume through fresh agents, live pause after a
settled effect, budget refusal/extension, wall-time limits, phased atomic deltas
and accepted-receipt replay, periodic/multiple reviewer schedules and cooldowns,
consolidation usage/failure/scope validation, Bolt restart/CAS/idempotency, and
Soot conversation controls without provider calls.

Case events retain their existing dispatch/retry lifetime: they start a new
run from scoped host facts rather than resuming an operational tool transcript.
Interactive lifecycle commands apply to conversation checkpoints. This does not
change Soot's uncertain-effect journal or introduce a parallel case scheduler.
