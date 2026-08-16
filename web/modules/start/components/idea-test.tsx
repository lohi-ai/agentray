'use client';

import Link from 'next/link';
import { Target } from 'lucide-react';
import type { ValidationStatus } from '@/lib/api';
import { Button, EmptyState, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { conversionReadout } from '../validation-readout';

// IdeaTest is the validate job's scoreboard — the one surface in AgentRay that
// reads a product that does not exist yet.
//
// It is built around a single rule: the number is agreed BEFORE the data
// arrives. Everything here is arranged so the owner cannot quietly move it
// afterwards — the commit is its own deliberate act, the target is shown beside
// the count at all times, and the verdict comes from the server so this page and
// the agent's `test_status` can never tell two different stories.
export function IdeaTest({
  status,
  agentID,
  committing,
  onCommit,
  onDecide,
}: {
  status: ValidationStatus | null;
  agentID: string;
  committing: boolean;
  onCommit: (id: string) => void;
  onDecide: (id: string, decision: string, note: string) => void;
}) {
  const test = status?.test ?? null;
  const waitlist = status?.waitlist_count ?? 0;

  if (!test) {
    return (
      <Panel title="The test">
        <EmptyState
          icon={<Target size={18} />}
          title="No test on the board"
          detail="Ask your teammate to design the cheapest test that could prove this idea wrong, then commit to its kill/keep number here."
          action={
            <Link href={chatHref(agentID, 'Design the cheapest test that could prove this idea wrong.')}>
              <Button variant="agent" size="sm">Design the test</Button>
            </Link>
          }
        />
        {waitlist > 0 ? <WaitlistLine count={waitlist} /> : null}
      </Panel>
    );
  }

  const progress = status?.progress;
  const metric = progress?.metric_count ?? 0;
  const baseline = progress?.baseline_count ?? 0;
  const daysLeft = progress?.days_left ?? 0;
  const pct = test.target_count > 0 ? Math.min(100, (metric / test.target_count) * 100) : 0;
  const proposed = test.status === 'proposed';
  // A rate over 100% is a broken measurement, not a triumph — see
  // validation-readout.ts. The count stays readable either way: a signup that
  // arrived is a signup that arrived.
  const conversion = conversionReadout(metric, baseline);
  const rateUnreadable = Boolean(test.baseline_event) && conversion.unreadable;

  return (
    <Panel title="The test">
      <p className="mb-3 text-[13px] leading-[1.55] text-[var(--color-text-primary)]">{test.hypothesis}</p>

      {proposed ? (
        <div className="rounded-[var(--radius-md)] border border-[var(--agent)] p-3">
          <p className="text-[12.5px] leading-[1.55] text-[var(--color-text-secondary)]">
            Your teammate proposes:{' '}
            <b className="text-[var(--color-text-primary)]">
              {test.target_count} people fire <code>{test.metric_event}</code> within {test.window_days} days
            </b>
            . Nothing is counted until you commit — and once you do, you have agreed to the number before you can
            see the result, which is the only way it means anything.
          </p>
          <div className="mt-3">
            <Button variant="agent" size="sm" disabled={committing} onClick={() => onCommit(test.id)}>
              {committing ? 'Committing…' : 'Commit to this number'}
            </Button>
          </div>
        </div>
      ) : (
        <>
          <StatsStrip
            stats={[
              { label: test.metric_event, value: `${metric} / ${test.target_count}` },
              ...(test.baseline_event
                ? [
                    {
                      label: 'Of who saw it',
                      value: conversion.label ?? '—',
                    },
                  ]
                : []),
              { label: 'Days left', value: String(daysLeft) },
              { label: 'On the waitlist', value: String(waitlist) },
            ]}
          />
          <div
            className="mt-3 h-1.5 w-full overflow-hidden rounded-full bg-[var(--color-background-muted)]"
            role="progressbar"
            aria-valuenow={metric}
            aria-valuemin={0}
            aria-valuemax={test.target_count}
            aria-label={`${metric} of ${test.target_count} toward the committed threshold`}
          >
            <div className="h-full rounded-full bg-[var(--agent)]" style={{ width: `${pct}%` }} />
          </div>
          {rateUnreadable ? (
            <Line tone="var(--warning)">
              {metric} joined but only {baseline} <code>{test.baseline_event}</code> arrived, which cannot both be
              true — the snippet is not firing on every visit. Paste it again on every page of the prototype; until
              then read the count, not the rate.
            </Line>
          ) : null}
          <Verdict verdict={status?.verdict ?? ''} test={test} onDecide={onDecide} agentID={agentID} />
        </>
      )}
    </Panel>
  );
}

// Verdict is the whole point of having committed. It never invents a reading:
// `passed` and `failed` come from the server against the agreed number, and
// while the window is open it says so rather than letting a thin first week
// masquerade as an answer.
function Verdict({
  verdict,
  test,
  agentID,
  onDecide,
}: {
  verdict: string;
  test: NonNullable<ValidationStatus['test']>;
  agentID: string;
  onDecide: (id: string, decision: string, note: string) => void;
}) {
  if (verdict === 'passed') {
    return (
      <Line tone="var(--success)">
        <b>Cleared the number you agreed to.</b> That is the signal to build.
        <span className="mt-2 flex gap-2">
          <Button variant="primary" size="sm" onClick={() => onDecide(test.id, 'passed', 'Threshold cleared')}>
            Close it as passed
          </Button>
        </span>
      </Line>
    );
  }
  if (verdict === 'failed') {
    return (
      <Line tone="var(--danger)">
        <b>The window closed short of the number.</b> Before you call the idea dead, ask which it was — no demand, the
        wrong message, or too small a sample. A test few people saw has not falsified anything.
        <span className="mt-2 flex flex-wrap gap-2">
          <Link href={chatHref(agentID, 'My validation test missed its threshold. Was that no demand, the wrong message, or too small a sample?')}>
            <Button variant="agent" size="sm">Ask which it was</Button>
          </Link>
          <Button variant="outline" size="sm" onClick={() => onDecide(test.id, 'failed', 'Missed the committed threshold')}>
            Close it as failed
          </Button>
          <Button variant="ghost" size="sm" onClick={() => onDecide(test.id, 'abandoned', 'Stopped before the window closed')}>
            Abandon
          </Button>
        </span>
      </Line>
    );
  }
  return (
    <Line tone="var(--color-border-strong)">
      Still running — too early to call, and the product will not let you call it. Drive traffic to the page and let the
      number decide.
    </Line>
  );
}

function Line({ tone, children }: { tone: string; children: React.ReactNode }) {
  return (
    <p
      className="mt-3 border-s-2 ps-3 text-[12.5px] leading-[1.55] text-[var(--color-text-secondary)]"
      style={{ borderInlineStartColor: tone }}
    >
      {children}
    </p>
  );
}

function WaitlistLine({ count }: { count: number }) {
  return (
    <p className="mt-3 text-[12.5px] text-[var(--color-text-secondary)]">
      {count} {count === 1 ? 'person has' : 'people have'} joined the waitlist already — that is a costly signal, and it
      is the number a test should be judged on.
    </p>
  );
}

function chatHref(agentID: string, prompt: string) {
  return `/chat?${new URLSearchParams(agentID ? { agent: agentID, q: prompt } : { q: prompt }).toString()}`;
}
