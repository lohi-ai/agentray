# AgentGarden

AgentGarden is the AgentRay surface for **creating, managing, and testing agents**.
A new agent should be a data/config change, not an AgentRay backend PR.

Read with:

- [AGENT-GOVERNANCE.md](AGENT-GOVERNANCE.md) — boundary, secrets, tools, sandbox.
- [ARCHITECT-EXTENSIONS.md](ARCHITECT-EXTENSIONS.md) — the two extension paths (MCP for tenants, hooks for maintainers).
- [ARCHITECT-AGENT-TEAM.md](ARCHITECT-AGENT-TEAM.md) — deferred sub-agent/team model.

## What users do here

| Area | Purpose |
|---|---|
| Create | Add an agent to a project; set name, slug, enabled state, and persona. |
| Manage | Edit `SOUL.md`, `AGENTS.md`, skills, tools, secrets, triggers, and task→model tiers. |
| Test (Lab) | Run the agent, inspect traces/tool calls, debug prompts, and later inspect sub-agent run trees. |

AgentGarden owns the authoring loop. Product APIs own the capabilities an agent may
call. Governance owns the trust boundary between them.

## Agent as data

```text
project
  └─ agents
       ├─ definition: SOUL.md + AGENTS.md
       ├─ skills: approved reusable instructions
       ├─ tools: enabled tool names + per-tool config
       ├─ secrets: write-only encrypted values referenced as {{cred:NAME}}
       ├─ triggers: chat / manual / schedule / webhook
       ├─ task tiers: triage / run / compaction / reflection → workspace model tier
       └─ runs, memory, traces
```

The default analytics agent is just the default agent for a project. Non-default
agents reuse the project's shared model pool unless configured otherwise through the
task-tier map.

## Runtime path

At run start, AgentGarden config is assembled into the existing runtime:

```text
agent row + definition + skills
  + enabled tools + tool config
  + encrypted secrets → credential vault
  + task tiers → model choices
  → preset.Full(...) + the product's own plugins
  → agentcore.Build(...)
  → chat/manual/schedule/webhook run
```

`internal/runtime.Build` is a **plugin composition**, not a `Config` hand-off:
the default agent comes from `preset.Full` — the same list any other agentcore
consumer starts from — and everything product-specific is appended as a named
plugin (the goal gate, the evidence finish guard, delegation, the run plan, the
sandbox and its guard, the vault, cost accounting). `BuildParams` is still the
external surface, so no caller changed. What changed is that
`agent.Describe()` now names which plugin owns which seam, and dropping a
capability is one entry off a list.

No separate engine exists for user-created agents. The same policy gate,
credential resolution, HTTP SSRF guard, sandbox, traces, and usecase boundary apply
to every agent.

### Everything that can be a plugin, is

The rule the design is held to:

> **The loop names no plugin.** Delete every package under
> `agentcore/plugins/` and `agentcore` still compiles and runs — it just does
> less.

That forces an honest split, and the split is the interesting part.

**Core** is what a run always does: the turn loop, the provider call, tool
dispatch, history, the durable log, usage accounting, the permission gate,
`read_skill`, and forking a child agent. None of these is a plugin, because
"always used" and "ejectable" are contradictory — a plugin you cannot remove is
just code in a different folder.

**Plugins** are everything else, and they reach the loop only through the generic
interfaces in `agentcore/extension.go`: `ToolInterceptor`, `BatchInterceptor`,
`StepInterceptor`, `StopInterceptor`, `RunObserver`, `LogObserver`,
`ToolContributor`, `PromptContributor`, `ContextContributor`, `RunCloser`,
`SelfGated`. The loop discovers what an extension can do by **type assertion** at
run start, so the set of things a plugin may do is open — adding a new kind of
plugin does not touch core.

Two sub-kinds, and conflating them is how a plugin system becomes a monolith with
folders:

- **Seams** configure something the loop always does (driver, model, policy,
  session, compaction). Exactly one provider each; a second claim is a build
  error naming both plugins.
- **Extensions** add something the loop does not do (spill, jobs, session
  retrieval, repeat guard, verify-on-stop, delegation, observation). Additive and
  unkeyed — several may intercept the same point and compose in registration
  order, because two interceptors bounding a tool result is a waterfall, not a
  conflict.

See [agentcore/plugins/README.md](../agentcore/plugins/README.md) for the full
table; every folder carries its own README covering what the model sees, the
token effect, the KV-cache effect, and what the plugin cannot do.

Three properties the machinery buys:

- **Replaceability.** `Registry.Describe()` prints which plugin owns which seam;
  `Agent.Describe()` prints what an agent actually ended up configured with.
- **Reversibility.** Registrations are effects owned by the plugin that made
  them, so `Registry.Unload(name)` unwinds one completely.
- **Ordering is explicit, not positional.** Hooks carry a `Priority`
  (`PriorityGate` < `PriorityDefault` < `PriorityLate`), so the permission gate
  is consulted before any consumer hook regardless of list order — the governance
  guarantee is a property of the composition, not of construction order.

And one property that is a safety invariant rather than an ergonomic: **the core
owns persistence.** An extension returns `AdditionalContexts`; the loop appends
them (after every tool result in a batch, never interleaved, which would break
tool-call/result adjacency) and writes them to the durable log. No plugin touches
`appendEntry`. That is what makes "model-visible means logged" structurally true
instead of a convention every new injector has to remember —
`observe.LogInvariant` checks it at runtime.

`agentcore/plugins/preset` composes them back into agentcore's default agent, and
two tests carry the claims: `preset.New(cfg)` and `agentcore.New(cfg)` build the
same agent (parity), and building the same agent *without* a capability leaves no
trace of it — no tool, no name in the extension list (ejection). The second is
the test that fails if someone re-couples the loop to a plugin by name.

`preset.Plugins` stays a pure mirror of `Config` so that parity proof holds.
`preset.Full(cfg, opts)` is the list a deployment composes: `Plugins` plus the
capabilities `Config` has no field for — `spill`, `jobs`, `repeatguard`,
`sessionquery`, and the two `observe` plugins. Every `Options` field degrades to
*off*, never to *wrong*: a nil spill store leaves the loop's own truncation in
place rather than minting locators into storage that dies with the process,
which a resumed run would read back as not-found. AgentRay supplies a
Postgres-backed one (`agent_spill`, cascading off `agent_runs` exactly like the
session log).

What is deliberately **not** copied from deepseek-harness / Cordis: the
dependency-injection framework. No reflection, no service container, no lifecycle
graph. A plugin is a value with a `Register` method, a seam is a typed field, an
extension point is a Go interface, and composition is a function call.

For AgentRay this changes nothing about the agents-as-config rule — an agent is
still a marketplace preset, never Go. It changes what the *platform* can do: swap
or eject a capability for a tenant or a test without forking the loop.

### Goal gate (`/goal`)

A chat message starting with `/goal <condition>` (task on the following lines,
optional) runs that turn goal-gated: the completion contract lands in the system
prompt, and the run may only stop by ending its answer with `STATUS: DONE` (goal
met) or `STATUS: BLOCKED` (+ reason). The sentinel is matched on the answer's
closing line only (mentioning it mid-prose does not count), and the goal is
recorded in the durable log so a crash-resumed run stays gated. A finish
without either sentinel is
re-opened with a keep-going nudge. The gate is uncapped but never wedges a run —
turn/tool/budget limits still bound the loop, a budget wrap-up bypasses it, and a
verbatim-repeated answer stops as `goal_stalled`. Mechanism:
the gate is a plugin (`agentcore/plugins/goal`) reaching the loop through the
generic `PromptContributor` + `StopInterceptor` extension points. Core keeps only
the goal as durable STATE — it writes `EntryGoal`, recovers it on resume, and
hands it back as `RunInfo.Goal` (`agentcore/goal.go`), because only the loop may
write the durable log. `agentcore.Config.Goal` therefore RECORDS a goal;
enforcing it needs the plugin, which `preset.Plugins` (and therefore
`internal/runtime`) wires automatically. The chat directive parser is `parseGoalDirective`
(`internal/runtime/chat.go`).

## Tools and secrets

- Tool kinds are code-defined and audited once.
- Agent authors choose which existing tools an agent may use.
- `http_request` uses a per-agent host allowlist.
- `mcp` is the one selection that expands to many tools: its config lists remote
  Model Context Protocol servers the project operates, and each contributes its
  advertised tools as `mcp__<server>__<tool>`. This is how a tenant adds a
  capability without an AgentRay PR — see
  [ARCHITECT-EXTENSIONS.md](ARCHITECT-EXTENSIONS.md).
- Secrets are write-only, encrypted at rest, and referenced by name:
  `{{cred:NOVEL_API_KEY}}`.
- Secret values are resolved only at the trust boundary, after tracing and policy
  checks, immediately before tool execution.

## Triggers

| Trigger | Use |
|---|---|
| chat | Human conversation with a selected agent. |
| manual | Lab/run-now testing. |
| schedule | Autonomous recurring work. |
| webhook | External systems enqueue a run by token/HMAC. |

Webhook requests map to a prompt/context and enqueue a run; they never invoke tools
directly.

## Lab

Lab is the safe test bench for agents:

- run an agent with a prompt;
- inspect messages, tool calls, credentials-as-placeholders, usage, and errors;
- verify tool allowlists and missing-secret failures;
- replay/debug prompt and skill changes;
- later: inspect `spawn_subagent` parent/child run trees and team kanban card runs.

## Seed proof: novel-request moderator

The acceptance test for AgentGarden is the novel request moderator. It moderates
`truyen.lohi2.com/admin/yeu-cau` as config only:

| Need | AgentGarden config |
|---|---|
| Workflow | `AGENTS.md` + a moderation skill. |
| Reach target API | `http_request` with `allow_hosts=[api.lohi2.com, webnovel.vn]`. |
| Auth | `X-API-Key: {{cred:NOVEL_API_KEY}}`. |
| Start work | schedule or webhook trigger. |
| Test | Lab run against sample pending requests. |

The target Novel API exposes audited operations under `/novel/agent/*` plus a
capability manifest. AgentGarden does not hardcode novel moderation behavior.

### Second surface: reader edit-suggestion pre-review

The same `agent_mod` principal also pre-reviews reader-submitted paragraph
edits, and it needed **zero** AgentGarden code — only three new operations and a
playbook section in the Novel API's manifest (`list_edit_queue`,
`get_edit_context`, `submit_edit_verdict`). Nothing about the agent changes:
same key, same `http_request` allowlist, same trigger.

Note what carried the second and largest improvement: `get_edit_context`, which
hands the reviewer the paragraphs around the edit and how comparable edits were
judged before. That is **input**, not capability — the agent gained no new
power, it simply stopped being asked to judge a two-line diff blind. When an
agent's answers are weak, widening what it can *read* is almost always the
cheaper fix than widening what it can *do*.

The split of responsibility is the point, and it is worth copying for any future
surface:

| Layer | Owns | Artifact |
|---|---|---|
| Agent skill | The judging rubric: what counts as a real fix, what is vandalism, when to answer "unsure", why confidence must be honest. | `kiem-lai/docs/agent-skills/novel-edit-review.md`, pasted into the skill editor |
| Agent credential | The API key. | `NOVEL_API_KEY` secret, referenced as `{{cred:NOVEL_API_KEY}}` in the `http_request` header config |
| Novel API manifest | Which operations exist, their paths and payloads. | `GET /novel/agent/skills` |
| Novel API policy | Turning verdict × confidence × submitter trust into `auto_accept` / `auto_reject` / `human`, and performing the write. | `api/src/modules/edit-suggestions/ai-review.ts` |

That split is the reusable part. **Rubric in a skill, key in a credential,
operation list in the API manifest, decision in the API.** Each moves for a
different reason and at a different cadence: the rubric is tuned without a
deploy, the key rotates without touching the rubric, and the thresholds that
decide whether a machine may write to a published chapter stay in reviewed,
tested server code where an agent cannot reach them.

The agent cannot accept a suggestion, cannot write chapter text, and cannot
raise its own thresholds — it has no such operation. A verdict the policy
declines to act on is still persisted and shown to the human mod, so the
low-confidence path is useful work rather than a wasted call.

## Current shipped state

- Per-agent secrets, tools, triggers, definitions, skills, and task-tier settings
  are implemented.
- First-class `agents` exist; projects can have many agents.
- Schedule and webhook triggers exist.
- Authoring UI exposes Configure/Agents surfaces and the seed recipe guide.
- Novel API `agent_mod` + `/novel/agent/*` shipped as the config-only proof.

Deferred:

- per-agent budget/quota;
- untrusted multi-tenant hardening of skill authoring and retrieved-data screening;
- new tool kinds beyond current built-ins;
- sub-agent and team orchestration (see Agent Team doc).
