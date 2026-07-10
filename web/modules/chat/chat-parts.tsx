'use client';

import { Fragment, useEffect, useRef, useState } from 'react';
import { Check, Paperclip, Plus, Trash2 } from 'lucide-react';
import {
  ChatMessage,
  ChatMessageBubble,
  ChatMessageList,
  ChatMessageMetadata,
  ChatToolCalls,
  type ChatToolCallItem,
} from '@astryxdesign/core/Chat';
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
import type { Agent, AgentResultCard, AgentToolTrace } from '@/lib/api';
import { formatCompact, formatCost } from '@/lib/format';
import type { ChatThread } from './use-chat-threads';
import { Chart, type ChartSpec } from '@/modules/shared/components/charts';
import type { MarkdownComponents } from '@astryxdesign/core/Markdown';
import { parseRichMessage, slugify } from './message-format';

// A single entry in the agent's visible work log for a turn — either a plain-
// language narration note or a tool call moving through running → done/blocked.
// Persisted on the message so a reload keeps the steps the user already saw.
export type ChatStep =
  | { kind: 'progress'; text: string }
  | { kind: 'tool'; tool: string; status: 'running' | 'done' | 'blocked' | 'error'; detail?: string };

export type ChatMsg = {
  id: number;
  prompt: string;
  text: string;
  progress: string;
  card: AgentResultCard | null;
  done: boolean;
  tools: AgentToolTrace[];
  // The agent's step-by-step work log for this turn (narration + tool calls),
  // shown inline so the user sees what the agent is doing, not just the answer.
  steps?: ChatStep[];
  // The backend run id, captured before the first token, so a turn left in flight
  // can be matched to its (background-finishing) run on return.
  runID?: string;
  route?: string;
  turns?: number;
  usage?: { input_tokens: number; output_tokens: number; cost_usd?: number } | null;
  // The agent that handled this turn (the per-message agent override). agentID is
  // stamped on the conversation entry; agentName is the resolved display label.
  // Empty falls back to the conversation's current agent — older turns keep the
  // agent that answered them even after the user switches agents mid-thread.
  agentID?: string;
  agentName?: string;
};

const STARTERS: Record<string, string[]> = {
  Traffic: ['Where is my best traffic coming from?', 'Is AI crawler traffic growing?'],
  Product: ['Which feature drives retention?', 'Where do new users drop off?'],
  Agents: ['What did my agents do today?', 'Any agents that need attention?'],
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
              startContent={<span className={`livedot ${t.id === activeID ? 'working' : 'idle'}`} />}
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

export function FrontDoor({ onPick }: { onPick: (value: string) => void }) {
  return (
    <VStack gap={6} className="mx-auto w-full max-w-[760px] px-1">
      <VStack gap={1}>
        <Heading level={2}>What do you want to figure out?</Heading>
        <Text type="supporting">Ask in plain language. The agent will pull the supporting signals and recommend a next step.</Text>
      </VStack>
      {Object.entries(STARTERS).map(([group, chips]) => (
        <VStack gap={2} key={group}>
          <Text type="supporting" weight="medium" className="uppercase tracking-[0.08em]">{group}</Text>
          <HStack gap={2} wrap="wrap">{chips.map((chip) => <Token key={chip} size="lg" label={chip} onClick={() => onPick(chip)} />)}</HStack>
        </VStack>
      ))}
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
    const status = s.status === 'running' ? 'running' : s.status === 'done' ? 'complete' : 'error';
    out.push({
      key: `${s.tool}-${i}`,
      name: prettyTool(s.tool),
      node: s.tool,
      status,
      resultDetail: s.status === 'done' ? s.detail || undefined : undefined,
      errorMessage: s.status === 'blocked' ? s.detail || 'Blocked by scope' : s.status === 'error' ? s.detail : undefined,
    });
  });
  return out;
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
function WorkLog({ calls, working, label }: { calls: ChatToolCallItem[]; working: boolean; label?: string }) {
  const [expanded, setExpanded] = useState(working);
  const prevWorking = useRef(working);
  useEffect(() => {
    if (prevWorking.current && !working) setExpanded(false);
    prevWorking.current = working;
  }, [working]);
  return <ChatToolCalls calls={calls} label={label} isExpanded={expanded} onExpandedChange={setExpanded} />;
}

// tracesToCalls projects the backend's authoritative per-turn tool traces onto
// Astryx ChatToolCalls items for the debug surface — allowed/ok vs blocked/errored,
// with the reason/result carried in target (ok) or errorMessage (blocked/error).
function tracesToCalls(tools: AgentToolTrace[] | undefined): ChatToolCallItem[] {
  if (!tools) return [];
  return tools.map((t, i) => ({
    key: `${t.tool}-${i}`,
    name: prettyTool(t.tool),
    node: t.tool,
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

export function Conversation({ messages, agentName, agentNameByID, debug }: { messages: ChatMsg[]; agentName: string; agentNameByID?: Record<string, string>; debug: boolean }) {
  return (
    <ChatMessageList density="balanced">
      {messages.map((m) => {
        // Per-turn agent label: the bubble's own stamped agent wins, then the
        // resolved id→name map, then the conversation's current agent.
        const who = m.agentName || (m.agentID && agentNameByID?.[m.agentID]) || agentName;
        const calls = toCalls(m.steps);
        const traceCalls = debug ? tracesToCalls(m.tools) : [];
        const showMeta = debug && !!(m.route || m.usage || m.turns);
        const working = !m.done;
        // While the turn runs, the work log's header carries the agent's live
        // narration; once settled it reads as a quiet "Worked through N steps".
        const workLabel = working ? (m.progress || 'Working…') : workSummary(calls);
        return (
          <Fragment key={m.id}>
            <ChatMessage sender="user">
              <ChatMessageBubble className="!bg-[color-mix(in_srgb,var(--primary)_16%,var(--color-background-surface))] !text-[var(--color-text-primary)] !border !border-[color-mix(in_srgb,var(--primary)_24%,transparent)]">
                <UserMessage prompt={m.prompt} />
              </ChatMessageBubble>
            </ChatMessage>
            <ChatMessage
              sender="assistant"
              avatar={<Avatar name={who} size="small" status={<StatusDot variant="success" label="Online" />} />}
            >
              {/* The sender name is the first row *inside* the body (not the
                  bubble's `name` slot). Astryx adds a name-height top margin to
                  the avatar whenever the bubble carries a `name`, which drops the
                  avatar a line below the header; rendering the name in-body keeps
                  the avatar top-aligned with it (classic avatar-leads-header). */}
              <ChatMessageBubble
                variant="ghost"
                metadata={showMeta ? <ChatMessageMetadata footer={debugFooter(m)} /> : undefined}
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
                    // replaces it. The pulsing dot *trails* the text as the live
                    // cue; leading it would indent the text past that left edge
                    // (and the faint dot reads as stray padding, not a marker).
                    <HStack gap={2} align="center">
                      <Text type="supporting">{m.progress || 'Thinking…'}</Text>
                      <span className="livedot working" />
                    </HStack>
                  ) : null}
                  {m.text ? (
                    // Native Astryx Markdown: streaming fade-in while the turn is
                    // live, and headingLevelStart={3} keeps the agent's `#`/`##`
                    // headings sized to fit inside the chat bubble hierarchy.
                    <Markdown headingLevelStart={3} isStreaming={working} components={MD_COMPONENTS}>{m.text}</Markdown>
                  ) : null}
                  {m.card ? <ResultCard card={m.card} /> : null}
                  {debug && traceCalls.length ? (
                    <ChatToolCalls label="Debug trace" calls={traceCalls} defaultIsExpanded={false} />
                  ) : null}
                  </VStack>
                </VStack>
              </ChatMessageBubble>
            </ChatMessage>
          </Fragment>
        );
      })}
    </ChatMessageList>
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
            <div key={s.label}><Text type="supporting" className="!text-[var(--color-text-disabled)]">{s.label}</Text><span className="font-mono text-[26px] font-semibold tracking-[-0.02em]">{s.value}{card.unit ? <span className="text-[var(--color-text-disabled)]"> {card.unit}</span> : null}</span></div>
          ))}
        </div>
      )}
    </Card>
  );
}
