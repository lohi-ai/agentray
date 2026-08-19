'use client';

import { useMemo, useState } from 'react';
import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, BookOpen, ChevronLeft, ChevronRight, CornerDownRight, Scissors } from 'lucide-react';
import { useAgentRun } from '@/modules/agent/hooks';
import { useRunSteps } from '@/modules/agent-monitor/hooks';
import { chapterAt, clampOffset, groupBySession } from '@/modules/agent-monitor/run-model';
import { formatCompact, formatCost, formatLatency, formatRelative } from '@/lib/format';
import type { AgentLLMCall, RunChapter } from '@/lib/api';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Intro, Loading, Panel, StatsStrip, StatusPill } from '@/modules/shared/components/signal-primitives';
import { StepInspector } from '../../lab/step-inspector';
import { StepRail } from '../../lab/step-rail';

// The run view: how a user reads a run that is too long to scroll.
//
// A short run is a list of steps and that is fine. A run of several thousand
// steps is not — and paging it does not help, because the reader still has no
// idea which page holds the part they came for. What is missing is not a smaller
// page but a SHAPE, and the run already produced one: every time its context
// filled, the loop had the model summarize what it had done, and kept the
// answer. Those summaries, in order, are a table of contents nobody had to
// write.
//
// So this page is two things stacked. The chapter list is small, complete, and
// always loaded — it is how you find the part you want. One window of steps is
// large and loaded only for the chapter you opened. Nothing here fetches a
// whole long run, which is the point: the cost of reading a 4,000-step run is
// the cost of reading the chapter you cared about.

// STEP_WINDOW is how many steps one page request carries. Steps are heavy (each
// one holds the entire context that entered it), so this is deliberately far
// below the endpoint's 200 ceiling — a chapter longer than this pages inside
// itself rather than arriving as one large body.
const STEP_WINDOW = 50;

function runDuration(started: string, finished?: string): string {
  if (!finished) return '—';
  return formatLatency(new Date(finished).getTime() - new Date(started).getTime());
}

// ChapterList is the run's table of contents. Each row says what the agent had
// accomplished by the end of the span and what the span cost, which together are
// what tell a reader whether to open it.
function ChapterList({
  chapters,
  selected,
  onSelect,
}: {
  chapters: RunChapter[];
  selected: number;
  onSelect: (chapter: RunChapter) => void;
}) {
  return (
    <div className="flex flex-col gap-1">
      {chapters.map((c) => {
        const isSelected = c.index === selected;
        return (
          <button
            key={c.index}
            onClick={() => onSelect(c)}
            className={`flex w-full flex-col items-stretch gap-1 rounded-md border px-3 py-2.5 text-left cursor-pointer ${
              isSelected
                ? 'border-agent bg-[color-mix(in_srgb,var(--agent)_10%,var(--surface-2))]'
                : 'border-[var(--color-border)] bg-[var(--color-background-muted)]'
            }`}
          >
            <div className="flex items-baseline gap-2">
              <span className="flex-none font-mono tabular-nums text-[11px] text-[var(--color-text-disabled)]">
                {String(c.index + 1).padStart(2, '0')}
              </span>
              <span className="flex-1 text-[12.5px] leading-[1.45] text-[var(--color-text-primary)]">{c.title}</span>
              {/* A chapter that ends on a compaction was closed by one; the last
                  chapter of a run was not, and saying so is more useful than
                  leaving the reader to infer it from a missing icon. */}
              {c.summary ? (
                <Scissors size={12} className="flex-none text-[var(--color-text-disabled)]" />
              ) : (
                <span className="flex-none text-[10.5px] uppercase tracking-[0.04em] text-[var(--color-text-disabled)]">open end</span>
              )}
            </div>
            <div className="flex flex-wrap gap-3 ps-[26px] text-[11px] text-[var(--color-text-secondary)]">
              <span className="font-mono tabular-nums">turns {c.first_turn}–{c.last_turn}</span>
              <span className="font-mono tabular-nums">{c.steps} steps</span>
              <span className="font-mono tabular-nums">{c.tool_calls} tool calls</span>
              <span className="font-mono tabular-nums">{formatCompact(c.tokens_in + c.tokens_out)} tok</span>
              <span className="font-mono tabular-nums">{formatCost(c.cost_usd)}</span>
            </div>
          </button>
        );
      })}
    </div>
  );
}

// DelegationSummary answers "who actually did the work" for a run that spawned
// sub-agents. A child shares its parent's run id, so without grouping by session
// its calls are indistinguishable from the parent's — one flat list that looks
// like one agent doing everything. `depth` is how far down the delegation tree
// the session sat; 0 is the run's own agent.
function DelegationSummary({ calls }: { calls: AgentLLMCall[] }) {
  const sessions = useMemo(() => groupBySession(calls), [calls]);

  if (sessions.length <= 1) {
    return (
      <p className="m-0 text-xs text-[var(--color-text-disabled)]">
        This run delegated nothing — every model call was made by the agent itself.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-1">
      {sessions.map((s) => (
        <div key={s.key} className="flex items-baseline gap-2 text-[11.5px]" style={{ paddingInlineStart: s.depth * 16 }}>
          {s.depth > 0 ? <CornerDownRight size={12} className="flex-none text-[var(--color-text-disabled)]" /> : null}
          <span className="flex-1 truncate font-mono tabular-nums text-[var(--color-text-primary)]">{s.key}</span>
          <span className="flex-none font-mono tabular-nums text-[var(--color-text-secondary)]">{s.calls} calls</span>
          <span className="flex-none font-mono tabular-nums text-[var(--color-text-secondary)]">{formatCompact(s.tokens)} tok</span>
          <span className="flex-none font-mono tabular-nums text-[var(--color-text-secondary)]">{formatCost(s.cost)}</span>
        </div>
      ))}
    </div>
  );
}

export function AgentRunPage() {
  const params = useParams<{ agentId: string; runId: string }>();
  const router = useRouter();
  const agentID = params.agentId;
  const runID = params.runId;

  const [offset, setOffset] = useState(0);
  const [selectedStep, setSelectedStep] = useState(0);

  const runQuery = useAgentRun(runID);
  const { steps, chapters, total, windowOffset, isLoading, isFetching } = useRunSteps(runID, agentID, offset, STEP_WINDOW);

  const run = runQuery.data?.run;
  const llmCalls = runQuery.data?.llm_calls ?? [];
  const toolCalls = runQuery.data?.tool_calls ?? [];

  // Which chapter the open window sits in. Derived from the offset the server
  // echoed back rather than stored, so the highlight can never drift from what
  // is actually on screen.
  const currentChapter = chapterAt(chapters, windowOffset);

  function openChapter(c: RunChapter) {
    setOffset(c.first_step);
    setSelectedStep(0);
  }

  function page(delta: number) {
    setOffset((o) => clampOffset(o + delta * STEP_WINDOW, total, STEP_WINDOW));
    setSelectedStep(0);
  }

  const back = (
    <button
      className="flex-none grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]"
      onClick={() => router.push(`/agents/${agentID}/monitor`)}
    >
      <ArrowLeft size={15} />
    </button>
  );

  if (isLoading && !run) {
    return (
      <AppShell active="monitor">
        <Intro title="Run" sub="What the agent did, chapter by chapter." />
        <Loading label="Loading run…" />
      </AppShell>
    );
  }

  const windowEnd = windowOffset + steps.length;

  return (
    <AppShell active="monitor">
      <Intro
        title={
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}>
            {back}
            {run?.summary || `Run ${runID.slice(0, 12)}`}
          </span>
        }
        sub="What the agent did, chapter by chapter."
        action={
          <>
            {run ? <StatusPill status={run.status === 'failed' ? 'attention' : run.status === 'running' ? 'working' : 'healthy'} label={run.status} grow={false} /> : null}
            <Button variant="outline" icon={<BookOpen size={15} />} onClick={() => router.push(`/agents/${agentID}/lab`)}>Open lab</Button>
          </>
        }
      />

      <StatsStrip
        stats={[
          { label: 'Steps', value: formatCompact(total) },
          { label: 'Chapters', value: String(chapters.length) },
          { label: 'Tool calls', value: formatCompact(toolCalls.length) },
          { label: 'Tokens', value: run ? formatCompact(run.token_input + run.token_output) : '—' },
          { label: 'Spend', value: run ? formatCost(run.cost_usd, run.cost_unpriced) : '—' },
          { label: 'Duration', value: run ? runDuration(run.started_at, run.finished_at) : '—' },
        ]}
      />

      <Panel title={`Chapters${chapters.length ? ` (${chapters.length})` : ''}`}>
        {chapters.length === 0 ? (
          <p className="m-0 text-xs text-[var(--color-text-disabled)]">This run has no recorded steps to divide.</p>
        ) : (
          <ChapterList chapters={chapters} selected={currentChapter?.index ?? -1} onSelect={openChapter} />
        )}
      </Panel>

      {/* The model's own account of the open chapter. It is the summary that
          CLOSED the span, so it reads as "what I had done by this point" — the
          one paragraph that makes the steps below make sense. */}
      {currentChapter?.summary ? (
        <Panel title={`Checkpoint — end of chapter ${currentChapter.index + 1}`}>
          <p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">
            {currentChapter.summary}
          </p>
        </Panel>
      ) : null}

      <Panel
        title={total ? `Steps ${windowOffset + 1}–${windowEnd} of ${total}` : 'Steps'}
        action={
          total > STEP_WINDOW ? (
            <>
              <Button variant="ghost" size="sm" icon={<ChevronLeft size={14} />} disabled={windowOffset === 0 || isFetching} onClick={() => page(-1)}>Previous</Button>
              <Button variant="ghost" size="sm" icon={<ChevronRight size={14} />} disabled={windowEnd >= total || isFetching} onClick={() => page(1)}>Next</Button>
            </>
          ) : undefined
        }
      >
        {steps.length === 0 ? (
          <EmptyState title="No steps recorded" detail="This run produced no folded steps — it may have failed before its first turn." />
        ) : (
          <>
            <StepRail steps={steps} selected={selectedStep} onSelect={setSelectedStep} />
            {steps[selectedStep] ? <StepInspector step={steps[selectedStep]} /> : null}
          </>
        )}
      </Panel>

      <Panel title="Who did the work">
        <DelegationSummary calls={llmCalls} />
        {run?.finished_at ? (
          <p className="m-0 mt-2.5 text-[11px] text-[var(--color-text-disabled)]">
            Finished {formatRelative(run.finished_at)} · triggered by {run.trigger}
          </p>
        ) : null}
      </Panel>
    </AppShell>
  );
}
