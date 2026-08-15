'use client';

import { ChevronRight, Scissors } from 'lucide-react';
import type { LabStep } from '@/lib/api';

// StepRail visualizes the loop: one selectable chip per step, in order. Selecting
// a chip drives the inspector below — so the user can scrub back through the loop's
// turns and see exactly what the harness did at each one.
//
// It renders the steps it is handed and nothing more. On a lab replay that is the
// whole run; on a long run's chapter view it is one window of it, and the caller
// owns which window — the rail is not the place to decide, because a rail that
// paged itself would disagree with the chapter the reader opened.
export function StepRail({ steps, selected, onSelect }: { steps: LabStep[]; selected: number; onSelect: (i: number) => void }) {
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
