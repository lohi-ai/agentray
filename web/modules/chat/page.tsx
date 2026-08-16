'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { useRouter } from 'next/navigation';
import { Bug, PanelLeft, PanelRight, Plus } from 'lucide-react';
import { ChatLayout } from '@astryxdesign/core/Chat';
import { Button } from '@astryxdesign/core/Button';
import { ToggleButton } from '@astryxdesign/core/ToggleButton';
import { HStack } from '@astryxdesign/core/HStack';
import { Badge } from '@astryxdesign/core/Badge';
import { Text } from '@astryxdesign/core/Text';
import { isSteered, type AgentPlanItem, type AgentResultCard, type AgentToolTrace } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { useAgent } from '@/modules/agent/hooks';
import { useAgents } from '@/modules/agent/hooks';
import { useAgentSkills } from '@/modules/agent/hooks';
import { useMediaQuery } from '@/modules/app/hooks/media';
import { firstRunHandoff, firstSessionNotice, firstValuePath, formatAgentError, instantReply, isFirstRun, isSampleProject, needsKeyRecovery, recoveryAction, threadNeedsRecovery } from '@/lib/ia';
import { useWorkspaceModels } from '@/modules/agent/hooks';
import { useEventNames } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { useStackSheet, type StackSheetPanel } from '@/modules/shared/components/stack-sheet';
import { ThreadsRail, FrontDoor, FirstRunPanel, FirstRunHandoff, Conversation, ContextMeter, AgentMenu, type ChatMsg } from './chat-parts';
import { Composer } from './composer';
import { composeMessage, readAttachment, MAX_ATTACHMENTS, type Attachment } from './message-format';
import { CommandNames, parseCommandLine, useChatCommands } from './commands';
import { WorkPanel, type PanelTab } from './chat-panel';
import { useChatThreads, isDraft, mergeDelta, syncSeq, syncTailID } from './use-chat-threads';
import { applyToolTrace, applyToolUpdate, serverToolStep, settleOrphanSteps } from './tool-steps';

// Identity for a message this tab just made up. It carries no server cursor
// (`seq`), which is exactly how the sync merge recognises it as unreconciled and
// adopts the server's id onto it when the same message comes back.
let localCounter = 0;
const localID = () => `local:${Date.now().toString(36)}-${(localCounter++).toString(36)}`;

// The conversation store's compaction budget, mirrored from ConvContextWindow /
// ConvReserveTokens in internal/runtime/conversation.go. The server's own
// trigger assumes this window, so the readout is a faithful copy of its
// arithmetic rather than a second opinion. Change these only with those.
const CONTEXT_WINDOW = 128_000;
const CONTEXT_RESERVE = 16_384;

// Stable StackSheet ids for the narrow-mode chat docks (module scope so they stay
// referentially constant across renders for effect dependency lists).
const THREADS_SHEET = 'chat-threads';
const PANEL_SHEET = 'chat-panel';

export function ChatPage() {
  const projectName = useAuthStore((s) => s.project?.name);
  const projectID = useAuthStore((s) => s.project?.id);
  const router = useRouter();
  const { chatStream, conversationSend, editMessage, regenerateMessage, cancelChat, sessionRun, runs, runsReady, recommendations, ackRecommendation } = useAgent();
  const { agents } = useAgents();
  const { threads, activeID, newChat, selectThread, removeThread, saveMessages, ensureConversation, loadConversation, syncConversation } = useChatThreads(projectID);

  // Below this width the chat column can't comfortably share space with both
  // side docks, so the rail and panel switch from docked columns to right-pinned
  // StackSheet panels (the same stacking overlay used by the daily readout). The
  // toggle buttons drive the docked flags when wide and push/close StackSheet
  // panels when narrow; their open state is read back from the live stack so the
  // toggle un-presses when the user dismisses a panel from its own close button.
  const narrow = useMediaQuery('(max-width: 1100px)');
  const [threadsOn, setThreadsOn] = useState(false);
  const [panelOn, setPanelOn] = useState(false);
  const { push, closeById, panels } = useStackSheet();
  const [debug, setDebug] = useState(false);
  const [tab, setTab] = useState<PanelTab>('recs');
  const [input, setInput] = useState('');
  const [messages, setMessages] = useState<ChatMsg[]>([]);
  const [streaming, setStreaming] = useState(false);
  // Where a message typed mid-run should land: with the agent now (next turn
  // boundary) or after it has finished the current answer.
  const [steerMode, setSteerMode] = useState<'steer' | 'followup'>('steer');
  const [pickedAgentID, setPickedAgentID] = useState('');
  // The message whose edit/regenerate fork is in flight, so its action can say
  // so without disabling itself out from under the keyboard.
  const [forking, setForking] = useState('');
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
  const cancelledRef = useRef(false);
  // Aborts the in-flight fetch ONLY when the user hits Stop — never on unmount
  // or thread switch, so navigating away leaves the run to finish server-side.
  const abortRef = useRef<AbortController | null>(null);
  // The committed messages, readable from an async handler whose captured
  // `messages` went stale across an await (stop() and steer() both do).
  const messagesRef = useRef(messages);
  useEffect(() => { messagesRef.current = messages; });
  // The assistant message the live stream is writing into. A ref, not a closure
  // capture, because steering mid-run *moves* the target: the answer so far
  // settles above the user's redirect and the rest streams into a new message
  // below it, so the tokens keep landing where the user is looking.
  const streamTargetRef = useRef('');
  // Whether a steer has split this run's answer across more than one message.
  // The run's `final` is the WHOLE answer, so once part of it is already on
  // screen above the split, backfilling the open message from `final` would
  // print that part twice.
  const splitRef = useRef(false);

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
    setMessages(t ? t.messages.map((m) => (m.text ? { ...m, text: formatAgentError(m.text) } : m)) : []);
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
        const formatted = msgs.map((m) => (m.text ? { ...m, text: formatAgentError(m.text) } : m));
        // Old failed turns stored the raw error (or only the user line). Keep a
        // local recovery bubble so the bar survives reload until the server
        // starts persisting the formatted sentence.
        if (threadNeedsRecovery(cur) && !threadNeedsRecovery(formatted)) {
          const extra = cur.filter((m) => needsKeyRecovery(m.text ?? '') && !formatted.some((s) => s.text === m.text));
          return extra.length ? [...formatted, ...extra] : formatted;
        }
        return formatted;
      });
    })();
    return () => { cancel = true; };
  }, [activeID, streaming, loadConversation]);

  // Persist the active thread once a turn settles (not on every streamed token).
  const dirty = useRef(false);
  const auto = useMemo(() => agents.find((a) => a.enabled && a.is_default) || agents.find((a) => a.enabled) || agents[0], [agents]);
  const agent = useMemo(() => agents.find((a) => a.id === pickedAgentID && a.enabled) || auto, [agents, pickedAgentID, auto]);
  const agentName = agent?.name || 'Agent';
  // The composer's `/` menu is two vocabularies: the runtime's commands (the
  // same list the server parses against) and this agent's skills.
  const { skills } = useAgentSkills(agent?.id);
  const { commands } = useChatCommands();
  const commandNames = useMemo(() => commands.map((c) => c.name), [commands]);
  const { names: eventNames, loading: catalogLoading } = useEventNames();
  const { models, modelsLoading } = useWorkspaceModels();
  const catalogReady = !catalogLoading && !!projectID;
  const sample = isSampleProject({ name: projectName });
  const firstValue = firstValuePath({ eventNames, catalogReady, sample });
  const sessionNotice = firstSessionNotice({
    eventNames,
    catalogReady,
    hasModelKey: modelsLoading ? undefined : !!models?.has_key,
    sample,
  });

  // First session: a workspace that has never run an agent gets the FirstRunPanel
  // instead of the chip wall. The gate keys off *runs*, not events — signup seeds
  // a populated Demo project, so an events-based check would never turn off.
  // A turn is one thing the user asked, so it is counted in user messages —
  // never in `messages.length`, which is two per exchange now that a message is
  // a message. Steering makes even that a floor rather than an exact count, but
  // both consumers only care whether the user is still on their first ask.
  const turnCount = useMemo(() => messages.filter((m) => m.role === 'user').length, [messages]);
  const firstRun = isFirstRun({ runs, runsReady, turnCount });
  // Whether the seeded first-run prompt was fired from this session. Local state,
  // not derived: once the user has watched the run, the handoff belongs to that
  // turn, and a reload legitimately drops back to the ordinary thread view.
  const [firstRunFired, setFirstRunFired] = useState(false);
  const lastMessage = messages[messages.length - 1];
  // A turn that settled with no prose is the failure shape the stream leaves
  // behind (abort, network, agent error) — that gets the error surface, not a
  // "your dashboard is ready" pointing at a board nothing was pinned to.
  const handoff = firstRunHandoff({
    started: firstRunFired,
    settled: !streaming && !!lastMessage?.done,
    failed: !lastMessage?.text || needsKeyRecovery(lastMessage.text),
    turnCount,
  });

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
      // The run row's status is how a returning client (or a second tab) learns
      // the turn was stopped rather than answered — the stop may have happened
      // somewhere this tab never saw.
      const outcome: ChatMsg['outcome'] = run.status === 'stopped' ? 'stopped' : run.status === 'error' ? 'failed' : 'ok';
      const next = messages.map((m) =>
        m.id === last.id
          ? {
              ...m,
              text: run.summary || m.text,
              steps: settleOrphanSteps(m.steps?.length ? m.steps : calls.map(serverToolStep)),
              progress: '',
              done: true,
              outcome,
            }
          : m,
      );
      setMessages(next);
      pollDeps.current.saveMessages(activeID, next, pollDeps.current.agentID);
    };
    timer = setTimeout(tick, 2500);
    return () => { stopped = true; clearTimeout(timer); };
  }, [activeID, streaming, messages]);

  // Realtime fan-out (DESIGN-CONVERSATION-STORE.md §7). While this tab is idle on a
  // durable conversation, ask the server for what it has appended past our watermark
  // and merge that in. Only the tail crosses the wire, so watching a hundred-message
  // thread costs what watching a two-message one does — the old poll re-read and
  // re-rendered the entire log every four seconds.
  //
  // mergeDelta owns the reconciliation: our own just-streamed messages adopt their
  // server ids (and stop being re-appended), messages we already hold update in
  // place, and genuinely new work from another machine or tab lands at the end.
  // Paused while streaming (that path owns updates) and while the last message is
  // unfinished (the resume poll owns that).
  const fanoutRef = useRef({ syncConversation, messages, saveMessages, agentID: agent?.id });
  useEffect(() => { fanoutRef.current = { syncConversation, messages, saveMessages, agentID: agent?.id }; });
  useEffect(() => {
    if (streaming || !activeID || isDraft(activeID)) return;
    let stopped = false;
    const tick = async () => {
      if (typeof document !== 'undefined' && document.hidden) return;
      const local = fanoutRef.current.messages;
      const last = local[local.length - 1];
      if (last && !last.done) return; // don't disturb an in-flight turn
      const delta = await fanoutRef.current.syncConversation(activeID, syncSeq(local), syncTailID(local));
      if (stopped || !delta || (delta.mode === 'delta' && !delta.messages.length)) return;
      // A `replace` means another tab forked this thread: our branch is no longer
      // the conversation, and merging cannot prune. Take the server's active path.
      const next = delta.mode === 'replace' ? delta.messages : mergeDelta(fanoutRef.current.messages, delta.messages);
      if (next === fanoutRef.current.messages) return;
      setMessages(next);
      // What arrived from elsewhere has to reach the cache too, or switching
      // threads and back re-reads the stale row and loses it.
      fanoutRef.current.saveMessages(activeID, next, fanoutRef.current.agentID);
    };
    const timer = setInterval(() => void tick(), 4000);
    return () => { stopped = true; clearInterval(timer); };
  }, [activeID, streaming]);

  function patch(id: string, fn: (m: ChatMsg) => ChatMsg) {
    if (cancelledRef.current) return;
    setMessages((items) => items.map((it) => (it.id === id ? fn(it) : it)));
  }

  // Pin a goal marker directly above the message the run is writing into. It goes
  // *above* rather than onto the message because it governs the run, not the
  // answer: a steer that splits the answer in two must not print the goal twice,
  // and a reload reads it back from its own log entry in exactly this position.
  function pinGoal(id: string, goal: string) {
    if (cancelledRef.current || !goal) return;
    setMessages((items) => {
      if (items.some((m) => m.marker === 'goal' && m.goal === goal)) return items;
      const at = items.findIndex((m) => m.id === id);
      const row: ChatMsg = { id: localID(), role: 'system', marker: 'goal', goal, text: goal, done: true };
      return at < 0 ? [...items, row] : [...items.slice(0, at), row, ...items.slice(at)];
    });
  }

  // Remove a message that ended up with nothing in it — no prose, no work log.
  // An empty bubble reads as a broken answer; the absence of one reads as what
  // it is.
  function dropEmpty(id: string) {
    setMessages((items) => {
      const m = items.find((it) => it.id === id);
      if (!m || m.text || m.card || m.steps?.length) return items;
      return items.filter((it) => it.id !== id);
    });
  }

  // Tokens arrive far faster than the screen refreshes — a fast model emits
  // hundreds a second, and a setState per token re-renders the whole transcript
  // (markdown, work log and all) that many times. Buffer them and flush once per
  // frame: the same text appears at the same moment, at monitor rate instead of
  // model rate. The buffer is keyed by target so a steer mid-run can't flush a
  // half-sentence into the message it just moved away from.
  const tokenBuf = useRef<{ id: string; text: string } | null>(null);
  const tokenFrame = useRef(0);
  function flushTokens() {
    tokenFrame.current = 0;
    const buf = tokenBuf.current;
    tokenBuf.current = null;
    if (buf?.text) patch(buf.id, (m) => ({ ...m, text: m.text + buf.text, progress: '' }));
  }
  function pushToken(id: string, t: string) {
    if (tokenBuf.current && tokenBuf.current.id !== id) flushTokens();
    tokenBuf.current = { id, text: (tokenBuf.current?.text ?? '') + t };
    if (tokenFrame.current) return;
    tokenFrame.current = typeof requestAnimationFrame === 'function'
      ? requestAnimationFrame(flushTokens)
      : (setTimeout(flushTokens, 16) as unknown as number);
  }
  // A stream that ends between frames leaves its last tokens in the buffer, so
  // the final flush is part of settling — never left to the next paint.
  useEffect(() => { if (!streaming) flushTokens(); });

  // A message typed while a turn is streaming is an amendment to that run, not a
  // new question — so it goes into the log where the user said it: after the
  // answer so far, before the rest of it. The partial answer settles, the
  // redirect lands as a ghost bubble, and a fresh assistant message opens below
  // to catch the continuing stream (streamTargetRef moves with it). That is both
  // the honest reading and the one a reload reproduces, because it is the order
  // the entries were appended in.
  //
  // The server owns the fallback: if the run finished between our deciding to
  // steer and its receiving the message, it answers as an ordinary turn — which
  // lands in the new message we just opened, so the split is right either way.
  async function steer(prompt: string) {
    const mode = steerMode;
    const ask: ChatMsg = { id: localID(), role: 'user', text: prompt, done: true, steer: mode };
    const answer: ChatMsg = { id: localID(), role: 'assistant', text: '', progress: 'Thinking…', card: null, done: false, tools: [], agentID: agent?.id, agentName };
    const open = streamTargetRef.current;
    // The turn hadn't said anything yet: there is nothing to settle above the
    // redirect, so the redirect slots in ahead of the still-empty message rather
    // than leaving a blank bubble behind. (Decided out here, off the committed
    // list, so the ref move isn't a side effect inside a state updater.)
    const at = messagesRef.current.findIndex((m) => m.id === open);
    const reuse = at >= 0 && !messagesRef.current[at].text && !messagesRef.current[at].steps?.length;
    streamTargetRef.current = reuse ? open : answer.id;
    // Reusing the open message keeps one message for the whole run; splitting
    // means part of `final` is already rendered above.
    if (!reuse) splitRef.current = true;
    setMessages((items) => {
      const i = items.findIndex((m) => m.id === open);
      if (reuse && i >= 0) return [...items.slice(0, i), ask, ...items.slice(i)];
      const settled = items.map((m) => (m.id === open ? { ...m, progress: '', done: true, outcome: 'ok' as const } : m));
      return [...settled, ask, answer];
    });
    setInput('');
    setAttachments([]);
    setNotice('');
    dirty.current = true;
    try {
      const result = await conversationSend(activeID, prompt, {}, { mode, agentID: agent?.id });
      if (isSteered(result)) return; // delivered — the live run is carrying it
      // The run had already finished, so the server answered this as a fresh
      // turn. Its tokens streamed while we had no handler attached, so the answer
      // lands at once instead of typing out — into the message opened above.
      patch(streamTargetRef.current, (m) => ({
        ...m, text: formatAgentError(result.final), progress: '', done: true, outcome: 'ok' as const,
        card: result.card ?? null, tools: result.tool_calls, route: result.route,
        turns: result.turns, usage: result.usage,
      }));
    } catch {
      // The amendment never reached the agent. Leave it in the transcript and
      // label it, rather than either dropping the user's words or letting them
      // read as delivered — and drop the answer bubble it will never fill.
      setMessages((items) =>
        items
          .filter((m) => !(m.id === answer.id && !m.text))
          .map((m) => (m.id === ask.id ? { ...m, delivered: false } : m)),
      );
      streamTargetRef.current = open;
      splitRef.current = false;
    }
  }

  // Edit and regenerate both fork the conversation server-side and stream the
  // new turn. Everything after the branch point stops being part of the thread,
  // so the local transcript is truncated there before the stream opens —
  // otherwise the replaced messages sit above their replacement until a reload.
  // The original branch is never destroyed; it is simply off the active path,
  // which is what the server's leaf and the client's path filter agree on.
  async function fork(m: ChatMsg, edited?: string) {
    if (forking || streaming || isDraft(activeID)) return;
    const at = messagesRef.current.findIndex((x) => x.id === m.id);
    if (at < 0) return;
    setForking(m.id);
    setNotice('');
    const answerID = localID();
    // An edit re-asks a rewritten question; a regenerate re-asks the same one.
    // Either way the branch point is where the user last spoke.
    const kept = edited !== undefined
      ? [...messagesRef.current.slice(0, at), { ...m, text: edited, id: localID(), seq: undefined }]
      : messagesRef.current.slice(0, at);
    const answer: ChatMsg = { id: answerID, role: 'assistant', text: '', progress: 'Thinking…', card: null, done: false, tools: [], agentID: agent?.id, agentName };
    setMessages([...kept, answer]);
    setStreaming(true);
    cancelledRef.current = false;
    streamTargetRef.current = answerID;
    splitRef.current = false;
    dirty.current = true;
    const ac = new AbortController();
    abortRef.current = ac;
    // The fork rewrites the log's active path, so the cached delta watermark is
    // meaningless afterwards — force the next server load to be a full one.
    loadedConvRef.current = null;
    try {
      const result = edited !== undefined
        ? await editMessage(activeID, m.id, edited, streamHandlers(), { signal: ac.signal })
        : await regenerateMessage(activeID, m.id, streamHandlers(), { signal: ac.signal });
      // Land any buffered tokens first: settling reads `m.text` to decide
      // whether the stream produced an answer, and a frame still in flight would
      // make it look like it hadn't — then append on top of the replacement.
      flushTokens();
      // A fork is gated on `!streaming`, so there is no live run for the server
      // to steer this into — but the send path shares one result union, so the
      // branch is settled rather than assumed away.
      if (isSteered(result)) {
        patch(streamTargetRef.current, (x) => ({ ...x, progress: '', done: true, outcome: 'ok' }));
        return;
      }
      const outcome = result.stopped ? 'stopped' : 'ok';
      patch(streamTargetRef.current, (x) => ({ ...x, text: x.text || formatAgentError(result.final), card: x.card || result.card || null, route: result.route, turns: result.turns, usage: result.usage, tools: x.tools?.length ? x.tools : result.tool_calls, progress: '', done: true, outcome, steps: outcome === 'stopped' ? settleOrphanSteps(x.steps) : x.steps }));
    } catch {
      flushTokens();
      patch(streamTargetRef.current, (x) => ({ ...x, progress: '', done: true, outcome: x.outcome ?? 'failed', steps: settleOrphanSteps(x.steps) }));
      setNotice('Couldn’t rerun from that message.');
    } finally {
      setForking('');
      setStreaming(false);
    }
  }

  // The SSE handler set, shared by an ordinary send and a fork. Every handler
  // writes to whichever message is currently open, read at call time — a steer
  // mid-run moves that target, and tokens must follow it.
  function streamHandlers() {
    const at = () => streamTargetRef.current;
    return {
      onRunID: (rid: string) => patch(at(), (m) => ({ ...m, runID: rid })),
      onToken: (t: string) => pushToken(at(), t),
      onProgress: (n: string) => patch(at(), (m) => ({ ...m, progress: n, steps: [...(m.steps ?? []), { kind: 'progress' as const, text: n }] })),
      onToolStart: (c: { callID: string; tool: string; target: string }) =>
        patch(at(), (m) => ({ ...m, steps: [...(m.steps ?? []), { kind: 'tool' as const, callID: c.callID, tool: c.tool, target: c.target, status: 'running' as const }] })),
      onToolUpdate: (c: { callID: string; note: string }) => patch(at(), (m) => ({ ...m, steps: applyToolUpdate(m.steps, c.callID, c.note) })),
      // The agent rewrote its plan. A full replace, because that is what
      // update_plan is — the agent always sends the whole list, so merging
      // revisions would resurrect steps it deliberately dropped.
      onPlan: (items: AgentPlanItem[]) => patch(at(), (m) => ({ ...m, plan: items })),
      // The turn is gated. The condition is pinned above the answer being
      // written, where it stays legible while the run works toward it.
      onGoal: (goal: string) => pinGoal(at(), goal),
      onCard: (c: AgentResultCard) => patch(at(), (m) => ({ ...m, card: c })),
      onTool: (t: AgentToolTrace) => patch(at(), (m) => ({ ...m, tools: [...(m.tools ?? []), t], steps: applyToolTrace(m.steps, t) })),
      onError: (msg: string) => patch(at(), (m) => ({ ...m, text: m.text || formatAgentError(msg) })),
    };
  }

  async function send(override?: string) {
    // Fold the typed /skill commands and any attachments into the single message
    // string the conversation store carries (FE-only — there's no separate channel).
    // The same composed string is both displayed and sent, so a reloaded turn
    // re-renders identically from the server projection.
    const prompt = composeMessage(override ?? input, attachments, skills, commandNames);
    if (!prompt) return;
    // The composer stays live during a run so the user can redirect the agent
    // instead of waiting it out. Draft threads have no live run to steer.
    // One parse of the command rule for this whole function, from the module that
    // owns the rule. Re-deriving it inline is how the composer and the reloaded
    // transcript end up disagreeing about what the user typed.
    const parsed = parseCommandLine(prompt, commandNames);
    if (streaming) {
      // A handled command is not something to say to a running agent — it acts on
      // the thread. Sending it now would land its reply in the middle of the
      // answer being written, so it waits for the turn to settle and says so.
      const cmd = parsed ? commands.find((c) => c.name === parsed.name) : undefined;
      if (cmd?.handled) {
        setNotice(`Finish or stop this answer first — /${cmd.name} acts on the whole chat.`);
        return;
      }
      if (!isDraft(activeID)) await steer(prompt);
      return;
    }
    // A command is never answered by the onboarding shortcut. `/goal what should
    // we measure next` matches the funnel-shaped-question heuristic word for
    // word, and letting it settle locally would drop a gated run on the floor
    // with a written opinion in its place.
    const instant = parsed ? null : instantReply({
      eventNames,
      catalogReady,
      hasModelKey: modelsLoading ? undefined : !!models?.has_key,
    }, prompt);
    if (instant) {
      const seeded: ChatMsg[] = [...messages,
        { id: localID(), role: 'user', text: prompt, done: true },
        { id: localID(), role: 'assistant', text: instant, card: null, done: true, outcome: 'ok', tools: [], agentID: agent?.id, agentName },
      ];
      setMessages(seeded);
      setInput('');
      setAttachments([]);
      setNotice('');
      dirty.current = true;
      // Keep this turn local: a server conversation with no entries would wipe
      // the written opinion on reload. The next live ask opens the store.
      if (activeID) saveMessages(activeID, seeded, agent?.id);
      return;
    }
    const answerID = localID();
    const seeded: ChatMsg[] = [...messages,
      { id: localID(), role: 'user', text: prompt, done: true },
      { id: answerID, role: 'assistant', text: '', progress: 'Thinking…', card: null, done: false, tools: [], agentID: agent?.id, agentName },
    ];
    setMessages(seeded);
    setInput('');
    setAttachments([]);
    setNotice('');
    setStreaming(true);
    cancelledRef.current = false;
    streamTargetRef.current = answerID;
    splitRef.current = false;
    dirty.current = true;
    const at = () => streamTargetRef.current;
    const handlers = streamHandlers();
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
        // Transcript markers (the compaction divider) are a reader's aid, not
        // conversation — the legacy path ships history verbatim, so they must
        // not ride along as turns.
        : await chatStream(prompt, handlers,
            messages.filter((m) => m.role !== 'system').map((m) => ({ role: m.role as 'user' | 'assistant', content: m.text })),
            { sessionID: activeID, agentID: agent?.id, signal: ac.signal });
      // See fork(): buffered tokens land before anything reads `m.text`.
      flushTokens();
      if (!isSteered(result)) {
        // `stopped` is the server's word, so a turn stopped from another tab (or
        // one whose cancel call we couldn't confirm) still settles as Stopped
        // rather than as a suspiciously short answer.
        const outcome = result.stopped ? 'stopped' : 'ok';
        // `result.final` is the whole run's answer. Across a steer split its
        // opening is already on screen in the message above, so only a message
        // that IS the whole run may be backfilled from it.
        //
        // A stopped run is never backfilled: it has no final by definition, and
        // formatAgentError('') is the recovery copy "Something went wrong. Try
        // again." — which turns the user's own deliberate Stop into a reported
        // failure, directly under the Stopped marker that already explains it.
        const backfill = splitRef.current || result.stopped ? '' : formatAgentError(result.final);
        // Where a real `final` exists it is also authoritative, not merely a
        // fallback for a silent stream. A turn can put text on screen that is not
        // the answer it settles on: a goal-gated run whose first attempt is
        // rejected streams that attempt, is told to redo it, and streams a second
        // one — leaving both on screen, plus the gate's `STATUS: DONE` marker the
        // server strips out of `final`. Settling to `final` is what a reload shows,
        // so the live view and the durable one stop disagreeing. When there is no
        // final, `backfill` stays what it was: the recovery copy for an empty
        // message, never a replacement for text the user watched arrive.
        const settled = result.final?.trim() ? backfill : '';
        patch(at(), (m) => ({ ...m, text: settled || m.text || backfill, card: m.card || result.card || null, route: result.route, turns: result.turns, usage: result.usage, tools: m.tools?.length ? m.tools : result.tool_calls, progress: '', done: true, outcome, steps: outcome === 'stopped' ? settleOrphanSteps(m.steps) : m.steps }));
      } else {
        patch(at(), (m) => ({ ...m, progress: '', done: true, outcome: 'ok' }));
      }
      // A handled command (/compact, /clear) wrote a seam entry of its own into
      // the log, between the user's line and the reply. Our optimistic pair has
      // no such row and can't invent one in the right place, so re-read the
      // thread: the canonical projection is also what a reload would show.
      if (!isSteered(result) && result.route === 'command') loadedConvRef.current = null;
      // A split that the run never wrote into leaves an empty bubble under the
      // user's redirect. Nothing was said, so nothing is shown — the redirect
      // stands on its own rather than under a blank answer.
      if (splitRef.current) {
        dropEmpty(at());
        // The split is a live-view convenience: the server persists ONE assistant
        // entry carrying the whole answer, which matches neither half on screen.
        // Leaving it that way, the next sync finds no echo and appends the whole
        // answer a third time. Re-read the thread instead — the canonical render
        // is also what a reload would show, so the two stop disagreeing. Clearing
        // the guard is enough: setStreaming(false) below re-runs the load effect.
        loadedConvRef.current = null;
      }
    } catch {
      // A user stop aborts the fetch, which lands here too — stop() has already
      // set the outcome, so only a genuine stream failure is marked failed.
      flushTokens();
      patch(at(), (m) => ({ ...m, progress: '', done: true, outcome: m.outcome ?? 'failed', steps: settleOrphanSteps(m.steps) }));
    } finally {
      setStreaming(false);
    }
  }

  // Stop is a server-side fact, not a client that looked away: cancel the run
  // first, then settle the view. Aborting the fetch alone would leave the agent
  // running, billing, and writing an answer into a conversation the user has
  // already been told is over.
  async function stop() {
    // The open message, not the last one — a steer mid-run leaves the stream
    // writing into a message that is no longer at the tail.
    const openID = streamTargetRef.current;
    // Say what is happening while the cancel is in flight, in the one place the
    // user is already watching. The Stopped marker waits for the server's answer
    // rather than appearing on the click, so the UI never claims a halt it
    // hasn't actually got.
    setMessages((items) => items.map((m) => (m.id === openID ? { ...m, progress: 'Stopping…' } : m)));
    // Always ask the server, draft or not. The legacy client-history path
    // registers its run under the same session id the draft is streaming on, so
    // skipping the call here would abort our reader and leave the agent running,
    // billing, and writing — the exact failure Stop exists to prevent. With no
    // live run the endpoint answers `stopped:false`, which costs one request.
    const cancelled = await cancelChat(activeID);
    // `stopped:false` usually means the turn finished while the click was in
    // flight. Give the queued `done` frame a beat to land before tearing the
    // reader down, or we stamp "Stopped" over an answer the server has already
    // persisted in full — a transcript that disagrees with itself on reload.
    if (!cancelled) await new Promise((r) => setTimeout(r, 600));
    // Everything the agent already said belongs to the user — flush before
    // `cancelledRef` starts dropping writes, or the last frame of text is lost.
    flushTokens();
    cancelledRef.current = true;
    abortRef.current?.abort();
    setStreaming(false);
    dirty.current = true;
    // The turn can finish on its own during the cancel round-trip. If it did, it
    // has a real answer and settled itself — leave it alone rather than stamping
    // "Stopped" over work that actually completed. Read through the live ref: the
    // `messages` this closure captured are from before the await.
    const raced = !!messagesRef.current.find((m) => m.id === openID)?.done;
    if (!raced) {
      setMessages((items) =>
        items.map((m) =>
          m.id === openID
            ? {
                ...m,
                progress: '',
                done: true,
                outcome: 'stopped' as const,
                // Any step still spinning belongs to work that will never
                // finish. A live spinner on a turn that is over is the same lie
                // as the missing Stop marker.
                steps: settleOrphanSteps(m.steps),
              }
            : m,
        ),
      );
    }
    // The one case where the agent may genuinely still be working: we asked the
    // server to stop, it didn't confirm, and the turn hasn't settled by itself.
    // Say so rather than implying it halted.
    setNotice(cancelled || raced ? '' : 'Couldn’t stop the agent — it may still be working.');
  }

  function onNew() {
    cancelledRef.current = true;
    setStreaming(false);
    setInput('');
    // The handoff belongs to the thread the first run happened in. Leaving it
    // armed would re-show it on any other two-message thread the user opens.
    setFirstRunFired(false);
    newChat();
  }

  // Switching threads aborts any in-flight stream (event handler — safe to
  // mutate the cancel ref here); render-time sync then loads the new messages.
  function onSelect(id: string) {
    cancelledRef.current = true;
    setStreaming(false);
    setFirstRunFired(false);
    selectThread(id);
  }

  // Editing and regenerating are only offered on a durable thread — a draft has
  // no server entries to fork from — and never while a run is in flight, which
  // would race two writers onto one conversation leaf.
  const forkActions = useMemo(
    () => (isDraft(activeID) || streaming
      ? undefined
      : { onEdit: (m: ChatMsg, text: string) => void fork(m, text), onRegenerate: (m: ChatMsg) => void fork(m), busyID: forking }),
    // fork() reads everything it needs through refs; re-creating this on every
    // render of it would remount the action row mid-hover.
    [activeID, streaming, forking], // eslint-disable-line react-hooks/exhaustive-deps
  );

  // How full the model's context window is — not an estimate of our own, but a
  // mirror of the number the server compacts on: the sum of the per-entry token
  // estimates since the last compaction, against the same window-less-reserve
  // budget planCompaction uses. Reading 100% is therefore a promise that the
  // next turn folds, not a guess that it might.
  //
  // Null until at least one message carries a server estimate — a thread whose
  // turns this tab streamed and hasn't yet reconciled has nothing to report, and
  // an invented number is exactly what this surface must not show.
  const contextPercent = useMemo(() => {
    // Everything above the last compaction seam has already left the window.
    const seam = messages.map((m) => m.role).lastIndexOf('system');
    const live = messages.slice(seam + 1);
    if (!live.some((m) => m.tokens)) return null;
    const used = live.reduce((n, m) => n + (m.tokens ?? 0), 0);
    return Math.min(100, Math.round((used / (CONTEXT_WINDOW - CONTEXT_RESERVE)) * 100));
  }, [messages]);

  // The thread's current plan and goal: the last one written, wherever in the
  // thread it was written. Both are standing facts about the conversation rather
  // than about one turn — the plan the agent is working from, the condition it is
  // bound by — which is why the panel shows them at thread level while the
  // transcript keeps them in the turn they belong to.
  const livePlan = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) if (messages[i].plan?.length) return messages[i].plan!;
    return [];
  }, [messages]);
  const liveGoal = useMemo(() => {
    // A /clear ends the goal with the context it was set in: the agent no longer
    // remembers the run it was gating, so continuing to fly the banner would
    // promise something nothing is enforcing.
    const cleared = messages.map((m) => m.marker).lastIndexOf('clear');
    for (let i = messages.length - 1; i > cleared; i--) if (messages[i].marker === 'goal') return messages[i].goal ?? '';
    return '';
  }, [messages]);

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
        <WorkPanel bare tab={tab} onTab={setTab} plan={livePlan} goal={liveGoal} recommendations={recommendations} runs={runs} onAck={(rid, status) => void ackRecommendation(rid, status)} />
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
    <CommandNames.Provider value={commandNames}>
    <AppShell active="chat" bleed>
      <div className="flex h-full flex-col">
        <HStack justify="between" align="center" className="h-12 flex-none border-b border-[var(--color-border)] bg-[var(--color-background-card)] px-4">
          <HStack align="center" gap={2}>
            <Text weight="semibold">Chat</Text>
            <Badge variant="neutral" label={<>Project <b className="font-medium text-[var(--color-text-primary)]">{projectName || '—'}</b></>} />
          </HStack>
          <HStack align="center" gap={2}>
            <Button label="New chat" size="sm" variant="secondary" icon={<Plus size={14} />} onClick={onNew} />
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
            {threadNeedsRecovery(messages, { hasModelKey: !!models?.has_key }) ? (() => {
              const hit = [...messages].reverse().find((m) => needsKeyRecovery(m.text ?? ''));
              const action = recoveryAction(hit?.text ?? '');
              return (
                <div className="flex flex-none items-center justify-between gap-3 border-b border-[var(--color-border)] bg-[var(--color-background-card)] px-4 py-2">
                  <Text type="supporting">{action.detail}</Text>
                  <Button label={action.label} size="sm" variant="primary" onClick={() => router.push(action.href)} />
                </div>
              );
            })() : null}
            <ChatLayout
              density="balanced"
              emptyState={firstRun ? (
                <FirstRunPanel
                  agentName={agentName}
                  sampleProjectName={projectName || 'Demo'}
                  hasModelKey={modelsLoading ? undefined : !!models?.has_key}
                  onPick={setInput}
                  onRun={(q) => { setFirstRunFired(true); void send(q); }}
                />
              ) : (
                <FrontDoor agentName={agentName} onPick={setInput} onAsk={(q) => void send(q)} showFirstEvent={firstValue.showFirstEvent} notice={sessionNotice} />
              )}
              composer={
                <Composer
                  value={input}
                  onChange={setInput}
                  onSubmit={() => void send()}
                  onStop={() => void stop()}
                  isStopShown={streaming}
                  placeholder={streaming ? 'Redirect the agent…' : 'Ask anything…  (type / for commands and skills, attach files)'}
                  steerMode={steerMode}
                  onSteerMode={setSteerMode}
                  headerContext={contextPercent === null ? undefined : <ContextMeter percent={contextPercent} />}
                  skills={skills}
                  commands={commands}
                  attachments={attachments}
                  onFiles={(files) => void addFiles(files)}
                  onRemoveAttachment={(id) => setAttachments((cur) => cur.filter((a) => a.id !== id))}
                  notice={notice}
                  footerActions={
                    <>
                      <AgentMenu agents={agents} currentID={agent?.id} currentName={agentName} onPick={setPickedAgentID} />
                      {debug || messages.length > 2 ? (
                        <ToggleButton label="debug traces" size="sm" icon={<Bug size={14} />} isPressed={debug} onPressedChange={setDebug} />
                      ) : null}
                    </>
                  }
                />
              }
            >
              {/* One child or none, never a list of nulls: ChatLayout decides
                  whether to show `emptyState` with `Array.isArray(children) &&
                  children.length === 0`, so two conditional expressions here
                  make `children` a truthy two-null array and the empty state
                  never renders — which silently blanks the whole front door. */}
              {messages.length ? (
                <>
                  <Conversation
                    messages={messages}
                    agentName={agentName}
                    agentNameByID={agentNameByID}
                    debug={debug}
                    actions={forkActions}
                  />
                  {handoff ? <FirstRunHandoff handoff={handoff} /> : null}
                </>
              ) : null}
            </ChatLayout>
          </main>
          {!narrow && panelOn ? <WorkPanel tab={tab} onTab={setTab} plan={livePlan} goal={liveGoal} recommendations={recommendations} runs={runs} onAck={(rid, status) => void ackRecommendation(rid, status)} /> : null}
        </div>
      </div>
    </AppShell>
    </CommandNames.Provider>
  );
}
