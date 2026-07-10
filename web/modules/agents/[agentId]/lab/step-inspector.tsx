'use client';

import { useState } from 'react';
import { AlertTriangle, Ban, Check, Scissors } from 'lucide-react';
import type { LabStep } from '@/lib/api';
import { formatCompact, formatCost } from '@/lib/format';
import { ConceptCaption } from './harness-guide';

// StepInspector renders one step of a run as the full harness snapshot the model
// saw at that moment: the context it read, the tools/skills/memory available, the
// tool calls it made, and the response it produced — each section captioned with
// the harness concept it embodies. This is the heart of both Lab purposes: it
// teaches the loop and it is how the user inspects a test they ran.

const ROLE_TONE: Record<string, string> = { system: 'paused', user: 'healthy', assistant: 'working', tool: 'attention' };

const PILL_TONE: Record<string, string> = { working: 'text-agent', healthy: 'text-success', attention: 'text-warning', paused: 'text-[var(--color-text-secondary)]' };

function Section({ title, count, children }: { title: string; count?: number; children: React.ReactNode }) {
  return (
    <div className="flex flex-col">
      <div className="text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)] flex items-center gap-2 mb-[7px]">{title}{count != null ? <span className="font-mono tabular-nums inline-flex h-4 min-w-[18px] items-center justify-center rounded-lg bg-[var(--color-background-surface)] px-[5px] text-[10.5px] text-[var(--color-text-secondary)]">{count}</span> : null}</div>
      {children}
    </div>
  );
}

function CompactionStep({ step }: { step: LabStep }) {
  return (
    <div className="flex flex-col gap-3.5">
      <div className="flex items-center gap-2 rounded-md bg-[color-mix(in_srgb,var(--agent)_12%,transparent)] px-[11px] py-2 text-[12.5px] text-[var(--color-text-primary)]"><Scissors size={14} /><b>Context compaction</b><span className="text-[var(--color-text-disabled)]">turn {step.turn}</span></div>
      <ConceptCaption concept="context" />
      <p className="m-0 mb-2 text-xs leading-[1.5] text-[var(--color-text-secondary)]">The older span of the conversation was summarized and replaced to stay under the context window. The recent tail was kept verbatim.</p>
      <Section title="Summary kept in place of the dropped span">
        <p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">{step.summary || '—'}</p>
      </Section>
      <Section title="Retained tail" count={step.context?.length ?? 0}>
        <MessageList messages={step.context} />
      </Section>
    </div>
  );
}

function MessageList({ messages }: { messages: LabStep['context'] }) {
  if (!messages?.length) return <p className="m-0 text-xs text-[var(--color-text-disabled)]">No messages in context.</p>;
  return (
    <div className="flex flex-col gap-2">
      {messages.map((m, i) => (
        <div className="flex items-start gap-[9px]" key={i}>
          <span className={`inline-flex items-center gap-1.5 rounded-[20px] bg-[var(--color-background-surface)] px-[9px] py-[3px] text-[11.5px] flex-none ${PILL_TONE[ROLE_TONE[m.role] || 'paused']}`}>{m.role}</span>
          <pre className="m-0 flex-1 max-h-[200px] overflow-auto whitespace-pre-wrap break-words rounded-sm bg-[var(--color-background-card)] px-[10px] py-[7px] font-mono text-[11.5px] leading-[1.5] text-[var(--color-text-primary)]">{m.content || '—'}</pre>
        </div>
      ))}
    </div>
  );
}

export function StepInspector({ step }: { step: LabStep }) {
  const [showSystem, setShowSystem] = useState(false);
  if (step.kind === 'compaction') return <CompactionStep step={step} />;

  const stepTokens = step.tokens_in + step.tokens_out;

  // The backend sends `null` (not `[]`) for empty collections; normalize so the
  // section render below can safely read `.length` / `.map`.
  const context = step.context ?? [];
  const memory = step.memory ?? [];
  const tools = step.tools ?? [];
  const skillsAdvertised = step.skills_advertised ?? [];
  const skillsLoaded = step.skills_loaded ?? [];
  const toolCalls = step.tool_calls ?? [];

  return (
    <div className="flex flex-col gap-3.5">
      <div className="flex flex-wrap items-center gap-2.5">
        <span className="rounded-lg bg-[color-mix(in_srgb,var(--agent)_18%,transparent)] px-1.5 font-mono text-[10.5px] leading-4 text-agent">turn {step.turn}</span>
        {step.stop_reason ? <span className="text-[var(--color-text-disabled)]">stop: <b className="font-mono tabular-nums">{step.stop_reason}</b></span> : null}
        <span className="ms-auto" />
        <span className="flex gap-3 text-[11.5px] text-[var(--color-text-secondary)]">
          <span><b className="font-mono tabular-nums font-medium text-[var(--color-text-primary)]">{formatCompact(stepTokens)}</b> tok</span>
          <span><b className="font-mono tabular-nums font-medium text-[var(--color-text-primary)]">{formatCost(step.cost_usd)}</b></span>
          <span className="text-[var(--color-text-disabled)]">cum {formatCompact(step.cum_tokens_in + step.cum_tokens_out)} tok · {formatCost(step.cum_cost_usd)}</span>
        </span>
      </div>

      {step.error ? <div className="flex items-center gap-2 rounded-md bg-[color-mix(in_srgb,var(--danger)_14%,transparent)] px-[11px] py-2 text-[12.5px] text-danger"><AlertTriangle size={14} /><span>{step.error}</span></div> : null}

      {/* Context: what the model read this turn */}
      <Section title="Context — messages this turn" count={context.length}>
        <ConceptCaption concept="messages" />
        <MessageList messages={context} />
      </Section>

      {/* System prompt + persona + memory: the assembled prompt */}
      <Section title="System prompt & persona">
        <ConceptCaption concept="context" />
        {step.persona ? <><div className="mt-[9px] mb-1 text-[10.5px] uppercase tracking-[0.04em] text-[var(--color-text-disabled)]">Persona (identity)</div><p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">{step.persona}</p></> : null}
        <button className="mt-1.5 border-0 bg-transparent p-0 text-[11.5px] text-agent cursor-pointer" onClick={() => setShowSystem((v) => !v)}>{showSystem ? 'Hide' : 'Show'} full system prompt</button>
        {showSystem ? <p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">{step.system || '—'}</p> : null}
      </Section>

      <Section title="Memory recalled" count={memory.length}>
        <ConceptCaption concept="memory" />
        {memory.length === 0 ? <p className="m-0 text-xs text-[var(--color-text-disabled)]">No memory recalled for this turn.</p> : (
          <ul className="m-0 flex flex-col gap-1 pl-[18px]">{memory.map((m, i) => <li className="text-xs leading-[1.5] text-[var(--color-text-primary)]" key={i}>{m}</li>)}</ul>
        )}
      </Section>

      {/* Tools & skills available, and skills actually loaded */}
      <Section title="Tools available" count={tools.length}>
        <ConceptCaption concept="tools" />
        <div className="flex flex-wrap gap-1.5">{tools.length === 0 ? <span className="text-xs text-[var(--color-text-disabled)]">No tools advertised.</span> : tools.map((t) => <span className="border border-[var(--color-border)] rounded-md bg-[var(--color-background-muted)] text-[var(--color-text-primary)] hover:border-[color-mix(in_srgb,var(--primary)_45%,var(--border))] hover:bg-[var(--color-background-surface)] font-mono tabular-nums" style={{ padding: '3px 9px', borderRadius: 20, fontSize: 11 }} key={t}>{t}</span>)}</div>
        {skillsAdvertised.length > 0 ? (
          <>
            <div className="mt-[9px] mb-1 text-[10.5px] uppercase tracking-[0.04em] text-[var(--color-text-disabled)]">Skills advertised ({skillsAdvertised.length})</div>
            <div className="flex flex-col gap-1.5">
              {skillsAdvertised.map((s) => (
                <div className="flex items-baseline gap-[9px]" key={s.id}>
                  <span
                    className={`border border-[var(--color-border)] rounded-md bg-[var(--color-background-muted)] text-[var(--color-text-primary)] hover:border-[color-mix(in_srgb,var(--primary)_45%,var(--border))] hover:bg-[var(--color-background-surface)] font-mono tabular-nums shrink-0${skillsLoaded.includes(s.id) || skillsLoaded.includes(s.name) ? ' border-agent text-agent' : ''}`}
                    style={{ padding: '2px 8px', borderRadius: 20, fontSize: 11, background: skillsLoaded.includes(s.id) || skillsLoaded.includes(s.name) ? 'color-mix(in srgb, var(--agent) 14%, var(--surface-2))' : undefined }}
                  >
                    {s.name}
                  </span>
                  <span className="text-[11.5px] leading-[1.45] text-[var(--color-text-secondary)]">{s.description}</span>
                </div>
              ))}
            </div>
            {skillsLoaded.length > 0 ? <div className="mt-[9px] mb-1 text-[10.5px] uppercase tracking-[0.04em] text-[var(--color-text-disabled)]">Loaded so far: <b className="font-mono tabular-nums">{skillsLoaded.join(', ')}</b></div> : null}
          </>
        ) : null}
      </Section>

      {/* The tool calls the model made this turn */}
      <Section title="Tool calls" count={toolCalls.length}>
        <ConceptCaption concept="loop" />
        {toolCalls.length === 0 ? <p className="m-0 text-xs text-[var(--color-text-disabled)]">The model answered directly — no tools called this turn.</p> : (
          <div className="mt-3 rounded-lg border border-dashed border-[var(--color-border)] bg-[color-mix(in_srgb,var(--surface-1)_60%,transparent)] px-3 py-2.5 text-xs">
            {toolCalls.map((call) => (
              <div className="py-2 [&+&]:mt-1 [&+&]:border-t [&+&]:border-dashed [&+&]:border-[var(--color-border)]" key={call.id}>
                <div className="flex items-center gap-[7px] py-[3px]">
                  {call.error ? <Ban size={14} className="flex-none text-danger" /> : call.allowed ? <Check size={14} className="flex-none text-success" /> : <Ban size={14} className="flex-none text-danger" />}
                  <span className="flex-none text-[var(--color-text-primary)] font-mono tabular-nums">{call.name}</span>
                  {!call.allowed ? <span className="inline-flex items-center gap-1.5 rounded-[20px] bg-[var(--color-background-surface)] px-[9px] py-[3px] text-[11.5px] flex-none text-[var(--color-text-secondary)]">blocked</span> : null}
                </div>
                <div className="mt-[9px] mb-1 text-[10.5px] uppercase tracking-[0.04em] text-[var(--color-text-disabled)]">Arguments</div>
                <pre className="m-0 max-h-[220px] overflow-auto whitespace-pre-wrap break-words rounded-sm bg-[var(--color-background-card)] px-2.5 py-2 font-mono text-[11.5px] leading-[1.55] text-[var(--color-text-primary)]">{call.args || '—'}</pre>
                <div className="mt-[9px] mb-1 text-[10.5px] uppercase tracking-[0.04em] text-[var(--color-text-disabled)]">{call.error ? 'Error' : 'Result'}</div>
                <pre className="m-0 max-h-[220px] overflow-auto whitespace-pre-wrap break-words rounded-sm bg-[var(--color-background-card)] px-2.5 py-2 font-mono text-[11.5px] leading-[1.55] text-[var(--color-text-primary)]" style={call.error ? { color: 'var(--danger)' } : undefined}>{call.error || call.result || '—'}</pre>
              </div>
            ))}
          </div>
        )}
      </Section>

      {/* The assistant response produced this turn */}
      <Section title="Assistant response">
        <ConceptCaption concept="loop" />
        <p className="m-0 whitespace-pre-wrap break-words text-[12.5px] leading-[1.55] text-[var(--color-text-primary)]">{step.response || '—'}</p>
      </Section>
    </div>
  );
}
