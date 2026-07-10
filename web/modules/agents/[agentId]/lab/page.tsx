'use client';

import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, ChevronRight, History, Play, Save, Scissors, Send, Square, StepForward, X } from 'lucide-react';
import { Table } from '@astryxdesign/core/Table';
import { TextInput } from '@astryxdesign/core/TextInput';
import { TextArea } from '@astryxdesign/core/TextArea';
import { AgentRayAPI, type LabStep, type LabTestResult } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';
import { formatCompact, formatCost } from '@/lib/format';
import { useExplainRun, useLabCases } from '@/modules/agent-lab/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Intro, Panel, Segment, StatusPill } from '@/modules/shared/components/signal-primitives';
import { HarnessGuide } from './harness-guide';
import { StepInspector } from './step-inspector';

const VERDICT: Record<LabTestResult['status'], string> = { pass: 'healthy', fail: 'attention', error: 'attention', blocked: 'paused' };

// StepRail visualizes the loop: one selectable chip per step, in order. Selecting
// a chip drives the inspector below — so the user can scrub back through the loop's
// turns and see exactly what the harness did at each one.
function StepRail({ steps, selected, onSelect }: { steps: LabStep[]; selected: number; onSelect: (i: number) => void }) {
  if (steps.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-1">
      {steps.map((s, i) => (
        <button
          key={i}
          className={`inline-flex h-[30px] items-center gap-1.5 rounded-[20px] border px-2.5 text-[11.5px] cursor-pointer ${
            s.error
              ? 'border-danger text-danger bg-[var(--color-background-muted)]'
              : i === selected
                ? 'border-agent text-[var(--color-text-primary)] bg-[color-mix(in_srgb,var(--agent)_14%,var(--surface-2))]'
                : 'border-[var(--color-border)] text-[var(--color-text-secondary)] bg-[var(--color-background-muted)]'
          }`}
          onClick={() => onSelect(i)}
        >
          {s.kind === 'compaction' ? <Scissors size={12} /> : <span className="font-mono tabular-nums">{s.turn}</span>}
          <span className="whitespace-nowrap">{s.kind === 'compaction' ? 'compact' : `turn ${s.turn}`}</span>
          {i < steps.length - 1 ? <ChevronRight size={12} className="ml-0.5 text-[var(--color-text-disabled)]" /> : null}
        </button>
      ))}
    </div>
  );
}

// DiffView colorizes a unified-style line diff: +added / -removed / context.
function DiffView({ diff }: { diff: string }) {
  return (
    <pre className="m-0 max-h-[220px] overflow-auto whitespace-pre-wrap break-words rounded-sm bg-[var(--color-background-card)] px-2.5 py-2 font-mono text-[11.5px] leading-[1.55] text-[var(--color-text-primary)] flex flex-col">
      {diff.split('\n').map((line, i) => {
        const cls = line.startsWith('+')
          ? 'bg-[color-mix(in_srgb,var(--success)_16%,transparent)] text-success'
          : line.startsWith('-')
            ? 'bg-[color-mix(in_srgb,var(--danger)_14%,transparent)] text-danger'
            : '';
        return <span key={i} className={`block whitespace-pre-wrap break-words rounded-sm px-1 ${cls}`}>{line || ' '}</span>;
      })}
    </pre>
  );
}

export function AgentLabPage() {
  const params = useParams<{ agentId: string }>();
  const router = useRouter();
  const agentID = params.agentId;
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);
  const { cases, save, remove, setVerdict } = useLabCases(agentID);
  const explain = useExplainRun(agentID);

  const [mode, setMode] = useState<'Explain' | 'Test'>('Explain');
  const [input, setInput] = useState('');
  const [expected, setExpected] = useState('');
  const [steer, setSteer] = useState('');
  const [result, setResult] = useState<LabTestResult | null>(null);
  const [testSelected, setTestSelected] = useState(0);
  const [replay, setReplay] = useState<{ name: string; steps: LabStep[] } | null>(null);
  const [replaySelected, setReplaySelected] = useState(0);

  const test = useMutation({
    mutationFn: () => new AgentRayAPI(projectID!).runLabTest(input, expected, agentID),
    onSuccess: (data) => { setResult(data.result); setTestSelected(0); },
    onError: (e: Error) => setError(e.message),
  });

  // Replay reopens the folded steps of a past run (saved-case last run), so the
  // user can re-inspect the harness without spending another run.
  const replayRun = useMutation({
    mutationFn: (vars: { runID: string; name: string }) =>
      new AgentRayAPI(projectID!).labReplaySteps(vars.runID, agentID).then((d) => ({ ...d, name: vars.name })),
    onSuccess: (data) => { setReplay({ name: data.name, steps: data.steps ?? [] }); setReplaySelected(0); },
    onError: (e: Error) => setError(e.message),
  });

  function loadCase(c: (typeof cases)[number]) {
    setInput(c.input);
    setExpected(c.expected);
  }

  function sendSteer() {
    if (!steer.trim()) return;
    const pending = explain.steer(steer);
    if (!pending) { setError('No live run to steer — start an explain run first.'); return; }
    void pending.then((r) => { if (!r?.ok) setError('Steer didn’t apply — the run may have already advanced or finished.'); });
    setSteer('');
  }

  const runDisabled = !input.trim();
  const isExplaining = explain.phase === 'running' || explain.phase === 'paused';
  const explainStep = explain.steps[explain.current];
  // A blocked test verdict (no sandbox) comes back with `steps: null`; normalize
  // so the run-stats and inspector below can safely read `.length` / index.
  const testSteps = result?.steps ?? [];
  const testStep = testSteps[testSelected];

  return (
    <AppShell active="monitor">
      <Intro
        title={<span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}><button className="flex-none grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]" onClick={() => router.push(`/agents/${agentID}/monitor`)}><ArrowLeft size={15} /></button>Agent lab</span>}
        sub="Learn how this agent's harness runs — context, tools, messages, memory, loop — and test it step by step."
        action={
          <>
            <Button variant="outline" icon={<Save size={15} />} onClick={() => input.trim() ? save.mutate({ name: input.slice(0, 48), input, expected }) : undefined}>Save case</Button>
            <Segment options={['Explain', 'Test']} value={mode} onChange={(o) => setMode(o as 'Explain' | 'Test')} />
          </>
        }
      />

      <HarnessGuide />

      <Panel title="Prompt" action={mode === 'Explain'
        ? <Button variant="agent" icon={<Play size={15} />} disabled={runDisabled} onClick={() => explain.start(input)}>{isExplaining ? 'Restart run' : 'Start explain run'}</Button>
        : <Button variant="primary" icon={<Play size={15} />} disabled={runDisabled || test.isPending} onClick={() => test.mutate()}>{test.isPending ? 'Running…' : 'Run test'}</Button>}>
        <div className={mode === 'Test' ? 'grid grid-cols-2 gap-[14px] max-[980px]:grid-cols-1' : undefined}>
          <div>
            <div className="mb-1.5 text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">Input</div>
            <TextArea label="Input" isLabelHidden rows={4} width="100%" value={input} placeholder="Ask the agent something…" onChange={(v) => setInput(v)} />
          </div>
          {mode === 'Test' ? (
            <div>
              <div className="mb-1.5 text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">Expected (what a good answer should contain)</div>
              <TextArea label="Expected answer" isLabelHidden rows={4} width="100%" value={expected} placeholder="A correct answer mentions…" onChange={(v) => setExpected(v)} />
            </div>
          ) : null}
        </div>
      </Panel>

      {/* ── Explain mode: step through the loop one turn at a time ── */}
      {mode === 'Explain' ? (
        explain.phase === 'idle' ? (
          <EmptyState title="Step through the loop" detail="Start an explain run to watch the harness build context, call tools, and decide — pausing before each turn so you can inspect it." />
        ) : (
          <>
            <Panel
              title="The loop"
              action={
                <>
                  <span className="text-[var(--color-text-disabled)]" style={{ marginRight: 8 }}>
                    {explain.phase === 'running' ? 'Computing…' : explain.phase === 'paused' ? `Paused at turn ${explainStep?.turn ?? ''}` : explain.phase === 'done' ? `Finished (${explain.status || 'done'})` : 'Error'}
                  </span>
                  <Button variant="outline" size="sm" icon={<StepForward size={14} />} disabled={explain.phase !== 'paused'} onClick={explain.advance}>Next step</Button>
                  <Button variant="ghost" size="sm" icon={<Square size={14} />} disabled={!isExplaining} onClick={explain.stop}>Stop</Button>
                </>
              }
            >
              <StepRail steps={explain.steps} selected={explain.current} onSelect={explain.setCurrent} />
              {explain.phase === 'paused' ? (
                <div className="mt-2.5 flex items-center gap-2 rounded-md border border-[var(--color-border)] bg-card py-1.5 pl-3 pr-1.5">
                  <TextInput label="Steer the run" isLabelHidden size="sm" className="flex-1" width="100%" value={steer} placeholder="Steer the run — inject a message before the next turn…" onChange={(v) => setSteer(v)} onEnter={sendSteer} />
                  <Button variant="ghost" size="sm" icon={<Send size={14} />} disabled={!steer.trim()} onClick={sendSteer}>Inject</Button>
                </div>
              ) : null}
            </Panel>
            {explainStep ? <Panel title={`Step ${explain.current + 1} of ${explain.steps.length}`}><StepInspector step={explainStep} /></Panel> : null}
            {explain.phase === 'done' && explain.final ? (
              <Panel title="Final answer" action={<StatusPill status={VERDICT[(explain.status as LabTestResult['status']) ?? 'pass'] ?? 'healthy'} label={explain.status || 'done'} grow={false} />}>
                <p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">{explain.final}</p>
              </Panel>
            ) : null}
            {explain.error ? <Panel title="Error"><p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-danger">{explain.error}</p></Panel> : null}
          </>
        )
      ) : null}

      {/* ── Test mode: run vs expected, then inspect every step ── */}
      {mode === 'Test' ? (
        result ? (
          <>
            <Panel title="Verdict" action={<StatusPill status={VERDICT[result.status]} label={result.status} grow={false} />}>
              {result.status === 'blocked' ? (
                <p className="m-0 mb-2 text-xs leading-[1.5] text-[var(--color-text-secondary)]">Test mode needs real tools to produce a meaningful verdict, but no sandbox is configured for this agent — so the run was blocked before executing. Switch to <b>Explain</b> mode to step through the loop (sandbox-dependent tools simply fail closed there).</p>
              ) : null}
              {result.verdict === 'judge' ? (
                <p className="m-0 mb-2 text-xs leading-[1.5] text-[var(--color-text-secondary)]">Graded by rubric judge against your criteria (not exact text match){result.rationale ? <> — <i>{result.rationale}</i></> : null}.</p>
              ) : null}
              <div className="grid grid-cols-2 gap-[14px] max-[980px]:grid-cols-1">
                <div>
                  <div className="mb-1.5 text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">Expected</div>
                  <p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">{result.expected || '—'}</p>
                </div>
                <div>
                  <div className="mb-1.5 text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">Actual answer</div>
                  <p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">{result.actual || '—'}</p>
                </div>
              </div>
              {result.diff ? (
                <>
                  <div className="mt-[14px] mb-1.5 text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">Diff — expected vs actual</div>
                  <DiffView diff={result.diff} />
                </>
              ) : null}
              <div className="mt-3 flex gap-3 text-[11.5px] text-[var(--color-text-secondary)] [&_b]:font-medium [&_b]:text-[var(--color-text-primary)]">
                <span><b className="font-mono tabular-nums">{testSteps.length}</b> steps</span>
                <span><b className="font-mono tabular-nums">{formatCompact(testSteps.reduce((s, st) => s + st.tokens_in + st.tokens_out, 0))}</b> tok</span>
                <span><b className="font-mono tabular-nums">{formatCost(testSteps.reduce((s, st) => s + st.cost_usd, 0))}</b></span>
              </div>
            </Panel>
            {testSteps.length > 0 ? (
              <Panel title="Inspect the run" action={<span className="text-[var(--color-text-disabled)]">{testSteps.length} steps</span>}>
                <StepRail steps={testSteps} selected={testSelected} onSelect={setTestSelected} />
                {testStep ? <StepInspector step={testStep} /> : null}
              </Panel>
            ) : null}
          </>
        ) : (
          <EmptyState title="Test against an expectation" detail="Give the agent a prompt and what a good answer should contain. You'll get a pass/fail verdict and the full step-by-step trace to inspect." />
        )
      ) : null}

      {/* ── Saved regression cases (shared by both modes) ── */}
      <Panel title="Saved cases">
        {cases.length === 0 ? <EmptyState title="No saved cases" detail="Save a prompt + expectation to build a regression set for this agent." /> : (
          /* Astryx migration: saved cases render through the data-driven Astryx
             Table (compact density, themed cells). The case-name cell stays
             clickable (loadCase) via its renderCell wrapper; verdict StatusPill and
             the row action buttons are preserved. */
          <Table
            data={cases}
            idKey="id"
            density="compact"
            columns={[
              { key: 'case', header: 'Case', align: 'start', renderCell: (c) => <span style={{ cursor: 'pointer' }} onClick={() => loadCase(c)}>{c.name || c.input.slice(0, 40)}</span> },
              { key: 'verdict', header: 'Last verdict', align: 'start', renderCell: (c) => c.last_status ? <StatusPill status={VERDICT[c.last_status as LabTestResult['status']] || 'paused'} label={c.last_status} grow={false} /> : <span className="text-[var(--color-text-disabled)]">—</span> },
              {
                key: 'actions',
                header: '',
                align: 'end',
                renderCell: (c) => (
                  <span className="inline-flex gap-1">
                    <Button variant="ghost" size="sm" icon={<History size={14} />} disabled={!c.last_run_id || replayRun.isPending} onClick={() => replayRun.mutate({ runID: c.last_run_id!, name: c.name || c.input.slice(0, 40) })}>Replay</Button>
                    <Button variant="ghost" size="sm" onClick={() => setVerdict.mutate({ id: c.id, status: 'pass' })}>Pass</Button>
                    <Button variant="ghost" size="sm" onClick={() => remove.mutate(c.id)}><span style={{ color: 'var(--danger)' }}>Delete</span></Button>
                  </span>
                ),
              },
            ]}
          />
        )}
      </Panel>

      {/* ── Replay: re-inspect a past run's steps without spending a new run ── */}
      {replay ? (
        <Panel title={`Replay — ${replay.name}`} action={<Button variant="ghost" size="sm" icon={<X size={14} />} onClick={() => setReplay(null)}>Close</Button>}>
          {replay.steps.length === 0 ? (
            <EmptyState title="No steps recorded" detail="This run has no folded steps to replay." />
          ) : (
            <>
              <StepRail steps={replay.steps} selected={replaySelected} onSelect={setReplaySelected} />
              {replay.steps[replaySelected] ? <StepInspector step={replay.steps[replaySelected]} /> : null}
            </>
          )}
        </Panel>
      ) : null}
    </AppShell>
  );
}
