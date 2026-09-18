# agentcore

The kernel. The runtime is one flat package, with two explicit subdirectories:
ejectable [`plugins/`](plugins/) and black-box [`integration/`](integration/)
tests. The runtime has a hard dependency rule in both directions:

> **The kernel names no plugin, and depends on nothing else in this module.**
> `agentcore` imports only the standard library plus focused Unicode and JSON
> Schema libraries. Delete every package under `plugins/` and this package still
> compiles, still runs, and still passes its tests — it just does less.

Both halves are tests, not prose: [`boundary_test.go`](boundary_test.go) reads
the package's own imports and fails on a `plugins/` import or on any other
package in this module. That is what makes the kernel publishable on its own and
what makes [`plugins/README.md`](plugins/README.md)'s ejectability claim true.

Everything structural on this page is enforced the same way, because a rule that
only this file knows is a rule that drifts:

| test | holds |
|---|---|
| `TestKernelNamesNoPlugin` | no `plugins/` import from the root package |
| `TestKernelIsAModuleLeaf` | no in-module import at all |
| `TestKernelTreeHoldsOnlyDeclaredBoundaries` | only `plugins/` and black-box `integration/` tests sit below the kernel |
| `TestEveryKernelFileJustifiesItself` | every root `.go` file has a row below |
| `TestPluginsDoNotNameEachOther` | no plugin imports a sibling (except `preset`) |
| `TestEveryPluginDocumentsItself` | every plugin folder has a `README.md` |

The API-side version of this page — the boundary rules, the three kinds of
contribution, and the layer map — is [`doc.go`](doc.go), so a reader who arrives
through `go doc` gets it too.

## Why the root is flat

The obvious tidy-up — `agentcore/session/`, `agentcore/context/`,
`agentcore/tools/` — is the wrong move here, because these files are not
independent concerns that happen to sit together. They are one machine:
the loop reads the limits, writes the log, dispatches the extensions, brackets
the compaction, and stamps the idempotency key, all through unexported fields
of one `Agent`. Splitting them into packages would either export that machinery
(making it everyone's to break) or produce packages that can only be imported in
one order — folders pretending to be boundaries.

Flat is not the same as unordered, though, and the distinction is where this
package earns the choice. The layering is real — composition sits above the
contracts, the contracts above the loop, the loop above the log — it is just
carried by *file* rather than by folder, and by a rule about which direction
job: `tooldispatch.go` is the trust boundary, `turn.go` is one turn against the
model, `compose.go` is how an agent gets built. A file earns a split only when
it starts needing "and" to describe a *boundary*, not a subject — the loop's
own retry, schema validation, and idempotency stamps live inside the files that
use them rather than beside them. [`doc.go`](doc.go) lists the layers in order.

The real boundary is `plugins/`, and it is enforced above. So the organizing
question for the root is not "which folder?" but:

> **Why is this file in the kernel and not a plugin?**

There are exactly four answers, and every file below gives one. A file that
cannot give one belongs in a plugin, or belongs nowhere.

| answer | means |
|---|---|
| **loop** | the loop calls it directly on every run; there is no composition in which it is absent |
| **contract** | a type the loop dispatches over, so a plugin outside this repo can implement it |
| **log** | it reads or writes durable run state, and only the log's owner may |
| **seam default** | the built-in behind a replaceable seam, so an agent composed with nothing still works |

## The files

### Composition — how an agent gets built

| file | why core |
|---|---|
| [`doc.go`](doc.go) | **contract** — the package doc: the two boundary rules, the three kinds of plugin contribution, and the layer map below rendered where `go doc` can see it. No code. |
| [`provider.go`](provider.go) | **contract** — `LLMProvider`, `ChatRequest/ChatResponse`, `Usage`. The wire seam every model call goes through, kept small enough that an implementation is a translation layer and nothing more. |
| [`plugin.go`](plugin.go) | **loop** — `Plugin`, `Registry`, `Priority`. The composition surface itself: seam setters, additive contributions, per-plugin `Unload`. |
| [`compose.go`](compose.go) | **loop** — `Build`, `BuildRegistry`, `ApplyConfig`, and `Limits`/`DefaultLimits`: the run's bounds are chosen at composition, read every turn, and published to extensions through `RunInfo`. There is no composition in which a run is unbounded. |
| [`agent.go`](agent.go) | **loop** — the configured runtime instance (capability-bearing fields unexported on purpose, see `fork.go`) plus `Agent.Describe()`: what the agent is *actually* configured with after every default and override. |
| [`definition.go`](definition.go) | **contract** — `AgentDefinition`, `Skill`, `SkillLoader`, and the always-loaded byte cap that keeps the system prompt bounded. |
| [`seams.go`](seams.go) | **seam default** — `ConfigPlugin`, `ModelPlugin`, `DefinitionPlugin`, `PolicyPlugin`, `ToolsPlugin`, `HooksPlugin`, `BudgetPlugin`, `SessionPlugin`, `CompactionPlugin`, `SteeringPlugin`. Core adapters that claim seams during `Build(...)` without needing fake wrapper packages under `plugins/`. |

### The loop and its extension points

| file | why core |
|---|---|
| [`loop.go`](loop.go) | **loop + seam default** — the `Driver` seam (control flow as a replaceable service; without it the loop is the one thing you could not change without forking) and `DefaultDriver`'s body: reason → act, parallel batches, compaction bracketing, the graceful-stop protocol. Also the **only** writer of the durable log, which carries an obligation: a tool result that exists only because the run was cancelled is not a settled fact and is not written, because a call with a recorded result is answered forever and nothing would ever retry it. For the same reason a cancelled call does not count against the circuit breaker — every call in a wide batch fails when the parent dies, and the breaker's verdict is durable, so counting them left a resumed run with a working tool permanently disabled. |
| [`turn.go`](turn.go) | **loop** — one turn against the model: `ProviderError` classification, same-rung retry, then escalation down the ladder, plus the streaming path. Retry lives with the loop, not in a provider, so failure behaviour cannot differ per vendor. That includes reading usage off the stream: a delta's `Usage` is a running total that may arrive at any point (Anthropic states input tokens before the first output token; OpenAI sends a usage-only chunk *after* the terminal one), so the turn keeps the newest non-zero value of each field rather than whatever rode `Done`. Getting it wrong is silent — the answer is still correct and only the number the budget gate meters on is zero. |
| [`tooldispatch.go`](tooldispatch.go) | **loop** — one tool call end to end: lookup → prepare → validate → gate → execute → bound → trace. The trust boundary, applied in exactly one place so it is unskippable rather than usually-called. Also the two context stamps a call carries: the idempotency key derived from `(sessionID, toolCallID)` — stable across crash-resume because both already survive in the log — and the provider-assigned tool-call id a spawn tool derives the child's deterministic session from. |
| [`result.go`](result.go) | **contract** — `RunResult`, `StreamEvent` and the event vocabulary, `ResultCard`. The loop's output side, which consumers render and plugins observe. (`ToolTrace` sits with the code that fills it, in `tooldispatch.go`.) |
| [`extension.go`](extension.go) | **contract** — every extension point (`ToolInterceptor`, `StepInterceptor`, `StopInterceptor`, `RunObserver`, `ToolContributor`, …) and the `extensionSet` the loop dispatches through. The file that forbids naming a plugin. |
| [`hooks.go`](hooks.go) | **contract** — the lifecycle hook types and their dispatch, including the `BeforeToolCall` shape the permission gate is built from. |
| [`tool.go`](tool.go) | **contract** — `Tool`, `ToolSet`, `ArgPreparer`, and the loop's own byte bounding, exported so a plugin bounds text the same way rather than a copy of the way. |
| [`permission.go`](permission.go) | **contract + seam default** — `Policy`, `Decision`, and `DenyAll`. Default-deny is the kernel's, so a composition that forgets governance is not ungoverned. |
| [`prompt.go`](prompt.go) | **loop** — system-prompt assembly, recall dedup, and provider-neutral prompt-cache breakpoints placed at the end of the request's *append-only* prefix (the persisted history), not on its final message. A `ContextHook` trailer is re-rendered every turn, so a prefix ending in one can never be read back: measured on a 300-turn run, **7 of 299** cache entries were still a prefix of the next request; anchoring before the trailer makes it **277 of 299** (the rest are the 22 compactions, which legitimately rewrite the window). |
| [`skill_tool.go`](skill_tool.go) | **loop** — the `read_skill` built-in, registered by the loop whenever a definition carries skills. Progressive disclosure is part of the prompt, not an add-on. |
| [`schema.go`](schema.go) | **loop** — compiles `OutputSchema` at composition time and validates final text locally, so providers that ignore structured-output hints cannot silently violate the result contract. |

### Durable state — only the log's owner may touch these

| file | why core |
|---|---|
| [`session.go`](session.go) | **log** — `SessionEntry` kinds, `SessionStore`, reduce/recover, side records (`EntryInbox`/`EntryInboxDone`, `EntryAssistantFrame`, `EntryToolProgress`), and the goal as a *fact about the run* (written once, recovered on resume; what to DO about an unmet goal is [`plugins/goal`](plugins/goal/)). The log is the source of truth; run state is rebuilt by reducing it, never mutated in place. Chain entries form the session tree; side records capture intent and mid-turn in-flight granularity (streaming frames, tool progress) without forking the chain. `SessionBatchStore` upgrades a turn save point from ordered-prefix compatibility to an atomic commit. The windowed read (`SessionWindowStore`, `LoadResumeLog`) lets the fold restart at a checkpoint, so a resume reads a suffix rather than a history — and the rule for when that is safe lives here, once, rather than in each backend. |
| [`session_tree.go`](session_tree.go) | **log** — the log is a tree: parent ids, branches, `EntryLeafMove`, `Rewind`. Reduce and recover walk only the active branch. |
| [`memsession.go`](memsession.go) | **seam default** — in-process append-only `SessionStore` with atomic save-point batches, so a run is resumable and the log/concurrency invariants are checkable with nothing wired. |
| [`compaction.go`](compaction.go) | **log + seam default** — WHEN to compact and the durable bracket around it, plus the goal pin that survives summarization, and `Compactor`/`DefaultCompactor`: WHAT replaces the old span, as a strategy with more than one right answer. The trigger is `min(model window − output headroom, configured ceiling)`, re-derived per turn from the answering rung: the ladder routinely mixes models whose windows differ by 30x, and a ceiling too high for the current one means the loop never compacts before the provider rejects the request. The kernel knows no model's window — the rung carries it. |
| [`fork.go`](fork.go) | **loop** — everything that crosses an agent boundary: building a child from a parent's *unexported* fields (a delegation plugin cannot do this from outside without those fields becoming exported, which is exactly how a child ends up out-scoping its parent), delegation depth (a spawn plugin's recursion cap is only enforceable if the depth survives the hop), and the run's session id (a sub-agent shares its parent's provider and ctx, so without this tag the two are one undifferentiated stream to anything decorating the seam). All three ride ctx — the only thread that survives the hop. |
| [`lab.go`](lab.go) | **log** — the read model: one pure fold from recorded facts to ordered steps, so a live-stepped run and a replayed run read identically — and the same fold one level up as chapters: a run's compaction summaries ARE its table of contents, and dividing the fold at them is what makes a several-thousand-step run navigable rather than merely paginated. |

### Host capabilities — injected, never reached for

| file | why core |
|---|---|
| [`env.go`](env.go) | **contract** — `Env` and everything it carries: the `Sandbox`/`SessionSandbox` execution contract with its session-id plumbing, and the `CredentialResolver`. Both host-injected, both optional; keeps the kernel free of infrastructure imports. |
| [`memory.go`](memory.go) | **contract** — `MemoryEntry`, `MemoryStore`, `Embedder`, `Cosine`: cross-run recall as a seam the consumer backs. A nil store is valid; ranking degrades to keyword recall rather than failing. |
| [`faux.go`](faux.go) | **seam default** — the providers that make the loop testable with no network and no key: `FauxProvider` (scripted) and `ReplayProvider` (plays a recorded `[]TurnRecord` and asserts the loop rebuilt the same request, so a transcript is a regression test). omp has no record/replay; this is agentray's. |

## Tests in the root

Root tests exercise **loop-owned** behaviour, not plugin policy. Two that look
like plugin tests and are not, because the state is the kernel's:

- `budget_test.go` — the graceful-stop protocol (one tool-free wrap-up turn,
  then `budget_exhausted`) is the loop's; `BudgetPlugin` in `seams.go`
  only supplies the ceiling.
- `session_test.go`, `compaction_test.go` — the goal's persistence and its
  survival through compaction are the log's;
  [`plugins/goal`](plugins/goal/) owns the completion protocol and is tested
  there.

Black-box tests that import plugins now live in [`integration/`](integration/).
They prove capabilities against the real public loop without mixing consumer
composition into the kernel's package-private test suite. A plugin-policy test
still belongs next to the plugin itself.
