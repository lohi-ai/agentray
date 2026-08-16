'use client';

import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { ArrowRight, Check, MessageSquare } from 'lucide-react';
import type { AgentPreset, ValidationStatus } from '@/lib/api';
import { jobLayers, jobPackName, jobSteps, primaryPack, type JobDef, type JobState, type JobStep } from '@/lib/jobs';
import { Button, Panel } from '@/modules/shared/components/signal-primitives';
import { IdeaTest } from './idea-test';
import { InstrumentSnippet } from './instrument-snippet';

// JobPlan is the architecture made useful: the four layers stated as what they
// do for this job, the ordered steps that arm them, and the questions the job
// exists to answer. Everything on it is one click from working.
export function JobPlan({
  job,
  state,
  agentID,
  presets,
  installing,
  onHire,
  validation,
  committing,
  onCommit,
  onDecide,
  apiKey,
  apiHost,
}: {
  job: JobDef;
  state: JobState;
  // The hired agent, if any — chat and prompts address it directly so the
  // question lands on the teammate that owns the job.
  agentID: string;
  presets: readonly AgentPreset[];
  installing: boolean;
  onHire: (slug: string) => void;
  // Validate-only. Null for grow/operate, which have no pre-product test.
  validation: ValidationStatus | null;
  committing: boolean;
  onCommit: (id: string) => void;
  onDecide: (id: string, decision: string, note: string) => void;
  apiKey: string;
  apiHost: string;
}) {
  const steps = jobSteps(job, state);
  const next = steps.find((s) => !s.done) ?? null;
  const preProduct = !job.needsEvents;
  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,340px)]">
      <div className="flex flex-col gap-4">
        <Panel title="Your next step">
          <ol className="flex flex-col gap-2">
            {steps.map((step) => (
              <StepRow
                key={step.id}
                step={step}
                isNext={step.id === next?.id}
                installing={installing}
                onHire={onHire}
              />
            ))}
          </ol>
        </Panel>
        {/* The scoreboard sits directly under the steps, because the step it
            belongs to ("agree the number") is meaningless without seeing what
            the number is being measured against. */}
        {preProduct ? (
          <IdeaTest
            status={validation}
            agentID={agentID}
            committing={committing}
            onCommit={onCommit}
            onDecide={onDecide}
          />
        ) : null}
        {/* Shown on the job whose next step is instrumentation, and kept on
            afterwards: an owner adding a second landing page needs it again. */}
        {preProduct ? (
          <Panel title="Put it on your page">
            <InstrumentSnippet apiKey={apiKey} host={apiHost} />
          </Panel>
        ) : null}
        <Panel title="Ask it now">
          <PromptList job={job} agentID={agentID} />
        </Panel>
      </div>
      <div className="flex flex-col gap-4">
        <Panel title="How this works">
          <HowItWorks job={job} presets={presets} />
        </Panel>
        <Panel title="Where the answers live">
          <div className="flex flex-wrap gap-2">
            {job.surfaces.map((surface) => (
              <Link
                key={surface.href}
                href={surface.href}
                className="rounded-[var(--radius-md)] border border-[var(--color-border)] px-2.5 py-1.5 text-[12.5px] text-[var(--color-text-secondary)] hover:border-[var(--color-border-strong)] hover:text-[var(--color-text-primary)]"
              >
                {surface.label}
              </Link>
            ))}
          </div>
        </Panel>
      </div>
    </div>
  );
}

function StepRow({
  step,
  isNext,
  installing,
  onHire,
}: {
  step: JobStep;
  isNext: boolean;
  installing: boolean;
  onHire: (slug: string) => void;
}) {
  const router = useRouter();
  const act = () => {
    if (step.action.install) onHire(step.action.install);
    else if (step.action.href) router.push(step.action.href);
  };
  return (
    <li
      className={`flex items-start gap-3 rounded-[var(--radius-md)] border p-3 ${
        isNext ? 'border-[var(--agent)]' : 'border-transparent'
      }`}
    >
      <span
        aria-hidden
        className={`mt-0.5 flex size-[18px] flex-none items-center justify-center rounded-full text-[var(--color-text-inverse)] ${
          step.done ? 'bg-[var(--success)]' : 'border border-[var(--color-border-strong)]'
        }`}
      >
        {step.done ? <Check size={11} /> : null}
      </span>
      <span className="min-w-0 flex-1">
        <span className="block text-[13px] font-[600] text-[var(--color-text-primary)]">{step.label}</span>
        <span className="mt-0.5 block text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">{step.detail}</span>
      </span>
      {step.done ? null : (
        // Only the hire step is blocked by an in-flight install — disabling the
        // whole list would freeze "Add AI key" while an unrelated hire runs.
        <Button
          variant={isNext ? 'agent' : 'outline'}
          size="sm"
          onClick={act}
          disabled={installing && !!step.action.install}
        >
          {installing && step.action.install ? 'Hiring…' : step.action.label}
        </Button>
      )}
    </li>
  );
}

function PromptList({ job, agentID }: { job: JobDef; agentID: string }) {
  const href = (prompt: string) =>
    `/chat?${new URLSearchParams(agentID ? { agent: agentID, q: prompt } : { q: prompt }).toString()}`;
  return (
    <div className="flex flex-col gap-1.5">
      {job.prompts.map((prompt) => (
        <Link
          key={prompt}
          href={href(prompt)}
          className="group flex items-center justify-between gap-3 rounded-[var(--radius-md)] border border-[var(--color-border)] px-3 py-2 text-[13px] text-[var(--color-text-primary)] hover:border-[var(--agent)]"
        >
          <span className="flex min-w-0 items-center gap-2">
            <MessageSquare size={14} className="flex-none text-[var(--color-text-secondary)]" />
            <span className="truncate">{prompt}</span>
          </span>
          <ArrowRight size={14} className="flex-none text-[var(--color-text-secondary)] group-hover:text-[var(--agent)]" />
        </Link>
      ))}
    </div>
  );
}

// The four layers, in order, each stated as what it does for this job. The
// catalog is the source of truth for the teammate's own words — the static name
// only fills the gap before it loads.
function HowItWorks({ job, presets }: { job: JobDef; presets: readonly AgentPreset[] }) {
  const slug = primaryPack(job);
  const preset = presets.find((p) => p.slug === slug);
  const layers = jobLayers(job);
  return (
    <div className="flex flex-col gap-3">
      <p className="text-[12.5px] leading-[1.55] text-[var(--color-text-secondary)]">
        <span className="font-[600] text-[var(--color-text-primary)]">{preset?.name ?? jobPackName(slug)}</span>
        {preset?.tagline ? ` — ${preset.tagline}` : ''}
      </p>
      <ol className="flex flex-col gap-2.5">
        {layers.map((layer, index) => (
          <li key={layer.id} className="flex gap-2.5">
            <span
              aria-hidden
              className="mt-[3px] size-[15px] flex-none rounded-full border border-[var(--color-border-strong)] text-center text-[9px] leading-[13px] text-[var(--color-text-secondary)]"
            >
              {index + 1}
            </span>
            <span className="min-w-0">
              <span className="block text-[12.5px] font-[600] text-[var(--color-text-primary)]">{layer.label}</span>
              <span className="block text-[12px] leading-[1.5] text-[var(--color-text-secondary)]">{layer.detail}</span>
            </span>
          </li>
        ))}
      </ol>
    </div>
  );
}
