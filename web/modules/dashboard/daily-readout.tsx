'use client';

import Link from 'next/link';
import { ArrowRight, Check, MessageSquare, Sparkles, TrendingUp, X } from 'lucide-react';
import type { AgentRecommendation, AgentRun } from '@/lib/api';
import { formatCost, formatRelative } from '@/lib/format';
import { useDailyReadout } from '@/modules/app/hooks';
import { Button, Loading } from '@/modules/shared/components/signal-primitives';
import { AgentMarkdown } from '@/modules/shared/components/agent-markdown';
import { useStackSheet } from '@/modules/shared/components/stack-sheet';

// A growth/retention category reads as "growth" tone; everything else (data
// quality, ops) reads as the neutral "agentic" tone.
function recTone(category: string): 'growth' | 'agentic' {
  return /grow|retention|acqui|activation|revenue|monet/i.test(category) ? 'growth' : 'agentic';
}

// leadText distills a full markdown readout to a one-glance teaser for the card:
// the agent leads with a "key takeaway" line, so we take the first substantive
// line, strip markdown markers (#, *, `, table pipes), and clamp it. The full
// narration — headings, tables, charts — lives in the sheet.
function leadText(summary: string): string {
  const line = summary
    .split('\n')
    .map((l) => l.trim())
    .find((l) => l && !/^[-|>#]+$/.test(l) && !/^\|?[\s:|-]+\|?$/.test(l));
  if (!line) return '';
  const clean = line
    .replace(/^#{1,6}\s*/, '')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/[`*|]/g, '')
    .trim();
  return clean.length > 220 ? `${clean.slice(0, 217).trimEnd()}…` : clean;
}

function RecCard({ rec, onAck, acking }: { rec: AgentRecommendation; onAck: (id: string, status: 'accepted' | 'dismissed') => void; acking: boolean }) {
  return (
    <div className={`mb-4 flex items-start gap-[13px] rounded-xl bg-[var(--color-background-card)] px-4 py-3.5 ${recTone(rec.category) === 'growth' ? '' : ''}`}>
      <span className={`grid h-[34px] w-[34px] flex-none place-items-center rounded-[10px] ${recTone(rec.category) === 'growth' ? 'bg-[color-mix(in_srgb,var(--primary)_16%,transparent)] text-primary' : 'bg-[color-mix(in_srgb,var(--agent)_16%,transparent)] text-agent'}`}><TrendingUp size={15} /></span>
      <div style={{ minWidth: 0 }}>
        <div className="mb-0.5 text-[11px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">{rec.category || 'recommendation'} · impact {Math.round(rec.impact_score)}</div>
        <div className="mb-0.5 text-sm font-semibold">{rec.title}</div>
        <div className="text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">{rec.rationale}</div>
      </div>
      <div className="ms-auto self-center flex gap-1.5">
        <button className="flex-none grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]" title="Accept" disabled={acking} onClick={() => onAck(rec.id, 'accepted')}><Check size={15} /></button>
        <button className="flex-none grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]" title="Dismiss" disabled={acking} onClick={() => onAck(rec.id, 'dismissed')}><X size={15} /></button>
      </div>
    </div>
  );
}

function RunNarration({ run }: { run: AgentRun }) {
  const { push } = useStackSheet();
  const lead = leadText(run.summary);
  const when = formatRelative(run.finished_at || run.started_at);

  const openFull = () => {
    push({
      id: `readout-${run.id}`,
      title: 'Daily readout',
      content: (
        <div style={{ padding: '16px 18px' }}>
          <div className="mb-[10px] text-[11px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">
            {when} · {run.token_input + run.token_output} tokens · {formatCost(run.cost_usd)}
          </div>
          <AgentMarkdown text={run.summary} />
        </div>
      ),
    });
  };

  return (
    <div className="mb-4 flex items-start gap-[13px] rounded-xl bg-[var(--color-background-card)] px-4 py-3.5">
      <span className="grid h-[34px] w-[34px] flex-none place-items-center rounded-[10px] bg-[color-mix(in_srgb,var(--agent)_16%,transparent)] text-agent"><Sparkles size={15} /></span>
      <div style={{ minWidth: 0, flex: 1 }}>
        <div className="mb-0.5 text-[11px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">Latest readout · {when}</div>
        <div className="mt-0.5 text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">{lead}</div>
        <button
          className="mt-2 inline-flex items-center gap-1 border-0 bg-transparent p-0 cursor-pointer text-agent text-[12.5px] font-semibold hover:underline"
          onClick={openFull}
        >
          Read full readout <ArrowRight size={13} />
        </button>
      </div>
    </div>
  );
}

// DailyReadout is the agent-narrated slot atop the dashboard home: what the
// agent saw on its last run, the open recommendations it wrote, and a one-click
// way to ask it a follow-up. This is what makes AgentRay's home "agent-led"
// rather than a static dashboard.
export function DailyReadout() {
  const { latestRun, recommendations, loading, ackRec, acking } = useDailyReadout();

  const ask = (
    <Link href="/chat">
      <Button variant="agent" size="sm" icon={<MessageSquare size={15} />}>Ask the agent</Button>
    </Link>
  );

  if (loading) {
    return (
      <div className="mb-4 rounded-xl bg-[var(--color-background-card)] p-4">
        <Loading label="Loading the agent's readout…" />
      </div>
    );
  }

  if (!latestRun && recommendations.length === 0) {
    return (
      <div className="mb-4 rounded-xl bg-[var(--color-background-card)] p-4">
        <div className="mb-3 flex items-center">
          <h3 className="m-0 text-[13px] font-semibold">Daily readout</h3>
          <div className="ms-auto">{ask}</div>
        </div>
        <p style={{ color: 'var(--color-text-secondary)', fontSize: 12.5, margin: 0 }}>
          Your agent hasn&apos;t produced a readout yet. Ask it about your product and it&apos;ll start
          narrating what it finds and recommending what to do next.
        </p>
      </div>
    );
  }

  return (
    <div className="mb-4 rounded-xl bg-[var(--color-background-card)] p-4">
      <div className="mb-3 flex items-center">
        <h3 className="m-0 text-[13px] font-semibold">Daily readout</h3>
        <div className="ms-auto">{ask}</div>
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        {latestRun?.summary ? <RunNarration run={latestRun} /> : null}
        {recommendations.slice(0, 3).map((rec) => (
          <RecCard key={rec.id} rec={rec} onAck={ackRec} acking={acking} />
        ))}
      </div>
    </div>
  );
}
