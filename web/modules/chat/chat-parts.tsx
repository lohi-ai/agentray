'use client';

import { useEffect, useRef, useState } from 'react';
import { Check, Copy, CornerDownLeft, LayoutDashboard, MessageSquare, Paperclip, Pencil, Plug, Plus, RotateCcw, Square, Trash2 } from 'lucide-react';
import {
  ChatMessage,
  ChatMessageBubble,
  ChatMessageList,
  ChatMessageMetadata,
  ChatSystemMessage,
  ChatToolCalls,
  type ChatToolCallItem,
} from '@astryxdesign/core/Chat';
import { Spinner } from '@astryxdesign/core/Spinner';
import { List } from '@astryxdesign/core/List';
import { ListItem } from '@astryxdesign/core/List';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Markdown } from '@astryxdesign/core/Markdown';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Token } from '@astryxdesign/core/Token';
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { IconButton } from '@astryxdesign/core/IconButton';
import { DropdownMenu, type DropdownMenuOption } from '@astryxdesign/core/DropdownMenu';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { Card } from '@astryxdesign/core/Card';
import { HStack } from '@astryxdesign/core/HStack';
import { VStack } from '@astryxdesign/core/VStack';
import { Heading } from '@astryxdesign/core/Heading';
import { Text } from '@astryxdesign/core/Text';
import { TextArea } from '@astryxdesign/core/TextArea';
import type { Agent, AgentResultCard, AgentToolTrace } from '@/lib/api';
import { formatCompact, formatCost } from '@/lib/format';
import { useRouter } from 'next/navigation';
import { FIRST_RUN_PROMPT, settingsPath, startersByKind, type FirstRunHandoff as FirstRunHandoffCopy, type FirstSessionNotice } from '@/lib/ia';
import { Callout } from '@/modules/shared/components/signal-primitives';
import { FirstEventQuickstart } from '@/modules/dashboard/first-event-quickstart';
import type { ChatThread } from './use-chat-threads';
import { Chart, type ChartSpec } from '@/modules/shared/components/charts';
import type { MarkdownComponents } from '@astryxdesign/core/Markdown';
import { parseRichMessage, slugify } from './message-format';

// A single entry in the agent's visible work log for a turn — either a plain-
// language narration note or a tool call moving through running → done/blocked.
// Persisted on the message so a reload keeps the steps the user already saw.
export type ChatStep =
  | { kind: 'progress'; text: string }
  | {
      kind: 'tool';
      // The provider's id for this invocation, and the row's React key. Two
      // concurrent calls to the same tool differ in nothing else, so keying on
      // the name — or on the array index, which shifts as the list grows —
      // reconciles one onto the other and remounts rows mid-run. Absent on steps
      // restored from a trace persisted before the id was carried; those fall
      // back to positional keying, which is no worse than it used to be.
      callID?: string;
      tool: string;
      // Short human label for the call's arguments, so two rows for the same
      // tool are distinguishable at a glance.
      target?: string;
      // 'stopped' is its own status, not an error: the user ended the turn, so
      // the row is over but nothing failed. Painting it red (and counting it in
      // the work-log summary's failure tally) blames the agent for a deliberate
      // act.
      status: 'running' | 'done' | 'blocked' | 'error' | 'stopped';
      detail?: string;
      durationMS?: number;
    };

// How a settled turn ended. `done` alone can't tell "finished" from "the user
// stopped it" from "it failed", which is why a stopped turn used to render as an
// ordinary short answer. Absent on turns cached before this existed — a settled
// turn with no outcome is read as 'ok'.
export type ChatOutcome = 'ok' | 'stopped' | 'failed';

// ChatMsg is ONE message, not one exchange.
//
// It used to be a `{prompt, text}` pair, which the conversation log has never
// been: the log is an append-only list of single-role entries. Every mismatch
// followed from that — a message steered into a running turn opened a pair with
// an empty answer, a stopped turn (which appends no assistant entry) left a
// dangling half, and the multi-machine merge had to guess at identity by
// comparing prompt strings, so asking the same question twice collapsed the two
// turns into one. Reconciliation is now by `id`, and `id` is the server's entry
// id wherever the server knows about the message.
export type ChatMsg = {
  // Stable identity. `local:<n>` until the message is known to the server, then
  // the conversation entry's id. Never the array index, never the text.
  id: string;
  // 'system' is a transcript marker, not a turn — today only the compaction
  // seam. It renders as a divider and carries none of the fields below.
  role: 'user' | 'assistant' | 'system';
  text: string;
  // The entry's sync cursor, when it came from the server. The highest seq the
  // client holds is the watermark it asks for deltas from.
  seq?: number;
  // Derived from a turn the log hasn't closed yet (tool traces with no answer
  // behind them), so it is re-derived on every sync rather than kept. Never set
  // on a message the user or the stream produced.
  provisional?: boolean;
  // The server's own token estimate for this entry — the same number its
  // compaction trigger sums. Absent until the message has been reconciled with
  // the log, which is why the context readout lags a locally-streamed turn by
  // one sync tick rather than guessing at it.
  tokens?: number;

  // --- assistant only ---
  // Live narration while the turn runs; empty once it settles.
  progress?: string;
  card?: AgentResultCard | null;
  // Whether the message has settled. A streaming assistant message is not done;
  // a user message always is.
  done?: boolean;
  outcome?: ChatOutcome;
  tools?: AgentToolTrace[];
  // The agent's step-by-step work log (narration + tool calls), shown inline so
  // the user sees what the agent is doing, not just the answer.
  steps?: ChatStep[];
  // The backend run id, captured before the first token, so a message left in
  // flight can be matched to its (background-finishing) run on return.
  runID?: string;
  route?: string;
  turns?: number;
  usage?: { input_tokens: number; output_tokens: number; cost_usd?: number } | null;
  // The agent that produced this message (the per-message agent override).
  // agentID is stamped on the conversation entry; agentName is the resolved
  // display label. Empty falls back to the conversation's current agent — older
  // messages keep the agent that answered them after the user switches.
  agentID?: string;
  agentName?: string;

  // --- user only ---
  // Set when the message was sent *into* a running turn rather than after it:
  // 'steer' reaches the agent at its next turn boundary, 'followup' runs once
  // the current answer is finished. Rendered as a ghost bubble so it doesn't
  // read as a new question. Only ever set on a live message — the conversation
  // entry doesn't record which mode was used.
  steer?: 'steer' | 'followup';
  // false means the message never reached the agent (the request failed). It
  // stays in the transcript, labelled, rather than vanishing on a flaky network.
  delivered?: boolean;
};




export function ThreadsRail({
  threads, activeID, onNew, onSelect, onDelete, bare,
}: {
  threads: ChatThread[];
  activeID: string;
  onNew: () => void;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
  // When hosted inside a StackSheet panel (narrow viewport) drop the grid
  // placement and right border — the sheet card supplies its own framing.
  bare?: boolean;
}) {
  return (
    <aside className={`flex min-h-0 flex-col overflow-hidden ${bare ? 'flex-1' : 'col-start-1 border-r border-[var(--color-border)] bg-[var(--color-background-card)]'}`}>
      <div className="p-3">
        <Button variant="primary" label="New chat" icon={<Plus size={16} />} onClick={onNew} className="w-full" />
      </div>
      <div className="flex-1 overflow-auto px-2 pb-3">
        <List header={threads.length ? 'Recent' : 'No chats yet'} density="compact">
          {threads.map((t) => (
            <ListItem
              key={t.id}
              className="group"
              isSelected={t.id === activeID}
              onClick={() => onSelect(t.id)}
              // `.livedot` had no CSS anywhere in the repo, so this marker was an
              // invisible empty span. StatusDot is the real primitive: accent for
              // the open thread, neutral for the rest, and it carries a label so
              // the distinction isn't colour-only.
              startContent={
                <StatusDot
                  variant={t.id === activeID ? 'accent' : 'neutral'}
                  label={t.id === activeID ? 'Open chat' : 'Chat'}
                />
              }
              label={<span className="block overflow-hidden text-ellipsis whitespace-nowrap">{t.title}</span>}
              endContent={
                <IconButton
                  label="Delete chat"
                  size="sm"
                  variant="ghost"
                  icon={<Trash2 size={13} />}
                  className="opacity-0 transition-opacity group-hover:opacity-100"
                  onClick={(e) => { e.stopPropagation(); onDelete(t.id); }}
                />
              }
            />
          ))}
        </List>
      </div>
    </aside>
  );
}

// AgentMenu turns the composer's agent chip into a real switcher: it lists the
// project's enabled agents and lets the user target a specific one for the turn.
// Built on Astryx DropdownMenu (data-driven items) with a pulsing StatusDot in the
// trigger; when only one agent is enabled it degrades to a plain disabled Button.
export function AgentMenu({ agents, currentID, currentName, onPick }: { agents: Agent[]; currentID?: string; currentName: string; onPick: (id: string) => void }) {
  const enabled = agents.filter((a) => a.enabled);
  const online = <StatusDot variant="success" label="Agent online" isPulsing />;

  if (enabled.length <= 1) {
    return <Button variant="secondary" size="sm" label={currentName} icon={online} isDisabled />;
  }

  const items: DropdownMenuOption[] = enabled.map((a) => ({
    label: a.is_default ? `${a.name} · default` : a.name,
    icon: a.id === currentID ? <Check size={13} className="text-success" /> : undefined,
    onClick: () => onPick(a.id),
  }));

  return (
    <DropdownMenu
      menuWidth={200}
      placement="above"
      button={{ label: currentName, variant: 'secondary', size: 'sm', icon: online }}
      items={items}
    />
  );
}

export function FrontDoor({
  onPick,
  onAsk,
  showFirstEvent = false,
  notice = null,
}: {
  onPick: (value: string) => void;
  onAsk?: (value: string) => void;
  showFirstEvent?: boolean;
  notice?: FirstSessionNotice | null;
}) {
  const router = useRouter();
  const groups = startersByKind();
  const ask = notice?.ask ?? groups[0]?.prompts[0] ?? 'What changed in the last 7 days?';
  const go = onAsk ?? onPick;
  const action = notice?.href && (notice.kind === 'setup' || /AI key/i.test(notice.detail ?? ''))
    ? <Button variant="secondary" size="sm" label="Add AI key" onClick={() => router.push(notice.href!)} />
    : <Button variant="secondary" size="sm" label={notice?.kind === 'noticed' ? 'Write that down' : 'Ask that'} onClick={() => go(ask)} />;
  const noticed = notice?.kind === 'noticed';
  return (
    <VStack gap={6} className="mx-auto w-full max-w-[760px] px-1 pt-4">
      <VStack gap={1}>
        <Heading level={2}>{noticed ? notice.title : 'Ask Growth Lead what to do next'}</Heading>
        <Text type="supporting">
          {noticed ? notice.detail : 'One question. I’ll look at your events and recommend a single next move.'}
        </Text>
      </VStack>
      {showFirstEvent ? <FirstEventQuickstart /> : null}
      {notice && !noticed ? (
        <Callout
          tone="agentic"
          icon={<MessageSquare size={16} />}
          label={notice.kind === 'setup' ? 'Almost ready' : 'Best next step'}
          title={notice.title}
          detail={notice.detail}
          action={action}
        />
      ) : noticed ? (
        <Callout
          tone="growth"
          icon={<MessageSquare size={16} />}
          label="What to do this week"
          title="I’ll write the next move from your events — not a guess."
          detail={notice.ask}
          action={action}
        />
      ) : null}
      {groups.map((group) => (
        <VStack gap={2} key={group.kind}>
          <Text type="supporting" weight="medium" className="uppercase tracking-[0.08em]">{group.label}</Text>
          <HStack gap={2} wrap="wrap">{group.prompts.map((chip) => <Token key={chip} size="lg" label={chip} onClick={() => go(chip)} />)}</HStack>
        </VStack>
      ))}
    </VStack>
  );
}

// FirstRunPanel is the first thing a hosted stranger sees. It replaces the
// FrontDoor chip wall for a workspace that has never run an agent.
//
// The thing it fixes: signup already creates a populated Demo project, a hired
// agent, a cloned dashboard and ~2 days of events — and nothing in the UI says
// so. This panel states that the teammate already exists, names the sample data
// honestly *before* the run (so the payoff is never a bait-and-switch), and
// offers exactly one primary move: watch it work.
//
// The starter chips are kept but demoted below — this is the opening move, not
// a menu, and "connect my own product" is never buried for the user who wants
// their own data first.
export function FirstRunPanel({
  agentName,
  sampleProjectName,
  onRun,
  onPick,
  hasModelKey,
}: {
  agentName: string;
  // The seeded project the run reads. Named in the copy so the user is told
  // what they are looking at before they see a number from it.
  sampleProjectName: string;
  onRun: (prompt: string) => void;
  onPick: (value: string) => void;
  // false = we know there is no workspace model key. undefined = still loading,
  // so the panel stays optimistic rather than flashing a blocker.
  hasModelKey?: boolean;
}) {
  const router = useRouter();
  const groups = startersByKind();
  const blocked = hasModelKey === false;
  return (
    <VStack gap={6} className="mx-auto w-full max-w-[760px] px-1 pt-4">
      <VStack gap={3} align="start">
        <Badge variant="purple" label={`${agentName} · hired & ready`} />
        <VStack gap={1}>
          <Heading level={2}>Your data is already here. Want me to read it?</Heading>
          <Text type="supporting">
            {`Your workspace ships with ${sampleProjectName} — a couple of days of sample product events, so I have something to read on day one. I’ll find the weakest step in it, pin what I find to a dashboard, and then you can point me at your own product.`}
          </Text>
        </VStack>
      </VStack>

      {blocked ? (
        // The one blocker that stops the run. Events are unaffected — say so, or
        // a new user reads "add a key" as "nothing works until you pay someone".
        <Callout
          tone="warn"
          icon={<MessageSquare size={16} />}
          label="One thing first"
          title={`${agentName} needs an AI key to answer`}
          detail="Bring your own key — you pay your model provider directly, and we never mark it up. Your events keep arriving either way."
          action={<Button variant="primary" size="sm" label="Add AI key" onClick={() => router.push(settingsPath('ai'))} />}
        />
      ) : null}

      <HStack gap={2} wrap="wrap">
        <Button
          variant="primary"
          label="Find my weakest funnel step"
          isDisabled={blocked}
          onClick={() => onRun(FIRST_RUN_PROMPT)}
          className="![background:var(--agent)] !text-[var(--agent-foreground)]"
        />
        <Button variant="secondary" label="Connect my own product instead" onClick={() => router.push(settingsPath('keys'))} />
      </HStack>

      <VStack gap={2}>
        <Text type="supporting" weight="medium" className="uppercase tracking-[0.08em]">Or ask me something else</Text>
        <HStack gap={2} wrap="wrap">
          {groups.flatMap((group) => group.prompts).map((chip) => (
            <Token key={chip} size="lg" label={chip} onClick={() => onPick(chip)} />
          ))}
        </HStack>
      </VStack>
    </VStack>
  );
}

// FirstRunHandoff closes the first run with the two callouts the design calls
// for: the payoff (the dashboard the user just watched get built) and the next
// commitment (bring your own product). Order matters — the payoff is never
// withheld behind the upsell.
export function FirstRunHandoff({ handoff }: { handoff: FirstRunHandoffCopy }) {
  const router = useRouter();
  return (
    <VStack gap={0} align="stretch" className="mx-auto w-full max-w-[760px] px-1 pb-2">
      <Callout
        tone="growth"
        icon={<LayoutDashboard size={16} />}
        label={handoff.dashboard.label}
        title={handoff.dashboard.title}
        detail={handoff.dashboard.detail}
        action={<Button variant="primary" size="sm" label={handoff.dashboard.action} onClick={() => router.push(handoff.dashboard.href)} />}
      />
      <Callout
        tone="agentic"
        icon={<Plug size={16} />}
        label={handoff.connect.label}
        title={handoff.connect.title}
        detail={handoff.connect.detail}
        action={<Button variant="secondary" size="sm" label={handoff.connect.action} onClick={() => router.push(handoff.connect.href)} />}
      />
    </VStack>
  );
}

// Raw tool ids read like code (`run_sql`, `explore_events`); the work log shows a
// plain-language verb instead, with the raw id kept as a mono `node` chip so the
// surface stays transparent about exactly which tool ran.
const TOOL_LABELS: Record<string, string> = {
  run_sql: 'Queried data',
  explore_events: 'Explored events',
  explore_persons: 'Explored people',
  persons: 'Explored people',
  activity_summary: 'Summarised activity',
  run_insight: 'Ran an insight',
  create_chart: 'Built a chart',
  submit_recommendation: 'Drafted a recommendation',
  read_skill: 'Read a skill',
  update_plan: 'Updated the plan',
  remember: 'Saved to memory',
  http_request: 'Called an API',
  run_shell: 'Ran a command',
  computer_use: 'Used the sandbox',
};
function prettyTool(tool: string): string {
  return TOOL_LABELS[tool] ?? tool.replace(/_/g, ' ').replace(/^\w/, (c) => c.toUpperCase());
}

// toCalls projects a turn's persisted step log onto Astryx ChatToolCalls items —
// the standard "what the agent did" surface. Only tool steps map; progress
// narration is shown live (m.progress) and not retained as a row. A denied
// (blocked) tool reads as an error with its reason carried in errorMessage.
function toCalls(steps: ChatStep[] | undefined): ChatToolCallItem[] {
  if (!steps) return [];
  const out: ChatToolCallItem[] = [];
  steps.forEach((s, i) => {
    if (s.kind !== 'tool') return;
    // Astryx has three terminal statuses and none of them is "stopped"; a
    // stopped call maps to `complete` (the row is settled) and says what
    // happened in its detail, rather than to `error`, which would read as the
    // tool having failed.
    const status =
      s.status === 'running' ? 'running'
      : s.status === 'done' || s.status === 'stopped' ? 'complete'
      : 'error';
    out.push({
      // The call id is the key. Omitting it entirely would be worse than the old
      // index: Astryx then derives a key from [name, status, target, …], so the
      // row remounts on every status change and slams its detail panel shut
      // mid-run.
      key: s.callID || `${s.tool}-${i}`,
      name: prettyTool(s.tool),
      node: s.status === 'blocked' ? 'Blocked' : s.tool,
      target: s.target || undefined,
      status,
      duration: s.durationMS ? formatDuration(s.durationMS) : undefined,
      // A running call shows its partial output as it arrives; a finished one
      // shows its result summary. Only a failure moves the text to errorMessage.
      resultDetail: s.status === 'done' || s.status === 'running' || s.status === 'stopped' ? s.detail || undefined : undefined,
      errorMessage: s.status === 'blocked' ? s.detail || 'Blocked by scope' : s.status === 'error' ? s.detail : undefined,
    });
  });
  return out;
}

// formatDuration renders a tool call's wall clock the way the row reads it —
// milliseconds under a second, one decimal above.
function formatDuration(ms: number): string {
  return ms < 1000 ? `${Math.round(ms)}ms` : `${(ms / 1000).toFixed(1)}s`;
}

// workSummary is the collapsed work-log chip label once a turn has settled —
// "Worked through N steps", flagging any blocked/errored tools.
function workSummary(calls: ChatToolCallItem[]): string {
  const n = calls.length;
  const errs = calls.filter((c) => c.status === 'error').length;
  const steps = `${n} step${n > 1 ? 's' : ''}`;
  return errs ? `${steps} · ${errs} blocked` : `Worked through ${steps}`;
}

// WorkLog wraps Astryx ChatToolCalls with the right disclosure behaviour for a
// live agent turn: the step list stays open while the agent is working (so the
// user watches it move), then auto-collapses to a single chip the moment the
// turn settles — while still letting the user re-open it. The label carries the
// agent's live narration mid-turn, and a quiet summary once done.
//
// The min-w-0 wrapper is load-bearing, not cosmetic: a ChatToolCalls row is one
// flex line whose name/target spans default to min-width:auto, so at a 390px
// viewport they refuse to shrink and push the whole document into horizontal
// scroll. Forcing min-w-0 down the subtree lets them truncate instead. The `!`
// is needed because Astryx's StyleX classes otherwise win the cascade.
function WorkLog({ calls, working, label }: { calls: ChatToolCallItem[]; working: boolean; label?: string }) {
  const [expanded, setExpanded] = useState(working);
  const prevWorking = useRef(working);
  useEffect(() => {
    if (prevWorking.current && !working) setExpanded(false);
    prevWorking.current = working;
  }, [working]);
  return (
    <div className="w-full min-w-0 [&_*]:!min-w-0">
      <ChatToolCalls calls={calls} label={label} isExpanded={expanded} onExpandedChange={setExpanded} />
    </div>
  );
}

// tracesToCalls projects the backend's authoritative per-turn tool traces onto
// Astryx ChatToolCalls items for the debug surface — allowed/ok vs blocked/errored,
// with the reason/result carried in target (ok) or errorMessage (blocked/error).
function tracesToCalls(tools: AgentToolTrace[] | undefined): ChatToolCallItem[] {
  if (!tools) return [];
  return tools.map((t, i) => ({
    key: t.call_id || `${t.tool}-${i}`,
    name: prettyTool(t.tool),
    node: t.tool,
    duration: t.latency_ms ? formatDuration(t.latency_ms) : undefined,
    status: t.error ? 'error' : t.allowed ? 'complete' : 'error',
    resultDetail: t.allowed && !t.error ? (t.result_meta || undefined) : undefined,
    errorMessage: t.error || (!t.allowed ? (t.reason || 'Blocked by scope') : undefined),
  }));
}

// debugFooter is the per-message metadata line (route + turn/token/cost spend),
// rendered through the native ChatMessage metadata slot when debug is on.
function debugFooter(m: ChatMsg) {
  const tokens = m.usage ? m.usage.input_tokens + m.usage.output_tokens : 0;
  const spend = [
    m.turns ? `${m.turns} turn${m.turns > 1 ? 's' : ''}` : null,
    tokens ? `${formatCompact(tokens)} tok` : null,
    m.usage?.cost_usd ? formatCost(m.usage.cost_usd) : null,
  ].filter(Boolean).join(' · ');
  return (
    <span className="inline-flex items-center gap-2">
      {m.route ? <Badge variant="purple" label={m.route} /> : null}
      {spend ? <Text type="supporting" className="font-mono">{spend}</Text> : null}
    </span>
  );
}

// The agent renders inline graphs with a ```chart fence whose body is a JSON
// ChartSpec. Astryx Markdown shows fences as code by default, so we override the
// block-code renderer to draw a real ECharts graph for `chart`, keeping a themed
// code block for any other language. A malformed spec degrades to raw source.
function ChartFence({ source }: { source: string }) {
  let spec: ChartSpec | null = null;
  try {
    spec = JSON.parse(source) as ChartSpec;
  } catch {
    spec = null;
  }
  if (!spec || (!spec.series && !spec.slices)) {
    return <div className="py-2"><CodeBlock code={source} language="json" size="sm" width="100%" container="section" /></div>;
  }
  return <div className="py-2"><Chart spec={spec} /></div>;
}

const MD_COMPONENTS: Partial<MarkdownComponents> = {
  code({ code, language }) {
    if (language === 'chart') return <ChartFence source={code} />;
    // Native Astryx CodeBlock: syntax highlighting + copy button, 'section'
    // container so it blends into the ghost message bubble instead of drawing
    // its own card border.
    return <div className="py-1"><CodeBlock code={code} language={language || 'plaintext'} size="sm" width="100%" container="section" /></div>;
  },
};

// Renders a user turn from its stored message string: the human prose, plus
// compact chips for any /skill commands invoked and files attached. Parsing the
// same string the store holds means a reloaded turn shows the same chips as the
// one just sent — the inlined skill directives and file blocks (which the agent
// needs in full) are folded back into tidy tokens for display.
// ContextMeter is how much of the model's window this conversation is using.
// Astryx has no compact inline capacity gauge (ProgressBar is a block element
// sized for a card), so this is a 3px track and a number — small enough to live
// in the composer header without ever becoming the loudest thing on screen.
// Colour never carries the warning alone: the label says it in words.
export function ContextMeter({ percent }: { percent: number }) {
  const pct = Math.max(0, Math.min(100, Math.round(percent)));
  const tone = pct >= 90 ? 'text-danger' : pct >= 75 ? 'text-warning' : 'text-muted-foreground';
  const fill = pct >= 90 ? 'bg-danger' : pct >= 75 ? 'bg-warning' : 'bg-primary';
  const label = pct >= 90 ? `Context ${pct}% — older messages will be summarized soon` : `Context ${pct}%`;
  return (
    <div role="meter" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label={label} className="flex items-center gap-2">
      <span className="h-[3px] w-16 overflow-hidden rounded-sm bg-surface-3">
        <span className={`block h-full ${fill}`} style={{ width: `${pct}%` }} />
      </span>
      <span className={`text-xs tabular-nums ${tone}`}>{label}</span>
    </div>
  );
}

// What the transcript can do to a message. Editing and regenerating both fork
// the conversation server-side, so they need the server's entry id — a message
// this tab hasn't reconciled yet can only be copied.
export type MessageActions = {
  onEdit: (m: ChatMsg, text: string) => void;
  onRegenerate: (m: ChatMsg) => void;
  // The message with a fork in flight. Its actions go aria-disabled rather than
  // disabled, so focus is never yanked out from under a keyboard user.
  busyID?: string;
};

// A local id belongs to a message the server has never seen, so there is nothing
// to fork from. `open:` is the provisional marker for a turn the log hasn't
// closed.
const isForkable = (m: ChatMsg) => !!m.id && !m.id.startsWith('local:') && !m.id.startsWith('open:');

// Copy is the one action with no server call and no failure worth a dialog, so
// it reports itself in place: the icon becomes a check for a beat.
function CopyAction({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1400);
    return () => clearTimeout(t);
  }, [copied]);
  return (
    <Button
      isIconOnly
      size="sm"
      variant="ghost"
      label={copied ? 'Copied' : label}
      icon={copied ? <Check size={14} /> : <Copy size={14} />}
      onClick={() => { void navigator.clipboard?.writeText(text).then(() => setCopied(true)).catch(() => {}); }}
    />
  );
}

// The footer action row. Always in the DOM (opacity, not conditional render) so
// keyboard and screen-reader users reach it; the hover reveal is a pointer
// nicety. On a coarse pointer it stays visible and pads out to a 44px target.
function ActionRow({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1 opacity-60 transition-opacity focus-within:opacity-100 group-hover:opacity-100 [@media(pointer:coarse)]:opacity-100 [@media(pointer:coarse)]:[&_button]:min-h-11 [@media(pointer:coarse)]:[&_button]:min-w-11">
      {children}
    </span>
  );
}

function RetryAction({ m, actions, label }: { m: ChatMsg; actions: MessageActions; label: string }) {
  const busy = actions.busyID === m.id;
  return (
    <Button
      isIconOnly
      size="sm"
      variant="ghost"
      label={busy ? 'Working…' : label}
      icon={busy ? <Spinner size="sm" /> : <RotateCcw size={14} />}
      aria-disabled={busy}
      onClick={() => { if (!busy) actions.onRegenerate(m); }}
    />
  );
}

function UserMessage({ prompt }: { prompt: string }) {
  const { text, skills, files } = parseRichMessage(prompt);
  if (!skills.length && !files.length) return <>{prompt}</>;
  return (
    <VStack gap={2} align="stretch">
      {text ? <span>{text}</span> : null}
      {skills.length || files.length ? (
        <HStack gap={2} align="center" wrap="wrap">
          {skills.map((name) => (
            <Token key={`s-${name}`} label={`/${slugify(name)}`} size="sm" color="purple" />
          ))}
          {files.map((name) => (
            <Token key={`f-${name}`} label={name} size="sm" color="gray" icon={<Paperclip size={12} />} />
          ))}
        </HStack>
      ) : null}
    </VStack>
  );
}

export function Conversation({ messages, agentName, agentNameByID, debug, actions }: { messages: ChatMsg[]; agentName: string; agentNameByID?: Record<string, string>; debug: boolean; actions?: MessageActions }) {
  // Which message is open in the inline editor. View state, not thread state:
  // cancelling discards it and nothing outside the transcript needs to know.
  const [editingID, setEditingID] = useState('');
  return (
    <ChatMessageList density="balanced">
      {messages.map((m) => {
        // The compaction seam. A divider, not a turn: it marks where the agent's
        // memory of this conversation stops being verbatim.
        if (m.role === 'system') {
          return (
            <ChatSystemMessage key={m.id} variant="divider" className="w-full min-w-0 overflow-hidden">
              {m.text}
            </ChatSystemMessage>
          );
        }
        if (m.role === 'user') {
          return (
            <UserTurn
              key={m.id}
              m={m}
              actions={actions}
              editing={editingID === m.id}
              onEditOpen={() => setEditingID(m.id)}
              onEditClose={() => setEditingID('')}
            />
          );
        }
        return <AssistantTurn key={m.id} m={m} agentName={agentName} agentNameByID={agentNameByID} debug={debug} actions={actions} />;
      })}
    </ChatMessageList>
  );
}

// A user message. A plain one gets the filled chip; one sent *into* a running
// turn gets a ghost bubble under a label, because it isn't a new question — it
// amends the answer forming below it, and a filled chip would read as a fresh
// ask the agent had ignored.
function UserTurn({ m, actions, editing, onEditOpen, onEditClose }: {
  m: ChatMsg;
  actions?: MessageActions;
  editing: boolean;
  onEditOpen: () => void;
  onEditClose: () => void;
}) {
  const busy = actions?.busyID === m.id;
  const canFork = !!actions && isForkable(m) && !m.steer && m.delivered !== false;
  const footer = actions ? (
    <ActionRow>
      <CopyAction text={m.text} />
      {canFork ? (
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          label={busy ? 'Working…' : 'Edit'}
          icon={busy ? <Spinner size="sm" /> : <Pencil size={14} />}
          aria-disabled={busy}
          onClick={() => { if (!busy) onEditOpen(); }}
        />
      ) : null}
    </ActionRow>
  ) : undefined;

  if (editing) {
    return (
      <ChatMessage sender="user">
        <MessageEditor
          initial={m.text}
          onCancel={onEditClose}
          onSave={(text) => { onEditClose(); actions?.onEdit(m, text); }}
        />
      </ChatMessage>
    );
  }

  if (!m.steer && m.delivered !== false) {
    return (
      <ChatMessage sender="user">
        {/* `group` scopes the hover reveal to this row; the metadata wrap rule
            keeps the action cluster from pushing the timestamp off a narrow
            screen. */}
        <div className="group w-full min-w-0 [&_.astryx-chat-message-metadata]:flex-wrap">
          <ChatMessageBubble
            className="!bg-[color-mix(in_srgb,var(--primary)_16%,var(--color-background-surface))] !text-[var(--color-text-primary)] !border !border-[color-mix(in_srgb,var(--primary)_24%,transparent)]"
            metadata={footer ? <ChatMessageMetadata footer={footer} /> : undefined}
          >
            <UserMessage prompt={m.text} />
          </ChatMessageBubble>
        </div>
      </ChatMessage>
    );
  }
  return (
    <ChatMessage sender="user">
      <VStack gap={0} align="stretch">
        <HStack gap={1} align="center" justify="end">
          <CornerDownLeft size={12} className="text-[var(--color-text-secondary)]" />
          {m.delivered === false ? (
            <Text type="supporting" className="!text-[var(--danger)]">Not delivered — the agent never got this</Text>
          ) : (
            <Text type="supporting" color="secondary">
              {m.steer === 'followup' ? 'Queued — will run after this answer' : 'Steered'}
            </Text>
          )}
        </HStack>
        <ChatMessageBubble variant="ghost">
          <UserMessage prompt={m.text} />
        </ChatMessageBubble>
      </VStack>
    </ChatMessage>
  );
}

// The inline editor an edited message opens into. It states the consequence
// before the user commits: saving forks the conversation here, and everything
// the agent said after this message stops being part of the thread.
function MessageEditor({ initial, onSave, onCancel }: { initial: string; onSave: (text: string) => void; onCancel: () => void }) {
  const [text, setText] = useState(initial);
  const ref = useRef<HTMLTextAreaElement | null>(null);
  useEffect(() => { ref.current?.focus(); ref.current?.setSelectionRange(text.length, text.length); }, []); // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = text.trim().length > 0;
  return (
    <VStack gap={2} align="stretch" className="w-full min-w-0">
      {/* Astryx's TextArea, not a raw <textarea>: the design system owns the
          border, radius, and focus ring, so this box follows theme and density
          changes with every other field instead of drifting on its own. */}
      <TextArea
        ref={ref}
        label="Edit your message"
        isLabelHidden
        value={text}
        onChange={(v) => setText(v)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') { e.preventDefault(); onCancel(); }
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && dirty) { e.preventDefault(); onSave(text.trim()); }
        }}
        rows={3}
        className="w-full min-w-0"
      />
      <HStack gap={2} align="center" justify="between" wrap="wrap">
        <Text type="supporting" color="secondary">Saving will replace everything after this message.</Text>
        <HStack gap={2} align="center">
          <Button label="Cancel" size="sm" variant="ghost" onClick={onCancel} />
          <Button label="Save & rerun" size="sm" variant="primary" isDisabled={!dirty} onClick={() => onSave(text.trim())} />
        </HStack>
      </HStack>
    </VStack>
  );
}

function AssistantTurn({ m, agentName, agentNameByID, debug, actions }: { m: ChatMsg; agentName: string; agentNameByID?: Record<string, string>; debug: boolean; actions?: MessageActions }) {
  // Per-message agent label: the bubble's own stamped agent wins, then the
  // resolved id→name map, then the conversation's current agent.
  const who = m.agentName || (m.agentID && agentNameByID?.[m.agentID]) || agentName;
  const calls = toCalls(m.steps);
  const traceCalls = debug ? tracesToCalls(m.tools) : [];
  const showMeta = debug && !!(m.route || m.usage || m.turns);
  const working = !m.done;
  const stopped = m.done && m.outcome === 'stopped';
  // While the message streams, the work log's header carries the agent's live
  // narration; once settled it reads as a quiet "Worked through N steps".
  const workLabel = working ? (m.progress || 'Working…') : workSummary(calls);

  // Actions appear on settle — offering "regenerate" for an answer still being
  // written is an invitation to race the stream. What they offer depends on how
  // the message ended: a stopped or failed one gets "Try again", not the
  // "Regenerate" that implies there was an answer worth redoing.
  const forkable = !!actions && isForkable(m);
  const footer = actions && !working ? (
    <ActionRow>
      {m.text ? <CopyAction text={m.text} label={m.outcome === 'failed' ? 'Copy error' : 'Copy'} /> : null}
      {forkable ? (
        <RetryAction m={m} actions={actions} label={m.outcome === 'ok' ? 'Regenerate' : 'Try again'} />
      ) : null}
    </ActionRow>
  ) : null;
  // Actions and the debug facts share Astryx's single free-form footer slot;
  // actions lead, because they are the thing the user reaches for.
  const metaFooter = footer || showMeta ? (
    <ChatMessageMetadata footer={<>{footer}{showMeta ? debugFooter(m) : null}</>} />
  ) : undefined;
  return (
    <ChatMessage
      sender="assistant"
      avatar={<Avatar name={who} size="small" status={<StatusDot variant="success" label="Online" />} />}
    >
      {/* The sender name is the first row *inside* the body (not the
          bubble's `name` slot). Astryx adds a name-height top margin to
          the avatar whenever the bubble carries a `name`, which drops the
          avatar a line below the header; rendering the name in-body keeps
          the avatar top-aligned with it (classic avatar-leads-header). */}
      <div className="group w-full min-w-0 [&_.astryx-chat-message-metadata]:flex-wrap">
      <ChatMessageBubble
        variant="ghost"
        metadata={metaFooter}
      >
        {/* Outer gap is tight (name → body), inner VStack owns the 12px
            rhythm between the work log, the answer prose, the result card,
            and the debug trace — one token-backed gap per seam, no ad-hoc
            margins. */}
        <VStack gap={1} align="stretch">
          <Text type="supporting" weight="semibold" color="secondary">{who}</Text>
          <VStack gap={3} align="stretch">
          {calls.length ? (
            <WorkLog calls={calls} working={working} label={workLabel} />
          ) : working ? (
            // No tools yet — show the agent's live status flush-left so it
            // lines up with the agent name above and the answer body that
            // replaces it. The spinner *trails* the text as the live cue;
            // leading it would indent the text past that left edge. (It
            // replaces a `.livedot` span that had no CSS anywhere in the
            // repo — the "working" pulse rendered as nothing at all.)
            <HStack gap={2} align="center">
              <Text type="supporting">{m.progress || 'Thinking…'}</Text>
              <Spinner size="sm" />
            </HStack>
          ) : null}
          {m.text ? (
            // Native Astryx Markdown: streaming fade-in while the turn is
            // live, and headingLevelStart={3} keeps the agent's `#`/`##`
            // headings sized to fit inside the chat bubble hierarchy.
            <Markdown headingLevelStart={3} isStreaming={working} components={MD_COMPONENTS}>{m.text}</Markdown>
          ) : null}
          {m.card ? <ResultCard card={m.card} /> : null}
          {/* A stop is deliberate, so it gets the neutral system-message
              treatment and never red — the user did this on purpose, it
              is not a fault. The word carries the meaning; colour never
              does it alone. With no text at all there is no bubble to
              annotate, so the marker states that plainly instead of
              leaving an empty one. Last in the turn: it says where the
              turn ended. */}
          {stopped ? (
            <ChatSystemMessage icon={<Square size={12} />}>
              {m.text ? 'Stopped — the agent didn’t finish this answer.' : 'Stopped before the agent replied.'}
            </ChatSystemMessage>
          ) : null}
          {debug && traceCalls.length ? (
            <ChatToolCalls label="Debug trace" calls={traceCalls} defaultIsExpanded={false} />
          ) : null}
          </VStack>
        </VStack>
      </ChatMessageBubble>
      </div>
    </ChatMessage>
  );
}

// ResultCard is the agent's structured answer attachment — a titled Astryx Card
// holding either a sparkline series or a row of headline stats.
function ResultCard({ card }: { card: AgentResultCard }) {
  return (
    <Card padding={4} className="relative overflow-hidden [&::before]:absolute [&::before]:left-0 [&::before]:top-0 [&::before]:h-0.5 [&::before]:w-full [&::before]:animate-[sweep_320ms_var(--ease)_forwards] [&::before]:bg-primary [&::before]:content-['']">
      <div className="mb-2.5 flex items-baseline justify-between gap-3"><Text type="supporting">{card.title}</Text></div>
      {card.kind === 'series' && card.points?.length ? (
        <Chart spec={{ type: 'area', x: card.points.map((p) => p.label ?? ''), series: [{ data: card.points.map((p) => p.value) }], unit: card.unit, height: 130 }} />
      ) : (
        <div className="flex flex-wrap items-end gap-4">
          {(card.stats ?? []).map((s) => (
            // A stat's label and unit are body text the reader has to parse to
            // know what the number means, so they sit on --color-text-secondary
            // (6.6:1) rather than --color-text-disabled (--faint, 3.35:1 — below
            // AA, and reserved for genuinely inert text).
            <div key={s.label}><Text type="supporting">{s.label}</Text><span className="font-mono text-[26px] font-semibold tracking-[-0.02em]">{s.value}{card.unit ? <span className="text-[var(--color-text-secondary)]"> {card.unit}</span> : null}</span></div>
          ))}
        </div>
      )}
    </Card>
  );
}
