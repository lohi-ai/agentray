# AgentGarden

AgentGarden is the AgentRay surface for **creating, managing, and testing agents**.
A new agent should be a data/config change, not an AgentRay backend PR.

Read with:

- [AGENT-GOVERNANCE.md](AGENT-GOVERNANCE.md) — boundary, secrets, tools, sandbox.
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

## Tools and secrets

- Tool kinds are code-defined and audited once.
- Agent authors choose which existing tools an agent may use.
- `http_request` uses a per-agent host allowlist.
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
