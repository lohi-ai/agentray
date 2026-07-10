# AgentRay Agent Teams

**Status:** P0–P1 shipped (`spawn_subagent` lives in `agentcore/subagent.go`,
enabled for every solo agent with the default caps). Cross-agent delegation —
P3's `delegate(member_id, task)` — is also shipped, pulled forward without the
team/kanban machinery: a per-agent **Teammates** grant list (FE setup tab →
`agent_delegates` table → `spawn_subagent`'s `agent` parameter) lets an agent
route a task to another workspace agent, which runs under its *own* persona,
tools, policy, and secrets via the normal runner path (its own run row, trigger
`delegate`). Depth rides the ctx (`agentcore.DelegationDepth`) so A→B→A
recursion bottoms out at the same cap as self-forks. P2 (teams/kanban/lead
orchestrator skill) remains deferred target architecture.

AgentRay has one agent abstraction: the **general agent** created and managed by
AgentGarden. A general agent is conversational, governed by
[AGENT-GOVERNANCE.md](AGENT-GOVERNANCE.md), and can do work through its allowed
tools.

## Key model

### 1. Any solo agent can spawn sub-agents

Every agent that is **not running in a team scope** may use:

```text
spawn_subagent(task, scope?)
```

A sub-agent is an ephemeral child for one task:

- inherits the parent definition, tools, secrets, and permissions;
- `scope` may only narrow access, never widen it;
- runs with isolated history so exploration does not pollute the parent chat;
- returns only a final answer/card to the parent;
- persists a child run linked by `parent_run_id` for Lab/run-tree inspection;
- is capped by depth, count, timeout, budget, and model-visible output size.

This is the replacement for the old `agentorch` worker handoff: delegation becomes
a normal governed tool call inside the existing `agentcore` loop.

### 2. A team is an agent fleet on a kanban

A **team** is a managed fleet of agents working from a shared board:

```text
team
  ├─ members: existing AgentGarden agents
  ├─ kanban: backlog / doing / review / done work items
  └─ orchestrator: one member selected as lead
```

The orchestrator is not a new runtime. It is a normal team member with an injected
**orchestrator skill** that teaches it to:

- pick cards from the kanban;
- break work into tasks;
- assign tasks to members;
- ask members for status/results;
- synthesize the final answer/update.

Team scope enables member delegation:

```text
delegate(member_id, task)
```

Only the selected lead receives the orchestrator skill. Other members remain normal
agents. A project may have multiple teams and choose a default team later.

## Why this replaces the old orchestrator

The retired `agentorch` design had a cheap front desk classifier and a hardcoded
`Workers` map. That was useful for the first analytics chat surface, but it made
orchestration a special package and made new products feel like worker plugins.

The target is simpler:

| Old | New |
|---|---|
| `Classifier` chooses route | a general agent owns the conversation |
| `Workers[route]` do domain work | tools, sub-agents, or team members do work |
| special `agentorch` package | normal `agentcore` tool calls |
| one hardcoded roster | AgentGarden agents + deferred Team roster |

A direct chat with `?agent=<id>` talks to that agent's own persona. A future team
chat with `?team=<id>` talks to the selected lead/orchestrator.

## Target data model

Deferred tables, names illustrative:

```text
teams(id, project_id, name, slug, lead_agent_id, default, ...)
team_members(team_id, agent_id, role, position)
team_cards(id, team_id, status, title, body, assignee_agent_id, ...)
```

Agents themselves remain the AgentGarden `agents` rows. Teams only group them and
add board/orchestration state.

## Delegation modes

| Mode | Shape | Phase |
|---|---|---|
| Sub-agent | solo agent forks one ephemeral child | shipped |
| Single delegate | agent routes a task to a granted teammate agent | shipped (Teammates grants, no team scope needed) |
| Chain | ordered tasks passing `{previous}` forward | later |
| Fan-out | lead asks several members/children, then synthesizes | later (parallel spawn/delegate calls already run concurrently) |

Suggested caps: max 8 child/delegate tasks per turn, 4 concurrent, about 50 KB
model-visible output per task, with full transcripts in traces.

## Chat and Lab surfaces

- AgentGarden is where users create/manage/test agents.
- Lab should show parent/child run trees for `spawn_subagent`.
- Future Team UI should show the kanban, lead selection, member roster, and per-card
  run traces.
- Chat can later route to direct agents (`?agent=`) or teams (`?team=`); team chat
  streams lead progress plus member/child updates.

## Phases

| Phase | Deliverable |
|---|---|
| P0 | Direct conversational general agents (`?agent=`) |
| P1 | `spawn_subagent` for non-team-scoped agents |
| P2 | Team schema + kanban + lead selection |
| P3 | Inject orchestrator skill into the selected lead; add `delegate(member_id, task)` |
| P4 | Chain/fan-out workflows, streamed multi-member progress, run-tree Lab views |

## Safety rules

- Delegation never bypasses governance: child/member calls pass through policy,
  credential resolution, sandbox/HTTP guards, and the usecase boundary.
- Sub-agents inherit narrow-only permissions.
- Cancelling a parent run cancels in-flight children/delegates.
- Team membership does not grant new tools by itself; tool access remains per agent.
