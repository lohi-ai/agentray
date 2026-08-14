# agentcore

The kernel. One flat package, no subdirectories except [`plugins/`](plugins/),
and a hard dependency rule in both directions:

> **The kernel names no plugin, and depends on nothing else in this module.**
> `agentcore` imports only the standard library and `golang.org/x/text`. Delete
> every package under `plugins/` and this package still compiles, still runs,
> and still passes its tests — it just does less.

Both halves are tests, not prose: [`boundary_test.go`](boundary_test.go) reads
the package's own imports and fails on a `plugins/` import or on any other
package in this module. That is what makes the kernel publishable on its own and
what makes [`plugins/README.md`](plugins/README.md)'s ejectability claim true.

## Why the root is flat

The obvious tidy-up — `agentcore/session/`, `agentcore/context/`,
`agentcore/tools/` — is the wrong move here, because these files are not
independent concerns that happen to sit together. They are one machine:
the loop reads the limits, writes the log, dispatches the extensions, brackets
the compaction, and stamps the idempotency key, all through unexported fields
of one `Agent`. Splitting them into packages would either export that machinery
(making it everyone's to break) or produce packages that can only be imported in
one order — folders pretending to be boundaries.

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
| [`provider.go`](provider.go) | **contract** — package doc, `LLMProvider`, `ChatRequest/ChatResponse`, `Usage`. The wire seam every model call goes through. |
| [`plugin.go`](plugin.go) | **loop** — `Plugin`, `Registry`, `Priority`. The composition surface itself: seam setters, additive contributions, per-plugin `Unload`. |
| [`compose.go`](compose.go) | **loop** — `Build`, `BuildRegistry`, `ApplyConfig`. Turns a plugin set into an `Agent`; `Config` is routed through the same setters plugins use. |
| [`agent.go`](agent.go) | **loop** — the configured runtime instance. Its capability-bearing fields are unexported on purpose (see `fork.go`). |
| [`describe.go`](describe.go) | **loop** — `Agent.Describe()`: what this agent is *actually* configured with, after every default and override. |
| [`definition.go`](definition.go) | **contract** — `AgentDefinition`, `Skill`, `SkillLoader`, and the always-loaded byte cap that keeps the system prompt bounded. |

### The loop and its extension points

| file | why core |
|---|---|
| [`driver.go`](driver.go) | **seam default** — `Driver`: control flow as a replaceable seam. Without this the loop is the one thing you could not change without forking. |
| [`loop.go`](loop.go) | **loop** — `DefaultDriver`: reason → act, parallel batches, the per-run circuit breaker, `Limits`. Also the **only** writer of the durable log. |
| [`extension.go`](extension.go) | **contract** — every extension point (`ToolInterceptor`, `StepInterceptor`, `StopInterceptor`, `RunObserver`, `ToolContributor`, …) and the `extensionSet` the loop dispatches through. The file that forbids naming a plugin. |
| [`hooks.go`](hooks.go) | **contract** — the lifecycle hook types and their dispatch, including the `BeforeToolCall` shape the permission gate is built from. |
| [`tool.go`](tool.go) | **contract** — `Tool`, `ToolSet`, `ArgPreparer`, and the loop's own byte bounding, exported so a plugin bounds text the same way rather than a copy of the way. |
| [`schema.go`](schema.go) | **loop** — argument validation between the model's JSON and execution, so a wrong-shape call is a self-correctable message, not a panic. |
| [`permission.go`](permission.go) | **contract + seam default** — `Policy`, `Decision`, and `DenyAll`. Default-deny is the kernel's, so a composition that forgets governance is not ungoverned. |
| [`retry.go`](retry.go) | **loop** — `ProviderError` classification and `RetryPolicy`. The loop decides same-rung retry vs escalation, so the decision lives with the loop. |
| [`cacheanchor.go`](cacheanchor.go) | **loop** — provider-neutral prompt-cache breakpoints. Only the loop knows which prefix of a request is stable across turns; providers map the marks or ignore them. |
| [`prompt.go`](prompt.go) | **loop** — system-prompt assembly and recall dedup, run before every request. |
| [`skill_tool.go`](skill_tool.go) | **loop** — the `read_skill` built-in, registered by the loop whenever a definition carries skills. Progressive disclosure is part of the prompt, not an add-on. |

### Durable state — only the log's owner may touch these

| file | why core |
|---|---|
| [`session.go`](session.go) | **log** — `SessionEntry` kinds, `SessionStore`, and reduce/recover. The log is the source of truth; run state is rebuilt by reducing it, never mutated in place. |
| [`session_tree.go`](session_tree.go) | **log** — the log is a tree: parent ids, branches, `EntryLeafMove`, `Rewind`. Reduce and recover walk only the active branch. |
| [`memsession.go`](memsession.go) | **seam default** — in-process append-only `SessionStore`, so a run is resumable and the log invariant is checkable with nothing wired. |
| [`goal.go`](goal.go) | **log** — the goal as a *fact about the run*: written once, recovered on resume. What to DO about an unmet goal is [`plugins/goal`](plugins/goal/). |
| [`compaction.go`](compaction.go) | **log** — WHEN to compact and the durable bracket around it, plus the goal pin that survives summarization. |
| [`compactor.go`](compactor.go) | **seam default** — `Compactor` + `DefaultCompactor`: WHAT replaces the old span, as a strategy with more than one right answer. |
| [`idempotency.go`](idempotency.go) | **log** — the key derived from `(sessionID, toolCallID)`, which only works because both already survive a crash-resume in the log. |
| [`delegation.go`](delegation.go) | **log** — delegation depth carried across an agent boundary. A spawn plugin's recursion cap is only enforceable if the depth survives the hop, and nothing outside the kernel can carry it. |
| [`fork.go`](fork.go) | **loop** — building a child from a parent's *unexported* fields. A delegation plugin cannot do this from outside without those fields becoming exported, which is exactly how a child ends up out-scoping its parent. |
| [`lab.go`](lab.go) | **log** — the read model: one pure fold from recorded facts to ordered steps, so a live-stepped run and a replayed run read identically. |

### Host capabilities — injected, never reached for

| file | why core |
|---|---|
| [`env.go`](env.go) | **contract** — `Env`: the sandbox and the credential resolver, both host-injected and both optional. Keeps the kernel free of infrastructure imports. |
| [`sandbox.go`](sandbox.go) | **contract** — the `Sandbox` / `SessionSandbox` execution contract and the session-id plumbing. The kernel never learns what a backend is. |
| [`memory.go`](memory.go) | **contract** — `MemoryEntry`, `MemoryStore`: cross-run recall as a seam the consumer backs. A nil store is valid. |
| [`embed.go`](embed.go) | **contract** — `Embedder` and `Cosine`, for semantic recall. Ranking degrades to keyword recall rather than failing. |
| [`faux.go`](faux.go) | **seam default** — `FauxProvider`: a scripted provider so the loop, hooks, and gate are testable with no network and no key. |

## Tests in the root

Root tests exercise **loop-owned** behaviour, not plugin policy. Two that look
like plugin tests and are not, because the state is the kernel's:

- `budget_gate_test.go` — the graceful-stop protocol (one tool-free wrap-up turn,
  then `budget_exhausted`) is the loop's; [`plugins/budget`](plugins/budget/)
  only supplies the ceiling.
- `goal_state_test.go`, `goalpin_test.go` — the goal's persistence and its
  survival through compaction are the log's;
  [`plugins/goal`](plugins/goal/) owns the completion protocol and is tested
  there.

Only one root test imports a plugin: the env-gated live-model
`agent_realprovider_test.go`, which is an end-to-end composition by design. Any
other plugin import from a root test is a sign the test moved out from under its
subject — put it next to the plugin instead.
