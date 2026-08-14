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
  → agentcore.New(...)
  → chat/manual/schedule/webhook run
```

No separate engine exists for user-created agents. The same policy gate,
credential resolution, HTTP SSRF guard, sandbox, traces, and usecase boundary apply
to every agent.

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
`agentcore.Config.Goal` (`agentcore/goal.go`), threaded per run via
`RunOptions.Goal`; the chat directive parser is `parseGoalDirective`
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
