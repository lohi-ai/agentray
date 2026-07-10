# DESIGN — Server-side Conversation Store (multi-machine, multi-user)

**Status:** proposal · **Date:** 2026-06-26 · **Owner:** agents

Adapts the session model from [earendil-works/pi](https://github.com/earendil-works/pi)
(MIT) to agentray: move the conversation of record from the browser's
`localStorage` to a Postgres-backed, append-only **entry log**, so an agent run
can be started on one machine, resumed on another, and joined by a second user —
all reading and writing one durable conversation.

This is a **rebuild**, not a copy. Pi is a single-user local CLI that stores to
JSONL; we take its data model (append-only typed entries + a movable leaf
pointer + compaction-as-an-entry) and back it with Postgres plus the distributed
parts Pi never needed (auth, concurrent-writer resolution, realtime fan-out).

---

## 1. Problem

Today the rich, user-visible history lives client-side:

- `web/modules/chat/` holds `ChatMsg[]` in `localStorage`, keyed by session id.
- The agent's LLM context is reconstructed by the **client replaying**
  `{role:user}` + `{role:assistant}` pairs as `History` on each send
  (`page.tsx`). Steps, tool traces, and cards are never replayed.
- The server keeps per-run state only: `agent_runs` + `agent_tool_calls` +
  the append-only `agent_session_log` (one log **per run**, the agentcore
  `SessionStore` seam). Cross-turn chat memory is explicitly out of scope —
  `runner.go`: *"the caller (client) holds these."*

Consequences:

| Requirement | Today |
|---|---|
| Continue a conversation on a second machine | ✗ — history is in machine 1's `localStorage` |
| A second user joins the same conversation | ✗ — no shared server record |
| Long-conversation context management | partial — compaction is **within a single run** only; a new turn re-seeds from client-replayed pairs |
| Survive reload | partial — client live-persists steps to `localStorage`; resume rebuilds tool steps from `agent_tool_calls` |

The gap is a single durable, **conversation-scoped** store that is the source of
truth for both projections — what the human sees and what the model is fed.

---

## 2. What Pi does (the model we're adopting)

Pi never stores "the messages the model sees." It stores an immutable **tree of
typed entries** and *derives* everything else.

1. **Append-only entry tree.** Every event is an entry
   `{ id, parentId, timestamp, type, … }`. `parentId` pointers form a DAG
   (git-like). Types: `message`, `compaction`, `branch_summary`, `model_change`,
   `active_tools_change`, `leaf`, … . IDs are `uuidv7` (time-ordered, globally
   unique with no coordination).
2. **Movable leaf pointer.** "Where the conversation currently is" is just an
   entry id. `moveTo(id)` re-points it; appending sets it to the new entry.
3. **Context is derived, not stored.** `buildContext = reduce(getPathToRoot(leaf))`.
   The reducer folds the path: model/tool settings collapse to their last value,
   messages accumulate. The same log renders for the human and reduces for the
   model — **one source, two projections**.
4. **Compaction is an entry.** When `contextTokens > contextWindow - reserveTokens`
   (defaults `reserveTokens: 16384`, `keepRecentTokens: 20000`), Pi finds a cut
   point that snaps to a **turn boundary** (never mid tool-call/result pair) and
   appends a `compaction` entry `{ summary, firstKeptEntryId, tokensBefore }`.
   The next `buildContext` emits `summary` + entries from `firstKeptEntryId`
   onward; the compacted prefix stays on disk but leaves the model window. Non-
   destructive, inspectable, repeatable.
5. **Swappable storage.** All of the above sits behind an async `SessionStorage`
   interface (`appendEntry / getEntry / getPathToRoot / getLeafId / setLeafId /
   findEntries`) with jsonl + in-memory impls. Pi ships **no** server, DB, or
   multi-user sync — but the interface is the intended extension point.

**Why this model fits our requirement:** an append-only DAG with parent pointers
is inherently sync- and concurrency-friendly. Three machines all just
`appendEntry(parentId)`; the leaf pointer resolves "current"; concurrent appends
to the same parent are *branches*, which a server resolves deliberately.

---

## 3. Mapping to agentray (we are ~70% there)

We already have the seam. `internal/storage/agent_session_log.go` is an
append-only, opaque-payload, sequence-ordered entry log behind agentcore's
`SessionStore` — Pi's `SessionStorage` by another name. The change is to
**promote it from run-scoped to conversation-scoped** and add identity, a leaf,
and sync.

| Pi concept | agentray today | Proposed |
|---|---|---|
| `SessionStorage` interface | `agent_session_log` (per `run_id`) | `agent_conversations` + `agent_conversation_entries` (per conversation) |
| entry `{id,parentId,type,payload}` | `AgentSessionEntry{id,run_id,seq,kind,payload_json}` | add `conversation_id`, `parent_id`; keep `kind`/`payload_json` |
| leaf pointer | implicit `MAX(seq)` per run | explicit `leaf_entry_id` on the conversation row |
| `buildContext` reducer | client replays user/assistant pairs | server folds `getPathToRoot(leaf)` into agentcore `History` |
| compaction-as-entry | within-run only | a `compaction` entry in the conversation log |
| rich `ChatMsg[]` (human view) | client `localStorage` | projection of `message`-kind entries via a list/since API |

We keep `agent_runs` + `agent_tool_calls` exactly as-is: a **run** stays the
unit of "one agent loop / one turn of work." A **conversation** is the new
parent that runs and entries hang off.

---

## 4. Schema

```sql
-- One conversation = the durable thread three machines share.
CREATE TABLE agent_conversations (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id     UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  agent_id       UUID NOT NULL,            -- which agent answers in this thread
  title          TEXT NOT NULL DEFAULT '',
  leaf_entry_id  UUID,                     -- the movable pointer; NULL = empty
  created_by     UUID NOT NULL,            -- opener; not an ownership lock
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX agent_conversations_project_idx
  ON agent_conversations (project_id, updated_at DESC);

-- Append-only typed entries. Generalizes agent_session_log.
CREATE TABLE agent_conversation_entries (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),  -- prefer uuidv7
  conversation_id UUID NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
  parent_id       UUID REFERENCES agent_conversation_entries(id),
  seq             BIGINT NOT NULL,         -- monotonic per conversation; sync cursor
  kind            VARCHAR(32) NOT NULL,    -- message | compaction | tool_trace |
                                           -- step | model_change | branch_summary | leaf
  role            VARCHAR(16) NOT NULL DEFAULT '', -- user | assistant | system (message kind)
  author_user_id  UUID,                    -- who appended it (NULL = the agent)
  run_id          UUID REFERENCES agent_runs(id) ON DELETE SET NULL,
  turn            INT NOT NULL DEFAULT 0,
  payload_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
  token_estimate  INT NOT NULL DEFAULT 0,  -- for the compaction trigger
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (conversation_id, seq)
);
CREATE INDEX agent_conv_entries_conv_idx
  ON agent_conversation_entries (conversation_id, seq ASC);
```

Notes:
- `seq` is the **sync cursor** — clients poll/stream "give me entries `> seq`."
  Assigned atomically like `agent_session_log` does today
  (`COALESCE(MAX(seq),0)+1`), with `UNIQUE(conversation_id, seq)` as the backstop.
- `parent_id` carries the tree; for a strictly linear thread it's just
  "previous entry," but it's what makes edit-and-resend / branching free.
- `author_user_id` distinguishes machine-1 user, machine-3 user, and the agent —
  needed for multi-user rendering and audit.
- `agent_session_log` (per-run) can stay for low-level agentcore resume, or be
  folded in later; this proposal does **not** require deleting it (§9).

---

## 5. Context derivation (the reducer)

A new `internal/agentruntime/conversation.go`:

```
BuildHistory(ctx, conversationID) -> agentcore.History:
  entries := store.PathToLeaf(conversationID)          // root→leaf, seq order
  comp    := last compaction entry in entries (if any)
  if comp != nil:
     out = [system: comp.summary]
     entries = entries from comp.firstKeptEntryId onward
  for e in entries where kind == "message":
     out += {role: e.role, content: e.payload.text}
  return out
```

This **replaces the client-side replay**. `send()` stops shipping a
client-built `History`; the server derives it from the conversation. The client
still sends the new user message, which the server appends as a `message` entry
*before* building context — so the model always sees the latest turn.

`step` / `tool_trace` / `card` entries are skipped by the reducer (human-only
projection) but still rendered in the UI, exactly like the current step
timeline — now durable and shared instead of `localStorage`-only.

---

## 6. Compaction-as-an-entry

Port Pi's policy into the loop's save path (agentcore already emits
`StreamSavePoint` / token usage):

```
after a turn:
  contextTokens = sum(token_estimate of entries on path since last compaction)
  if contextTokens > contextWindow - reserveTokens:       # reserve 16k
     cut = findCutPoint(path, keepRecentTokens=20k)        # snap to turn boundary
     summary = summarize(entries before cut)               # existing summarizer
     append entry{ kind:"compaction",
                   payload:{ summary, firstKeptEntryId: cut, tokensBefore } }
```

- **Turn-boundary cut** reuses our existing notion of turn (`turn` column);
  never split a tool-call entry from its result entry.
- The summarizer is the one we already use for within-run compaction
  (note the 9router SSE-fold fix in `Chat()` applies here too).
- Because compaction is an entry, the UI can show "history compacted" inline and
  the full transcript stays recoverable — strictly better than today's
  destructive within-run compaction.

Defaults, named once: `reserveTokens: 16384`, `keepRecentTokens: 20000`,
`shouldCompact: contextTokens > contextWindow - reserveTokens`.

---

## 7. Multi-machine / multi-user (the parts Pi doesn't ship)

### 7.1 Identity
A conversation is a server row with a stable UUID. The client URL carries
`conversationId` (replacing the `localStorage` session id). Any authed member of
the project who can reach that agent can open it — machine 2 and user 3 just
`GET /conversations/:id` and stream from `seq`.

### 7.2 Concurrency — the server owns the leaf
The single hard rule: **only the server advances `leaf_entry_id`**, inside the
append transaction.

```
AppendEntry(convID, parentId, …):
  BEGIN
    seq := next per-conversation seq
    INSERT entry
    UPDATE agent_conversations
       SET leaf_entry_id = entry.id, updated_at = now()
     WHERE id = convID
  COMMIT
```

- Two users sending at once → two appends → the second's `parent_id` may be the
  pre-existing leaf (a fork). Resolution policy (config, default **last-writer
  advances leaf**): the later commit wins the leaf; the earlier append remains a
  branch reachable via `parent_id`. We surface "User X also replied" rather than
  silently dropping.
- A **run** takes a soft conversation lock (advisory: `run_id` recorded on the
  conversation while `status = running`) so two agents don't loop on the same
  thread simultaneously; a second send while running becomes a *steer/followup*
  (we already distinguish these in `chatStream` `mode`) rather than a parallel
  run.

### 7.3 Realtime fan-out
Each open client subscribes after its last `seq`. Two transports, pick one:
- **SSE per conversation** (matches our existing `streamChat` SSE infra): on
  append, publish the new entry to subscribers of that conversation id.
- **Poll `?since=<seq>`** as the floor / reconnect path (already the shape of the
  resume poll in `page.tsx`).

So machine 1 streams its own run live; machine 2 (joined mid-run) and user 3 get
the same entries via the conversation subscription — including step/tool entries,
because those are now durable entries, not transient SSE.

---

## 8. API surface

```
POST   /agent/conversations                      -> { id }            (open)
GET    /agent/conversations                       -> [ {id,title,updated_at} ]
GET    /agent/conversations/:id?since=<seq>        -> { entries[], leafSeq }
POST   /agent/conversations/:id/messages           -> append user message,
                                                       start run, stream entries (SSE)
GET    /agent/conversations/:id/stream?since=<seq>  -> SSE of new entries (join/resume)
```

`POST …/messages` is the merge of today's `agentChatStream`: it (1) appends the
user `message` entry, (2) opens an `agent_run` linked to the conversation,
(3) derives `History` server-side (§5), (4) streams the run, persisting every
emitted step/tool/token as entries. The client no longer sends `history`.

---

## 9. Migration path (incremental, non-breaking)

1. **Add tables + store methods** (`CreateConversation`, `AppendEntry`,
   `PathToLeaf`, `ConversationEntries(since)`, `AdvanceLeaf`). No behavior change.
2. **Dual-write:** chat send also opens/appends to a conversation and writes
   message entries, while the client still drives history. Read path unchanged.
   Lets us validate the store against real traffic.
3. **Flip context derivation:** server builds `History` from the conversation;
   client stops sending `history`. Keep `localStorage` as a cache only.
4. **Flip rendering:** chat loads `ChatMsg[]` from `GET …/:id`; `localStorage`
   becomes an offline cache, not the source of truth. Multi-machine works here.
5. **Realtime join:** add the conversation SSE subscription; user 3 / machine 2
   live-join. Multi-user works here.
6. **Compaction-as-entry** replaces within-run compaction.
7. **Optional cleanup:** fold `agent_session_log` into the conversation log, or
   keep it as the low-level agentcore resume seam.

Each step is shippable and reversible on its own.

---

## 10. Open questions

- **uuidv7 in Postgres:** `gen_random_uuid()` is v4 (random). For globally
  time-ordered ids without coordination we want v7 — generate app-side (Go) or
  add a `pg_uuidv7`-style function. `seq` already gives per-conversation order,
  so v7 is a nice-to-have for cross-conversation merge, not a blocker.
- **Token estimate source:** Pi uses `chars/4` + provider usage. We have real
  usage on `agent_runs`; per-entry estimate can be `chars/4` at append, trued-up
  from run usage. Don't treat it as exact (compaction trigger only).
- **Branch UX:** do we expose edit-and-resend / fork in v1, or keep the tree
  internal (linear UX) and use branches only for concurrent-writer resolution?
  Recommend internal-only first.
- **RBAC granularity:** conversation visibility = project membership + agent
  grant (we already have `agent_project_grants`). Per-conversation ACL is likely
  over-engineering for v1.

---

## 11. Why rebuild, not copy

- Pi is TypeScript + JSONL + single-user-local. The **concepts** transfer; the
  code doesn't. MIT license → we may rebuild freely.
- We already own the harder half: an append-only entry log behind agentcore's
  `SessionStore`, run+tool-call persistence, SSE streaming, resume polling, agent
  grants. This proposal **generalizes** that from run-scoped to conversation-
  scoped and adds the leaf + sync + fan-out Pi never had to build.
- The risk is scope: §5/§8 touch the chat send path, persistence, resume, and
  the agentcore seed. The phased migration (§9) keeps each step small and
  reversible.

---

## References
- Pi: <https://github.com/earendil-works/pi> — `packages/agent/src/harness/session/{session,jsonl-storage,types}.ts`, `compaction/compaction.ts`
- agentray current seam: `internal/storage/agent_session_log.go`, `internal/storage/agent_runtime.go`, `internal/app/agent_routes.go`, `web/modules/chat/`
