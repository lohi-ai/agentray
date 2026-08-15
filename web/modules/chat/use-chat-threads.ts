'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import { AgentRayAPI, type AgentConversation, type AgentConversationEntry } from '@/lib/api';
import { formatAgentError } from '@/lib/ia';
import type { ChatMsg, ChatStep } from './chat-parts';

// A chat thread. Since the conversation store landed (DESIGN-CONVERSATION-STORE.md
// §9 step 4) the server is the source of truth: `id` is the server conversation's
// UUID and doubles as the session id sent to the auto-route, so a thread opened on
// one machine is loaded and continued on another. localStorage is now only a cache
// for instant render / offline — never the record. A `local:` id is a not-yet-saved
// draft thread that becomes a server conversation on its first send.
export type ChatThread = {
  id: string;
  title: string;
  agentID?: string;
  messages: ChatMsg[];
  updatedAt: number;
};

// The cache key carries a schema version. ChatMsg went from one-per-exchange
// ({prompt, text}, numeric id) to one-per-message ({role, text}, string id), and
// a v1 blob read back through the v2 renderer is a thread of blank bubbles —
// worse than no cache, because the server load that would fix it is what the
// cache exists to skip. Bumping the key drops every stale blob at once; the
// server is the record, so nothing is lost beyond one render's head start.
// Bump this on any incompatible ChatMsg / ChatThread change.
const CACHE_VERSION = 2;
const storageKey = (projectID?: string) => `agentray.chat.v${CACHE_VERSION}.${projectID ?? 'default'}`;
const legacyStorageKeys = (projectID?: string) => [
  `agentray.chat.${projectID ?? 'default'}`,
  ...Array.from({ length: CACHE_VERSION - 1 }, (_, i) => `agentray.chat.v${i + 1}.${projectID ?? 'default'}`),
];

// dropLegacyCache clears the blobs written under every earlier schema. Without
// it they sit in localStorage forever — invisible, unreadable, and counting
// against the origin's quota, which is what makes a later `setItem` throw.
function dropLegacyCache(projectID?: string) {
  if (typeof window === 'undefined') return;
  for (const key of legacyStorageKeys(projectID)) {
    try { window.localStorage.removeItem(key); } catch { /* private mode — ignore */ }
  }
}
const CACHED_ROLES = new Set<string>(['user', 'assistant', 'system']);
const draftID = () => `local:${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;

// isDraft marks a thread that exists only client-side (no server conversation yet),
// so the first send knows to open one. Server conversation ids are UUIDs.
export const isDraft = (id: string) => id.startsWith('local:');

function loadCache(projectID?: string): ChatThread[] {
  if (typeof window === 'undefined') return [];
  dropLegacyCache(projectID);
  try {
    const raw = window.localStorage.getItem(storageKey(projectID));
    const parsed = raw ? (JSON.parse(raw) as ChatThread[]) : [];
    if (!Array.isArray(parsed)) return [];
    // A blob can still be from a build mid-version. Keep only rows whose
    // messages are the current shape rather than trusting the key alone.
    return parsed.filter((t) => t && typeof t.id === 'string' && Array.isArray(t.messages)
      && t.messages.every((m) => m && typeof m.id === 'string' && CACHED_ROLES.has(m.role)));
  } catch {
    return [];
  }
}

// entriesToMessages renders a conversation's append-only entries as messages —
// one message per entry, in log order, no pairing. That is a straight
// translation now that ChatMsg is a single message: the log is already the
// shape the UI wants, and the old fold (open a turn on `user`, fill it on
// `assistant`) was the source of every reload artifact — a steered message
// opened a turn with an empty answer, and a stopped turn left a dangling half.
//
// `tool_trace` entries (mirrored server-side per §7.3) attach to the assistant
// message they precede, so a machine that wasn't the author still sees the
// work, not just the final answer. System entries (compaction summaries) are
// model-only and skipped. `id` is the entry's own id and `seq` its cursor, so
// reconciliation and the `?since=` watermark both key off the server's record.
type ToolTracePayload = { call_id?: string; tool?: string; target?: string; allowed?: boolean; reason?: string; error?: string; result_meta?: string };
export function entriesToMessages(entries: AgentConversationEntry[], leafEntryID?: string): ChatMsg[] {
  return renderEntries(entries, leafEntryID).messages;
}

// activePath narrows a full log to the branch the conversation currently points
// at. Editing a message or regenerating an answer forks the tree — the store
// keeps the old branch (nothing is destroyed) and GET returns every entry, so
// without this the transcript shows the message the user just replaced sitting
// above its replacement. Every append parents to the leaf, traces included, so
// the active branch is exactly the ancestor chain of `leaf_entry_id`.
//
// A window that doesn't contain the leaf (a `?since=` delta, an older cache)
// can't be walked, so it is returned untouched — deltas are appends on the
// current branch by construction, and the full load that follows a fork is what
// prunes.
function activePath(entries: AgentConversationEntry[], leafEntryID?: string): AgentConversationEntry[] {
  if (!leafEntryID) return entries;
  const byID = new Map(entries.map((e) => [e.id, e]));
  if (!byID.has(leafEntryID)) return entries;
  const keep = new Set<string>();
  for (let id: string | undefined = leafEntryID; id && byID.has(id) && !keep.has(id);) {
    keep.add(id);
    id = byID.get(id)?.parent_id;
  }
  return entries.filter((e) => keep.has(e.id));
}

// renderEntries is entriesToMessages plus the sync cursor a caller should ask
// from next. They differ when the log ends in tool traces: those belong to a
// turn whose answer hasn't been written yet, so the cursor stops short of them
// and the next poll reads them again alongside the answer they explain.
export function renderEntries(all: AgentConversationEntry[], leafEntryID?: string): { messages: ChatMsg[]; seq: number } {
  const entries = activePath(all, leafEntryID);
  const out: ChatMsg[] = [];
  // Traces stream before the answer they explain, so they buffer here until the
  // assistant message that closes the turn arrives.
  let pending: ChatStep[] = [];
  // The cursor to resume from: advanced only past entries that produced a
  // settled message, so a half-written turn is re-read rather than skipped.
  let seq = 0;
  for (const e of entries) {
    if (e.kind === 'tool_trace') {
      let p: ToolTracePayload = {};
      try { p = JSON.parse(e.payload_json || '{}') as ToolTracePayload; } catch { p = {}; }
      if (!p.tool) continue;
      const status = p.error ? 'error' : p.allowed ? 'done' : 'blocked';
      const detail = p.error || p.reason || p.result_meta || (p.allowed ? '' : 'blocked');
      // call_id and target come from the mirrored trace, so a reloaded timeline
      // keys and labels its rows exactly the way the streaming one did.
      pending.push({ kind: 'tool', callID: p.call_id, tool: p.tool, target: p.target, status, detail });
      continue;
    }
    // A compaction folded everything above it into a summary the model still
    // sees but the reader no longer does. Marking the seam is how the transcript
    // stops silently disagreeing with the agent's memory of it.
    if (e.kind === 'compaction') {
      out.push({ id: entryID(e), role: 'system', text: 'Older messages summarized', seq: e.seq, done: true });
      seq = Math.max(seq, e.seq);
      continue;
    }
    if (e.kind !== 'message') { seq = Math.max(seq, e.seq); continue; }
    let text = '';
    try { text = String((JSON.parse(e.payload_json || '{}') as { text?: string }).text ?? ''); } catch { text = ''; }
    if (!text) { seq = Math.max(seq, e.seq); continue; }
    if (e.role === 'user') {
      out.push({ id: entryID(e), role: 'user', text, seq: e.seq, done: true, tokens: e.token_estimate });
      seq = Math.max(seq, e.seq);
      continue;
    }
    if (e.role !== 'assistant') { seq = Math.max(seq, e.seq); continue; }
    // Stamp the message with the agent that produced it (the per-message
    // override), so a thread that switched agents shows the right one per bubble.
    out.push({
      id: entryID(e), role: 'assistant', text: formatAgentError(text), seq: e.seq,
      done: true, steps: pending.length ? pending : undefined, tokens: e.token_estimate,
      agentID: e.agent_id || undefined,
    });
    seq = Math.max(seq, e.seq);
    pending = [];
  }
  // Tools ran but no answer entry followed them: either the turn was stopped, or
  // it is still running elsewhere. The work happened either way, so it shows —
  // provisionally, because the answer may still arrive. `seq` deliberately does
  // NOT cover these entries, so the next poll re-reads them with their answer.
  //
  // No outcome is claimed. Saying `stopped` here would print "Stopped before the
  // agent replied" over a run that is actively working in another tab, for its
  // whole duration; the absence of an answer entry is not evidence of a stop.
  // The run row (resume poll) and the next delta are what actually know.
  if (pending.length) {
    out.push({ id: `open:${seq}`, role: 'assistant', text: '', done: true, steps: pending, provisional: true });
  }
  return { messages: out, seq };
}

// mergeDelta folds newly-synced messages into what this tab already holds.
//
// Three cases, in order. A delta message whose id we hold is an update of a
// message we already rendered. A delta message we don't hold but whose text and
// role match an unreconciled local message is *our own* turn coming back from
// the server — it adopts the server id and keeps the richer local fields (card,
// tools, live steps) the projection doesn't carry. Anything else is new work
// from another machine or tab, and is appended.
export function mergeDelta(local: ChatMsg[], delta: ChatMsg[]): ChatMsg[] {
  if (!delta.length) return local;
  // Provisional messages are re-derived from the log every poll, so the stale
  // copy goes before the fresh one lands — otherwise a stopped turn's work log
  // accumulates a duplicate per tick.
  const out = local.filter((m) => !m.provisional);
  const byID = new Map(out.map((m, i) => [m.id, i]));
  for (const d of delta) {
    const at = byID.get(d.id);
    if (at !== undefined) {
      out[at] = { ...out[at], ...d, steps: d.steps ?? out[at].steps, tools: out[at].tools ?? d.tools };
      continue;
    }
    const echo = out.findIndex((m) => !m.seq && m.role === d.role && m.text === d.text);
    if (echo >= 0) {
      out[echo] = { ...out[echo], id: d.id, seq: d.seq, agentID: out[echo].agentID || d.agentID };
      byID.set(d.id, echo);
      continue;
    }
    byID.set(d.id, out.length);
    out.push(d);
  }
  return out;
}

// syncSeq is the watermark to request the next delta from: the highest server
// cursor this tab holds. Zero means "nothing synced yet — load the whole thread".
export const syncSeq = (messages: ChatMsg[]) => messages.reduce((n, m) => (m.seq && m.seq > n ? m.seq : n), 0);

// syncTailID is the entry id at that watermark — the entry the server's next
// append should parent to. When it doesn't, the thread was forked elsewhere.
export function syncTailID(messages: ChatMsg[]): string {
  let seq = 0;
  let id = '';
  for (const m of messages) {
    if (m.seq && m.seq > seq) { seq = m.seq; id = m.id; }
  }
  return id;
}

// What a sync tick learned. `delta` is the tail to merge in; `replace` is the
// whole active path, because the branch this tab was on is no longer the one the
// conversation is in and merging cannot prune.
export type ConvDelta = { mode: 'delta' | 'replace'; messages: ChatMsg[] };

// entryID is the message's stable identity. The store's entry id is preferred;
// seq is the fallback for a payload shape that predates it, and is unique within
// a conversation either way.
const entryID = (e: AgentConversationEntry) => e.id || `seq:${e.seq}`;

// useChatThreads keeps a project's chat threads with the server conversation store
// as the source of truth and localStorage as a cache. The page owns streaming; this
// hook owns selection, the thread list, and the server load/create plumbing.
export function useChatThreads(projectID?: string) {
  const [threads, setThreads] = useState<ChatThread[]>(() => loadCache(projectID));
  const [activeID, setActiveID] = useState<string>(() => loadCache(projectID)[0]?.id ?? '');
  const [loadedProject, setLoadedProject] = useState(projectID);

  const api = useMemo(() => (projectID ? new AgentRayAPI(projectID) : null), [projectID]);

  // On project change, render the cache immediately (no flash) then reconcile.
  if (projectID !== loadedProject) {
    setLoadedProject(projectID);
    const cached = loadCache(projectID);
    setThreads(cached);
    setActiveID(cached[0]?.id ?? '');
  }

  // Reconcile the thread LIST from the server (authoritative for which threads
  // exist, their titles, and order). Cached messages are kept so a reopened thread
  // renders instantly; selecting it loads the authoritative entries. A draft thread
  // (unsaved) is preserved at the top so an in-progress new chat isn't dropped.
  const mergeServerThreads = useCallback((conversations: AgentConversation[]) => {
    setThreads((prev) => {
      const cacheByID = new Map(prev.map((t) => [t.id, t]));
      const drafts = prev.filter((t) => isDraft(t.id));
      const server: ChatThread[] = conversations.map((c) => ({
        id: c.id,
        title: c.title || cacheByID.get(c.id)?.title || 'New chat',
        agentID: c.agent_id && c.agent_id !== projectID ? c.agent_id : undefined,
        messages: cacheByID.get(c.id)?.messages ?? [],
        updatedAt: Date.parse(c.updated_at) || Date.now(),
      }));
      const merged = [...drafts, ...server];
      if (typeof window !== 'undefined') {
        try { window.localStorage.setItem(storageKey(projectID), JSON.stringify(merged)); } catch { /* ignore */ }
      }
      return merged;
    });
  }, [projectID]);

  // Reconcile on mount / project change. The setState lives in the promise
  // continuation (an async subscription, not a synchronous effect write), so it
  // doesn't cascade renders.
  useEffect(() => {
    if (!api) return;
    let cancel = false;
    api.listConversations().then(({ conversations }) => {
      if (cancel) return;
      mergeServerThreads(conversations);
    }).catch(() => { /* offline — keep the cache */ });
    return () => { cancel = true; };
  }, [api, mergeServerThreads]);

  // newChat opens a fresh DRAFT thread (client-only). The server conversation is
  // created lazily on the first send (ensureConversation), so empty chats never
  // create server rows.
  const newChat = useCallback(() => {
    const id = draftID();
    setActiveID(id);
    return id;
  }, []);

  // ensureConversation turns a draft into a real server conversation on first send,
  // returning the server id (the new activeID). A non-draft id is returned as-is.
  // The draft thread row is re-keyed to the server id so its cached messages carry
  // over without a flash.
  const ensureConversation = useCallback(async (id: string, agentID?: string): Promise<string> => {
    if (!api || !isDraft(id)) return id;
    const conv = await api.createConversation(agentID && agentID !== projectID ? agentID : '', '');
    setActiveID((cur) => (cur === id ? conv.id : cur));
    setThreads((prev) => prev.map((t) => (t.id === id ? { ...t, id: conv.id } : t)));
    return conv.id;
  }, [api, projectID]);

  // loadConversation fetches a thread's authoritative entries from the server and
  // returns the rendered messages, also updating the cache. Returns null for a draft
  // (nothing on the server yet) or on error (caller keeps the cached messages).
  const loadConversation = useCallback(async (id: string): Promise<ChatMsg[] | null> => {
    if (!api || isDraft(id)) return null;
    try {
      const { conversation, entries } = await api.getConversation(id);
      // The leaf is what makes an edited or regenerated thread read correctly:
      // GET returns every branch, and only the leaf's ancestor chain is the
      // conversation the user is actually in.
      const messages = entriesToMessages(entries, conversation?.leaf_entry_id);
      setThreads((prev) => {
        const next = prev.map((t) => (t.id === id ? { ...t, messages } : t));
        if (typeof window !== 'undefined') {
          try { window.localStorage.setItem(storageKey(projectID), JSON.stringify(next)); } catch { /* ignore */ }
        }
        return next;
      });
      return messages;
    } catch {
      return null;
    }
  }, [api, projectID]);

  // syncConversation fetches only what the server has appended past `since` and
  // returns it as messages. This is the realtime path: a poll that used to pull
  // the whole log every 4s now pulls the tail, so a long thread costs the same
  // as a short one and a tab that is merely watching doesn't re-render history.
  // `since = 0` degenerates to a full load, which is what a cold thread wants.
  //
  // `tailEntryID` is the entry this tab last rendered. A delta can only add, so
  // when another tab forks the thread (edit / regenerate repoints the leaf
  // backwards and appends a new branch) merging the delta would stack the new
  // branch under the superseded one forever. The new branch doesn't parent to
  // our tail, which is exactly how we detect it — and the honest answer then is
  // to re-read the whole active path instead of appending to a dead one.
  const syncConversation = useCallback(async (id: string, since: number, tailEntryID?: string): Promise<ConvDelta | null> => {
    if (!api || isDraft(id)) return null;
    try {
      const { conversation, entries } = await api.getConversation(id, since);
      if (!entries.length) return { mode: 'delta', messages: [] };
      if (tailEntryID && entries[0].parent_id !== tailEntryID) {
        const full = await api.getConversation(id);
        return { mode: 'replace', messages: entriesToMessages(full.entries, full.conversation?.leaf_entry_id) };
      }
      return { mode: 'delta', messages: renderEntries(entries, conversation?.leaf_entry_id).messages };
    } catch {
      return null;
    }
  }, [api]);

  const selectThread = useCallback((id: string) => setActiveID(id), []);

  // removeThread drops a thread from the local list/cache. The server conversation
  // row is retained (no destructive delete in v1); it simply stops being listed
  // locally until the next refresh, which is acceptable for a v1 hide.
  const removeThread = useCallback((id: string) => {
    setThreads((prev) => {
      const next = prev.filter((t) => t.id !== id);
      if (typeof window !== 'undefined') {
        try { window.localStorage.setItem(storageKey(projectID), JSON.stringify(next)); } catch { /* ignore */ }
      }
      return next;
    });
    setActiveID((cur) => (cur === id ? '' : cur));
  }, [projectID]);

  // saveMessages caches the active thread's messages for instant reload (the server
  // owns the durable record via the conversation store). It also upserts a draft row
  // and titles it from the first prompt so the thread rail shows it immediately.
  const saveMessages = useCallback((id: string, messages: ChatMsg[], agentID?: string) => {
    if (messages.length === 0) return;
    const title = ((messages.find((m) => m.role === 'user')?.text) || 'New chat').slice(0, 48);
    setThreads((prev) => {
      const existing = prev.find((t) => t.id === id);
      const row: ChatThread = { id, title: existing?.title && !isDraft(id) ? existing.title : title, agentID, messages, updatedAt: Date.now() };
      const next = existing ? prev.map((t) => (t.id === id ? row : t)) : [row, ...prev];
      next.sort((a, b) => b.updatedAt - a.updatedAt);
      if (typeof window !== 'undefined') {
        try { window.localStorage.setItem(storageKey(projectID), JSON.stringify(next)); } catch { /* ignore */ }
      }
      return next;
    });
  }, [projectID]);

  return { threads, activeID, newChat, selectThread, removeThread, saveMessages, ensureConversation, loadConversation, syncConversation };
}
