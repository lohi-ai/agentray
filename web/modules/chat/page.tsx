'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { Bug, PanelLeft, PanelRight } from 'lucide-react';
import { ChatLayout } from '@astryxdesign/core/Chat';
import { ToggleButton } from '@astryxdesign/core/ToggleButton';
import { HStack } from '@astryxdesign/core/HStack';
import { Badge } from '@astryxdesign/core/Badge';
import { Text } from '@astryxdesign/core/Text';
import { isSteered, type AgentResultCard, type AgentToolCall, type AgentToolTrace } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { useAgent } from '@/modules/agent/hooks';
import { useAgents } from '@/modules/agent/hooks';
import { useAgentSkills } from '@/modules/agent/hooks';
import { useMediaQuery } from '@/modules/app/hooks/media';
import { AppShell } from '@/modules/shared/components/app-shell';
import { useStackSheet, type StackSheetPanel } from '@/modules/shared/components/stack-sheet';
import { ThreadsRail, FrontDoor, Conversation, AgentMenu, type ChatMsg, type ChatStep } from './chat-parts';
import { Composer } from './composer';
import { composeMessage, readAttachment, MAX_ATTACHMENTS, type Attachment } from './message-format';

// Map a streamed tool trace onto the timeline: a completed call reconciles the
// most recent matching running step (so it flips spinner → check), or appends a
// done step if no start was seen (non-streaming tools that only emit on finish).
function toolStatus(t: AgentToolTrace): 'done' | 'blocked' | 'error' {
  return t.error ? 'error' : t.allowed ? 'done' : 'blocked';
}
function applyToolTrace(steps: ChatStep[] | undefined, t: AgentToolTrace): ChatStep[] {
  const detail = t.error || t.reason || t.result_meta || (t.allowed ? '' : 'blocked');
  const list = steps ? [...steps] : [];
  for (let i = list.length - 1; i >= 0; i--) {
    const s = list[i];
    if (s.kind === 'tool' && s.status === 'running' && s.tool === t.tool) {
      list[i] = { kind: 'tool', tool: t.tool, status: toolStatus(t), detail };
      return list;
    }
  }
  list.push({ kind: 'tool', tool: t.tool, status: toolStatus(t), detail });
  return list;
}
// Rebuild a tool step from the persisted trace, used to restore the timeline a
// reload lost (the durable trace has no error column, so allowed drives status).
function serverToolStep(tc: AgentToolCall): ChatStep {
  return { kind: 'tool', tool: tc.tool, status: tc.allowed ? 'done' : 'blocked', detail: tc.result_meta || (tc.allowed ? '' : 'blocked') };
}
import { WorkPanel, type PanelTab } from './chat-panel';
import { useChatThreads, isDraft } from './use-chat-threads';

// Stable StackSheet ids for the narrow-mode chat docks (module scope so they stay
// referentially constant across renders for effect dependency lists).
const THREADS_SHEET = 'chat-threads';
const PANEL_SHEET = 'chat-panel';

export function ChatPage() {
  const projectName = useAuthStore((s) => s.project?.name);
  const projectID = useAuthStore((s) => s.project?.id);
  const { chatStream, conversationSend, sessionRun, runs, recommendations, ackRecommendation } = useAgent();
  const { agents } = useAgents();
  const { threads, activeID, newChat, selectThread, removeThread, saveMessages, ensureConversation, loadConversation } = useChatThreads(projectID);

  // Below this width the chat column can't comfortably share space with both
  // side docks, so the rail and panel switch from docked columns to right-pinned
  // StackSheet panels (the same stacking overlay used by the daily readout). The
  // toggle buttons drive the docked flags when wide and push/close StackSheet
  // panels when narrow; their open state is read back from the live stack so the
  // toggle un-presses when the user dismisses a panel from its own close button.
  const narrow = useMediaQuery('(max-width: 1100px)');
  const [threadsOn, setThreadsOn] = useState(true);
  const [panelOn, setPanelOn] = useState(true);
  const { push, closeById, panels } = useStackSheet();
  const [debug, setDebug] = useState(false);
  const [tab, setTab] = useState<PanelTab>('recs');
  const [input, setInput] = useState('');
  const [messages, setMessages] = useState<ChatMsg[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [pickedAgentID, setPickedAgentID] = useState('');
  // Composer attachments (text files inlined into the next turn) and a transient
  // notice for files that were skipped (binary, or over the per-turn cap).
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  const [notice, setNotice] = useState('');
  // Pre-select the agent when arriving from an agent card's "Talk to agent"
  // (/chat?agent=<id>). useSearchParams is reactive — a one-shot window.location
  // read misses the param because client navigation settles the URL after mount.
  const searchParams = useSearchParams();
  const initialAgentID = searchParams.get('agent') ?? '';
  const initialQuery = searchParams.get('q') ?? '';
  const cancelled = useRef(false);
  // Aborts the in-flight fetch ONLY when the user hits Stop — never on unmount
  // or thread switch, so navigating away leaves the run to finish server-side.
  const abortRef = useRef<AbortController | null>(null);

  // Arriving via a deep-linked question (/chat?q=…, e.g. from the dashboard's
  // "Ask the agent") prefills the composer once so the user can hit send.
  const prefilled = useRef(false);
  useEffect(() => {
    if (initialQuery && !prefilled.current) { prefilled.current = true; setInput(initialQuery); }
  }, [initialQuery]);

  // Ensure there is always an active session so the first send has a thread id.
  useEffect(() => { if (!activeID) newChat(); }, [activeID, newChat]);

  // Arriving via "Talk to agent" (/chat?agent=<id>) opens a fresh chat targeted
  // at that agent rather than reusing the last thread (which carries its own).
  // Gated on projectID: useChatThreads re-points activeID to the latest stored
  // thread when the project resolves, so the fresh chat must be started after.
  const startedForAgent = useRef(false);
  useEffect(() => {
    if (initialAgentID && projectID && !startedForAgent.current) { startedForAgent.current = true; newChat(); }
  }, [initialAgentID, projectID, newChat]);

  // Load a thread's messages (and its agent) when the selection changes. Done
  // during render (guarded by the last-loaded id) so it fires on activeID change
  // but not on every save — and never sets state inside an effect.
  const [loadedID, setLoadedID] = useState<string | null>(null);
  if (activeID !== loadedID) {
    setLoadedID(activeID);
    const t = threads.find((x) => x.id === activeID);
    setMessages(t ? t.messages : []);
    setPickedAgentID(t?.agentID || initialAgentID || '');
  }

  // Server load: the conversation store is the source of truth, so when a real
  // (non-draft) thread becomes active we fetch its authoritative entries and render
  // them — this is what lets a thread started on machine 1 appear on machine 2. We
  // load once per conversation id (loadedConvRef), never while streaming, and never
  // over an unfinished turn (the resume poll owns that). send() marks a just-streamed
  // conversation as already loaded so the richer local turn (card/tools/steps) isn't
  // replaced by the leaner server projection mid-session.
  const loadedConvRef = useRef<string | null>(null);
  useEffect(() => {
    if (streaming || !activeID || isDraft(activeID)) return;
    if (loadedConvRef.current === activeID) return;
    loadedConvRef.current = activeID;
    let cancel = false;
    void (async () => {
      const msgs = await loadConversation(activeID);
      if (cancel || !msgs) return;
      setMessages((cur) => {
        const last = cur[cur.length - 1];
        if (last && !last.done) return cur; // don't clobber an in-flight turn
        return msgs;
      });
    })();
    return () => { cancel = true; };
  }, [activeID, streaming, loadConversation]);

  // Persist the active thread once a turn settles (not on every streamed token).
  const dirty = useRef(false);
  const auto = useMemo(() => agents.find((a) => a.enabled && a.is_default) || agents.find((a) => a.enabled) || agents[0], [agents]);
  const agent = useMemo(() => agents.find((a) => a.id === pickedAgentID && a.enabled) || auto, [agents, pickedAgentID, auto]);
  const agentName = agent?.name || 'Agent';
  // The current agent's skills back the composer's `/` command menu.
  const { skills } = useAgentSkills(agent?.id);

  // Read dropped/picked/pasted files into text attachments, dropping unreadable
  // ones and anything past the per-turn cap, and surfacing what was skipped.
  async function addFiles(files: File[]) {
    const results = await Promise.all(files.map(readAttachment));
    const next = [...attachments];
    const skipped: string[] = [];
    files.forEach((f, i) => {
      const a = results[i];
      if (!a) { skipped.push(`${f.name} (unsupported)`); return; }
      if (next.some((x) => x.id === a.id)) return; // already attached
      if (next.length >= MAX_ATTACHMENTS) { skipped.push(`${f.name} (max ${MAX_ATTACHMENTS})`); return; }
      next.push(a);
    });
    setAttachments(next);
    setNotice(skipped.length ? `Skipped ${skipped.join(', ')} — text files only.` : '');
  }
  // id→name lookup so each turn renders the agent that actually handled it (the
  // per-message override), not just the conversation's current agent.
  const agentNameByID = useMemo(() => Object.fromEntries(agents.map((a) => [a.id, a.name])), [agents]);
  useEffect(() => {
    if (!streaming && dirty.current && messages.length && activeID) {
      saveMessages(activeID, messages, agent?.id);
      dirty.current = false;
    }
  }, [streaming, messages, activeID, agent?.id, saveMessages]);

  // Persist mid-stream whenever a new step lands (not on every token): the user
  // may navigate away or reload while the run keeps going, and the steps already
  // shown must survive. Keyed on the live step count so token spam doesn't trigger
  // writes; the save inputs are read through a ref so that count is the only key.
  const stepSaveRef = useRef({ activeID, messages, agentID: agent?.id, saveMessages });
  useEffect(() => { stepSaveRef.current = { activeID, messages, agentID: agent?.id, saveMessages }; });
  const liveStepCount = streaming ? messages.reduce((n, m) => n + (m.steps?.length ?? 0), 0) : -1;
  useEffect(() => {
    if (liveStepCount < 0) return;
    const s = stepSaveRef.current;
    if (s.activeID && s.messages.length) s.saveMessages(s.activeID, s.messages, s.agentID);
  }, [liveStepCount]);

  // Resume on return. When a thread's last turn is still unfinished and nothing
  // is streaming in this tab, the run is finishing server-side after we left the
  // page — poll the session's latest run until it reaches a terminal status,
  // then hydrate the final answer and persist. Keyed on the thread id and the
  // streaming flag (an actively-streaming turn owns its own update path, so the
  // poller only takes over once streaming is false). saveMessages/sessionRun are
  // read through a ref so the effect's identity tracks only the thread + status.
  const pollDeps = useRef({ sessionRun, saveMessages, agentID: agent?.id });
  useEffect(() => { pollDeps.current = { sessionRun, saveMessages, agentID: agent?.id }; });
  useEffect(() => {
    if (streaming || !activeID) return;
    const last = messages[messages.length - 1];
    if (!last || last.done) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      const res = await pollDeps.current.sessionRun(activeID);
      if (stopped) return;
      const run = res?.run ?? null;
      const calls = res?.toolCalls ?? [];
      if (!run || run.status === 'running') {
        // Show progress and, if a reload left this turn with no steps, rebuild the
        // tool timeline from the durable trace so the user still sees the work.
        setMessages((items) => {
          let changed = false;
          const out = items.map((m) => {
            if (m.id !== last.id) return m;
            const needProgress = m.progress !== 'Still working…';
            const needSteps = !(m.steps?.length ?? 0) && calls.length > 0;
            if (!needProgress && !needSteps) return m;
            changed = true;
            return { ...m, progress: 'Still working…', steps: needSteps ? calls.map(serverToolStep) : m.steps };
          });
          return changed ? out : items;
        });
        timer = setTimeout(tick, 2500);
        return;
      }
      const next = messages.map((m, i) =>
        i === messages.length - 1
          ? { ...m, text: run.summary || m.text, steps: m.steps?.length ? m.steps : calls.map(serverToolStep), progress: '', done: true }
          : m,
      );
      setMessages(next);
      pollDeps.current.saveMessages(activeID, next, pollDeps.current.agentID);
    };
    timer = setTimeout(tick, 2500);
    return () => { stopped = true; clearTimeout(timer); };
  }, [activeID, streaming, messages]);

  // Realtime fan-out (DESIGN-CONVERSATION-STORE.md §7). While this tab is idle on a
  // durable conversation, poll the server for turns another machine/user appended and
  // merge them in. The server projection is leaner than a locally-streamed turn (no
  // card/tools/steps), so for turns we already hold we keep the richer local fields
  // and only adopt server turns beyond what we have — a joiner sees new work without a
  // tab losing its own. Paused while streaming (that path owns updates) and while the
  // last turn is unfinished (the resume poll owns that). loadConversation is read
  // through a ref so the interval's identity tracks only the thread + streaming flag.
  const fanoutRef = useRef({ loadConversation, messages });
  useEffect(() => { fanoutRef.current = { loadConversation, messages }; });
  useEffect(() => {
    if (streaming || !activeID || isDraft(activeID)) return;
    let stopped = false;
    const tick = async () => {
      if (typeof document !== 'undefined' && document.hidden) return;
      const server = await fanoutRef.current.loadConversation(activeID);
      if (stopped || !server) return;
      const local = fanoutRef.current.messages;
      const last = local[local.length - 1];
      if (last && !last.done) return; // don't disturb an in-flight turn
      if (server.length <= local.length) return; // nothing new from elsewhere
      const merged = server.map((sm, i) => {
        const lm = local[i];
        if (lm && lm.prompt === sm.prompt) {
          return { ...sm, text: lm.text || sm.text, card: lm.card ?? sm.card, tools: lm.tools.length ? lm.tools : sm.tools, steps: lm.steps?.length ? lm.steps : sm.steps };
        }
        return sm;
      });
      setMessages(merged);
    };
    const timer = setInterval(() => void tick(), 4000);
    return () => { stopped = true; clearInterval(timer); };
  }, [activeID, streaming]);

  function patch(id: number, fn: (m: ChatMsg) => ChatMsg) {
    if (cancelled.current) return;
    setMessages((items) => items.map((it) => (it.id === id ? fn(it) : it)));
  }

  async function send() {
    // Fold the typed /skill commands and any attachments into the single message
    // string the conversation store carries (FE-only — there's no separate channel).
    // The same composed string is both displayed and sent, so a reloaded turn
    // re-renders identically from the server projection.
    const prompt = composeMessage(input, attachments, skills);
    if (!prompt || streaming) return;
    const id = Date.now();
    const seeded: ChatMsg[] = [...messages, { id, prompt, text: '', progress: 'Thinking…', card: null, done: false, tools: [], agentID: agent?.id, agentName }];
    setMessages(seeded);
    setInput('');
    setAttachments([]);
    setNotice('');
    setStreaming(true);
    cancelled.current = false;
    dirty.current = true;
    const handlers = {
      onRunID: (rid: string) => patch(id, (m) => ({ ...m, runID: rid })),
      onToken: (t: string) => patch(id, (m) => ({ ...m, text: m.text + t, progress: '' })),
      onProgress: (n: string) => patch(id, (m) => ({ ...m, progress: n, steps: [...(m.steps ?? []), { kind: 'progress' as const, text: n }] })),
      onToolStart: (tool: string) => patch(id, (m) => ({ ...m, steps: [...(m.steps ?? []), { kind: 'tool' as const, tool, status: 'running' as const }] })),
      onCard: (c: AgentResultCard) => patch(id, (m) => ({ ...m, card: c })),
      onTool: (t: AgentToolTrace) => patch(id, (m) => ({ ...m, tools: [...m.tools, t], steps: applyToolTrace(m.steps, t) })),
      onError: (msg: string) => patch(id, (m) => ({ ...m, text: m.text || msg })),
    };
    // Open (or reuse) the server conversation, so the thread is durable and shared.
    // If that fails (offline / no project), fall back to the legacy client-history
    // path so chat still works without the conversation store.
    let convID = activeID;
    try { convID = await ensureConversation(activeID, agent?.id); } catch { convID = activeID; }
    // Don't let the server-load effect re-fetch and replace this richer local turn
    // (card/tools/steps) once streaming ends — we already hold the freshest state.
    loadedConvRef.current = convID;
    // Persist optimistically the moment the turn is sent (under the resolved id), so
    // a mid-stream navigation leaves the unfinished turn on disk for the resume poll.
    saveMessages(convID, seeded, agent?.id);
    const ac = new AbortController();
    abortRef.current = ac;
    try {
      const useStore = !isDraft(convID);
      const result = useStore
        ? await conversationSend(convID, prompt, handlers, { signal: ac.signal, agentID: agent?.id })
        : await chatStream(prompt, handlers, messages.flatMap((m) => [
            { role: 'user' as const, content: m.prompt },
            { role: 'assistant' as const, content: m.text },
          ]), { sessionID: activeID, agentID: agent?.id, signal: ac.signal });
      if (!isSteered(result)) {
        patch(id, (m) => ({ ...m, text: m.text || result.final, card: m.card || result.card || null, route: result.route, turns: result.turns, usage: result.usage, tools: m.tools.length ? m.tools : result.tool_calls, progress: '', done: true }));
      } else {
        patch(id, (m) => ({ ...m, progress: '', done: true }));
      }
    } catch {
      patch(id, (m) => ({ ...m, progress: '', done: true }));
    } finally {
      setStreaming(false);
    }
  }

  function stop() {
    cancelled.current = true;
    abortRef.current?.abort();
    setStreaming(false);
    dirty.current = true;
    setMessages((items) => items.map((m, i) => (i === items.length - 1 ? { ...m, progress: '', done: true } : m)));
  }

  function onNew() {
    cancelled.current = true;
    setStreaming(false);
    setInput('');
    newChat();
  }

  // Switching threads aborts any in-flight stream (event handler — safe to
  // mutate the cancel ref here); render-time sync then loads the new messages.
  function onSelect(id: string) {
    cancelled.current = true;
    setStreaming(false);
    selectThread(id);
  }

  // Docks only occupy grid columns when wide; when narrow they collapse to a
  // full-width chat stage and live in StackSheet panels instead.
  const railCol = !narrow && threadsOn ? '240px' : '0';
  const panelCol = !narrow && panelOn ? '320px' : '0';

  // Narrow-mode docks are pushed onto the shared StackSheet under stable ids, so
  // each toggle's pressed state is just "is my panel currently on the stack".
  const threadsSheetOpen = panels.some((p) => p.id === THREADS_SHEET && !p.closing);
  const panelSheetOpen = panels.some((p) => p.id === PANEL_SHEET && !p.closing);

  // Build the current panel content fresh each call so a (re)push carries live
  // threads/runs/tab — selecting or creating a thread also closes the rail sheet
  // so the chat stage is visible again.
  function threadsPanel(): StackSheetPanel {
    return {
      id: THREADS_SHEET,
      title: 'Chats',
      width: 300,
      content: (
        <ThreadsRail
          bare
          threads={threads}
          activeID={activeID}
          onNew={() => { onNew(); closeById(THREADS_SHEET); }}
          onSelect={(id) => { onSelect(id); closeById(THREADS_SHEET); }}
          onDelete={removeThread}
        />
      ),
    };
  }
  function workPanel(): StackSheetPanel {
    return {
      id: PANEL_SHEET,
      title: 'Workspace',
      width: 380,
      content: (
        <WorkPanel bare tab={tab} onTab={setTab} recommendations={recommendations} runs={runs} onAck={(rid, status) => void ackRecommendation(rid, status)} />
      ),
    };
  }

  // Keep the open sheets' content live: re-push (replace-in-place) whenever the
  // data they render changes. Read the latest builders through a ref so the
  // effect keys only on the underlying data, never re-pushing in a loop. When the
  // viewport grows back to wide, dismiss any open sheets — the docks take over.
  const sheetBuild = useRef({ threadsPanel, workPanel });
  useEffect(() => { sheetBuild.current = { threadsPanel, workPanel }; });
  useEffect(() => {
    if (!narrow) { closeById(THREADS_SHEET); closeById(PANEL_SHEET); return; }
    if (threadsSheetOpen) push(sheetBuild.current.threadsPanel());
    if (panelSheetOpen) push(sheetBuild.current.workPanel());
  }, [narrow, threadsSheetOpen, panelSheetOpen, threads, activeID, tab, recommendations, runs, push, closeById]);

  return (
    <AppShell active="chat" bleed>
      <div className="flex h-full flex-col">
        <HStack justify="between" align="center" className="h-12 flex-none border-b border-[var(--color-border)] bg-[var(--color-background-card)] px-4">
          <HStack align="center" gap={2}>
            <Text weight="semibold">Chat</Text>
            <Badge variant="neutral" label={<>Project <b className="font-medium text-[var(--color-text-primary)]">{projectName || '—'}</b></>} />
          </HStack>
          <HStack align="center" gap={2}>
            <ToggleButton
              label="Threads"
              size="sm"
              icon={<PanelLeft size={14} />}
              isPressed={narrow ? threadsSheetOpen : threadsOn}
              onPressedChange={(v) => (narrow ? (v ? push(threadsPanel()) : closeById(THREADS_SHEET)) : setThreadsOn(v))}
            />
            <ToggleButton
              label="Panel"
              size="sm"
              icon={<PanelRight size={14} />}
              isPressed={narrow ? panelSheetOpen : panelOn}
              onPressedChange={(v) => (narrow ? (v ? push(workPanel()) : closeById(PANEL_SHEET)) : setPanelOn(v))}
            />
          </HStack>
        </HStack>

        <div
          className="grid min-h-0 flex-1"
          style={{ gridTemplateColumns: `${railCol} minmax(0, 1fr) ${panelCol}` }}
        >
          {!narrow && threadsOn ? <ThreadsRail threads={threads} activeID={activeID} onNew={onNew} onSelect={onSelect} onDelete={removeThread} /> : null}
          <main className="col-start-2 flex min-h-0 flex-col bg-background">
            <ChatLayout
              density="balanced"
              emptyState={<FrontDoor onPick={setInput} />}
              composer={
                <Composer
                  value={input}
                  onChange={setInput}
                  onSubmit={() => void send()}
                  onStop={stop}
                  isStopShown={streaming}
                  placeholder="Ask anything…  (type / for skills, attach files)"
                  skills={skills}
                  attachments={attachments}
                  onFiles={(files) => void addFiles(files)}
                  onRemoveAttachment={(id) => setAttachments((cur) => cur.filter((a) => a.id !== id))}
                  notice={notice}
                  footerActions={
                    <>
                      <AgentMenu agents={agents} currentID={agent?.id} currentName={agentName} onPick={setPickedAgentID} />
                      <ToggleButton label="debug traces" size="sm" icon={<Bug size={14} />} isPressed={debug} onPressedChange={setDebug} />
                    </>
                  }
                />
              }
            >
              {messages.length ? <Conversation messages={messages} agentName={agentName} agentNameByID={agentNameByID} debug={debug} /> : null}
            </ChatLayout>
          </main>
          {!narrow && panelOn ? <WorkPanel tab={tab} onTab={setTab} recommendations={recommendations} runs={runs} onAck={(rid, status) => void ackRecommendation(rid, status)} /> : null}
        </div>
      </div>
    </AppShell>
  );
}
