'use client';

import { Compass, HeartPulse, TrendingUp } from 'lucide-react';
import type { ComponentType } from 'react';
import { JOBS, jobProgress, type JobDef, type JobId, type JobState } from '@/lib/jobs';

const JOB_ICON: Record<JobId, ComponentType<{ size?: number }>> = {
  validate: Compass,
  grow: TrendingUp,
  operate: HeartPulse,
};

// The picker asks the one question a new owner can actually answer — where their
// product is — instead of asking which of our capabilities they need. The card
// is deliberately written in their words: the layer names live one level down,
// on the plan, where they explain something.
export function JobPicker({
  active,
  state,
  onPick,
}: {
  active: JobId;
  state: JobState;
  onPick: (id: JobId) => void;
}) {
  return (
    <div className="mb-5 grid gap-3 md:grid-cols-3">
      {JOBS.map((job) => (
        <JobCard key={job.id} job={job} active={job.id === active} state={state} onPick={onPick} />
      ))}
    </div>
  );
}

function JobCard({
  job,
  active,
  state,
  onPick,
}: {
  job: JobDef;
  active: boolean;
  state: JobState;
  onPick: (id: JobId) => void;
}) {
  const Icon = JOB_ICON[job.id];
  // Progress is only meaningful for the job on screen — the state carries that
  // job's installed packs and its triggers, so the other two cards would render
  // a confidently wrong count.
  const progress = active ? jobProgress(job, state) : null;
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={() => onPick(job.id)}
      className={`rounded-[var(--radius-lg)] border p-4 text-left transition-[border-color,background-color] ${
        active
          ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
          : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
      }`}
    >
      <span className="mb-2 flex items-center gap-2 text-[var(--agent)]">
        <Icon size={16} />
        <span className="text-[12.5px] font-[600] uppercase tracking-[0.06em]">{job.label}</span>
      </span>
      <span className="block text-[14px] font-[600] text-[var(--color-text-primary)]">{job.situation}</span>
      <span className="mt-1 block text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">{job.outcome}</span>
      {progress ? (
        <span className="mt-3 block text-[12px] text-[var(--color-text-secondary)]">
          {progress.done === progress.total
            ? 'Set up and running'
            : `${progress.done} of ${progress.total} set up`}
        </span>
      ) : null}
    </button>
  );
}
