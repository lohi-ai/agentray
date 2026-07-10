'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import { AgentRayAPI, type AgentConversationEntry } from '@/lib/api';
import type { ChatMsg } from './chat-parts';

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

const storageKey = (projectID?: string) => `agentray.chat.${projectID ?? 'default'}`;
const draftID = () => `local:${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;

// isDraft marks a thread that exists only client-side (no server conversation yet),
// so the first send knows to open one. Server conversation ids are UUIDs.
export const isDraft = (id: string) => id.startsWith('local:');

function loadCache(projectID?: string): ChatThread[] {
  if (typeof window === 'undefined') return [];
  try {
    const raw = window.localStorage.getItem(storageKey(projectID));
    const parsed = raw ? (JSON.parse(raw) as ChatThread[]) : [];
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

// entriesToMessages folds a conversation's append-only entries into the ChatMsg
// pairs the UI renders. A `user` entry opens a turn (its prompt); the next
// `assistant` entry fills the answer. `tool_trace` entries (mirrored server-side per
// §7.3) attach to the open turn's step timeline, so a machine that wasn't the author
// still sees the work — not just the final answer. System entries (compaction
// summaries) are model-only and skipped. id uses the entry seq (stable per
// conversation).
type ToolTracePayload = { tool?: string; allowed?: boolean; reason?: string; error?: string; result_meta?: string };
export function entriesToMessages(entries: AgentConversationEntry[]): ChatMsg[] {
  const out: ChatMsg[] = [];
  let cur: ChatMsg | null = null;
  for (const e of entries) {
    if (e.kind === 'tool_trace') {
      if (!cur) continue;
      let p: ToolTracePayload = {};
      try { p = JSON.parse(e.payload_json || '{}') as ToolTracePayload; } catch { p = {}; }
      if (!p.tool) continue;
      const status = p.error ? 'error' : p.allowed ? 'done' : 'blocked';
      const detail = p.error || p.reason || p.result_meta || (p.allowed ? '' : 'blocked');
      cur.steps = [...(cur.steps ?? []), { kind: 'tool', tool: p.tool, status, detail }];
      continue;
    }
    if (e.kind !== 'message') continue;
    let text = '';
    try { text = String((JSON.parse(e.payload_json || '{}') as { text?: string }).text ?? ''); } catch { text = ''; }
    if (!text) continue;
    if (e.role === 'user') {
      cur = { id: e.seq, prompt: text, text: '', progress: '', card: null, done: true, tools: [] };
      out.push(cur);
    } else if (e.role === 'assistant') {
      // Stamp the turn with the agent that answered it (the per-message override),
      // so a thread that switched agents shows the right one per bubble.
      if (cur && !cur.text) { cur.text = text; cur.agentID = e.agent_id || undefined; }
      else { out.push({ id: e.seq, prompt: '', text, progress: '', card: null, done: true, tools: [], agentID: e.agent_id || undefined }); cur = null; }
    }
  }
  return out;
}

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

  const persist = useCallback((next: ChatThread[]) => {
    setThreads(next);
    if (typeof window !== 'undefined') {
      try { window.localStorage.setItem(storageKey(projectID), JSON.stringify(next)); } catch { /* quota — ignore */ }
    }
  }, [projectID]);

  // Reconcile the thread LIST from the server (authoritative for which threads
  // exist, their titles, and order). Cached messages are kept so a reopened thread
  // renders instantly; selecting it loads the authoritative entries. A draft thread
  // (unsaved) is preserved at the top so an in-progress new chat isn't dropped.
  const refreshThreads = useCallback(async () => {
    if (!api) return;
    try {
      const { conversations } = await api.listConversations();
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
    } catch { /* offline — keep the cache */ }
  }, [api, projectID]);

  // Reconcile on mount / project change. The setState lives in the promise
  // continuation (an async subscription, not a synchronous effect write), so it
  // doesn't cascade renders.
  useEffect(() => {
    if (!api) return;
    let cancel = false;
    api.listConversations().then(({ conversations }) => {
      if (cancel) return;
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
    }).catch(() => { /* offline — keep the cache */ });
    return () => { cancel = true; };
  }, [api, projectID]);

  const active = useMemo(() => threads.find((t) => t.id === activeID) ?? null, [threads, activeID]);

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
      const { entries } = await api.getConversation(id);
      const messages = entriesToMessages(entries);
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
    const title = (messages[0].prompt || 'New chat').slice(0, 48);
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

  return { threads, activeID, active, newChat, selectThread, removeThread, saveMessages, ensureConversation, loadConversation, refreshThreads };
}
