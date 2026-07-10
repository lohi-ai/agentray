'use client';

import { useState } from 'react';
import { Brain, ChevronDown, Layers, ListTree, RefreshCw, Wrench } from 'lucide-react';

// The Lab's teaching layer. HARNESS_CONCEPTS is the canonical short explanation
// of each part of the agent loop the inspector visualizes; the same copy is
// reused as per-section captions inside the step inspector so the diagram and the
// live data stay in lock-step. This is purpose #1 of the Lab: explain how the
// harness actually runs before (and while) the user tests it.
export const HARNESS_CONCEPTS = {
  loop: {
    icon: RefreshCw,
    label: 'The loop',
    short: 'Each turn the model reads the context, decides to call tools or answer, tools run, results feed back in — repeat until it stops.',
  },
  context: {
    icon: Layers,
    label: 'Context',
    short: 'Everything the model sees this turn: system prompt + persona + recalled memory + the running message history. The model is stateless — context is rebuilt and re-sent every turn.',
  },
  messages: {
    icon: ListTree,
    label: 'Messages',
    short: 'The conversation so far — user inputs, assistant replies, and tool results, in order. New tool results are appended as messages for the next turn.',
  },
  tools: {
    icon: Wrench,
    label: 'Tools & skills',
    short: 'Capabilities the model can invoke. Each call is gated (allowed or blocked), runs, and returns a result. Skills are progressively loaded only when needed.',
  },
  memory: {
    icon: Brain,
    label: 'Memory',
    short: 'Durable facts recalled from past runs and injected into context, so the agent carries knowledge forward instead of starting cold each run.',
  },
} as const;

export type HarnessConcept = keyof typeof HARNESS_CONCEPTS;

// ConceptCaption is the one-line teaching note shown above each inspector
// section, tying the live data below it back to the harness concept it embodies.
export function ConceptCaption({ concept }: { concept: HarnessConcept }) {
  const c = HARNESS_CONCEPTS[concept];
  const Icon = c.icon;
  return (
    <div className="flex items-start gap-[7px] mb-[9px] rounded-sm bg-[color-mix(in_srgb,var(--agent)_8%,transparent)] px-[9px] py-[7px] text-[11.5px] leading-[1.5] text-[var(--color-text-secondary)]">
      <Icon size={13} className="mt-px shrink-0 text-agent" />
      <span>{c.short}</span>
    </div>
  );
}

// HarnessGuide is the collapsible primer at the top of the Lab: the five concepts
// that make up the loop, each a card the user can read before running anything.
export function HarnessGuide() {
  const [open, setOpen] = useState(true);
  return (
    <div className="rounded-xl bg-[var(--color-background-card)] p-4">
      <button className="flex w-full items-center gap-2 border-0 bg-transparent p-0 text-[var(--color-text-secondary)] cursor-pointer" onClick={() => setOpen((v) => !v)}>
        <h3 className="m-0 text-[13.5px] text-[var(--color-text-primary)]">How an agent harness runs</h3>
        <span className="ms-auto" />
        <ChevronDown size={16} style={{ transform: open ? 'rotate(180deg)' : undefined, transition: 'transform .15s' }} />
      </button>
      {open ? (
        <div className="mt-3 grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-2.5">
          {(Object.keys(HARNESS_CONCEPTS) as HarnessConcept[]).map((key, i) => {
            const c = HARNESS_CONCEPTS[key];
            const Icon = c.icon;
            return (
              <div className="rounded-lg border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--surface-1)_60%,transparent)] px-3 py-[11px]" key={key}>
                <div className="flex items-center gap-[7px] text-[12.5px] text-[var(--color-text-primary)]">
                  <span className="font-mono tabular-nums inline-flex h-[17px] w-[17px] items-center justify-center rounded-md bg-[color-mix(in_srgb,var(--agent)_18%,transparent)] text-[10px] text-agent">{i + 1}</span>
                  <Icon size={14} className="text-agent" />
                  <b>{c.label}</b>
                </div>
                <p className="mt-[7px] mb-0 text-[11.5px] leading-[1.5] text-[var(--color-text-secondary)]">{c.short}</p>
              </div>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}
