'use client';

import { useState } from 'react';
import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, Beaker, Target, TriangleAlert } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { apiBase } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { formatRelative } from '@/lib/format';
import { conversionReadout } from '@/modules/start/validation-readout';
import { InstrumentSnippet } from '@/modules/start/components/instrument-snippet';
import { AppShell } from '@/modules/shared/components/app-shell';
import {
  Button,
  Callout,
  Intro,
  Loading,
  Panel,
  StatsStrip,
  StatusPill,
} from '@/modules/shared/components/signal-primitives';
import { usePrototype } from './hooks';
import { chatHref, isRecorded, progressPct, stateOf, STATE_LABEL, STATE_PILL, STATE_TONE } from './lib/prototype';

// /prototypes/[id] — one bet, in full: the terms, the count against them, and
// the decision. The verdict shown here is the server's, the same one the agent
// quotes from `test_status`; this page never re-scores a count itself.

const inputCls =
  'w-full rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-2.5 py-1.5 text-[13px] text-[var(--color-text)] outline-none focus:border-[var(--color-border-strong)]';

export function PrototypeDetailPage() {
  const params = useParams<{ prototypeId: string }>();
  const id = params?.prototypeId ?? '';
  const router = useRouter();
  const { test, waitlistCount, isLoading, error, commit, committing, decide, deciding } = usePrototype(id);
  const apiKey = useAuthStore((s) => s.project?.api_key ?? '');
  const [note, setNote] = useState('');

  if (isLoading && !test) {
    return (
      <AppShell active="prototypes">
        <Loading label="Opening the prototype…" />
      </AppShell>
    );
  }

  if (error || !test) {
    return (
      <AppShell active="prototypes">
        <Intro title="Prototype" sub="One falsifiable bet on one idea." />
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Not found"
          title="We couldn’t open that prototype"
          detail="It may have been deleted, or it belongs to another project. Nothing has been changed."
          action={
            <Button variant="outline" size="sm" onClick={() => router.push('/prototypes')}>
              Back to Prototypes
            </Button>
          }
        />
      </AppShell>
    );
  }

  const state = stateOf(test);
  const pct = progressPct(test);
  const tone = STATE_TONE[state];
  const conversion = conversionReadout(test.metric_count, test.baseline_count);
  const rateUnreadable = Boolean(test.baseline_event) && test.measured && conversion.unreadable;
  const recorded = isRecorded(test);

  return (
    <AppShell active="prototypes">
      <Intro
        title={
          <HStack gap={2} align="center">
            <span>Prototype</span>
            <StatusPill status={STATE_PILL[state]} label={STATE_LABEL[state]} grow={false} />
          </HStack>
        }
        sub={
          test.committed_at
            ? `Committed ${formatRelative(test.committed_at)} · ${test.window_days}-day window`
            : `Proposed ${formatRelative(test.created_at)} · nothing is counted yet`
        }
        action={
          <Button variant="ghost" size="sm" icon={<ArrowLeft size={13} aria-hidden />} onClick={() => router.push('/prototypes')}>
            Prototypes
          </Button>
        }
      />

      <Panel title="The bet">
        <VStack gap={3} align="stretch">
          <Text weight="medium">{test.hypothesis}</Text>
          <HStack gap={2} align="center" className="flex-wrap">
            <Badge variant="neutral" label={<code>{test.metric_event}</code>} />
            {test.baseline_event ? <Badge variant="neutral" label={<code>of {test.baseline_event}</code>} /> : null}
            <Text type="supporting">
              {test.target_count} within {test.window_days} days
            </Text>
          </HStack>
          {state === 'proposed' ? (
            <div className="rounded-[var(--radius-md)] border border-[var(--agent)] p-3">
              <Text type="supporting">
                Nothing is counted until you commit — and once you do, you have agreed to the number before you can see
                the result, which is the only way it means anything.
              </Text>
              <div className="mt-3">
                <Button variant="agent" size="sm" disabled={committing} onClick={commit}>
                  {committing ? 'Committing…' : 'Commit to this number'}
                </Button>
              </div>
            </div>
          ) : null}
        </VStack>
      </Panel>

      {test.measured ? (
        <>
          <StatsStrip
            stats={[
              { label: test.metric_event, value: `${test.metric_count} / ${test.target_count}` },
              ...(test.baseline_event ? [{ label: 'Of who saw it', value: conversion.label ?? '—' }] : []),
              { label: 'Days elapsed', value: String(test.days_elapsed) },
              { label: 'Days left', value: String(test.days_left) },
              { label: 'On the waitlist', value: String(waitlistCount) },
            ]}
          />
          {pct !== null ? (
            <div
              className="mb-4 h-1.5 w-full overflow-hidden rounded-full bg-[var(--color-background-muted)]"
              role="progressbar"
              aria-valuenow={test.metric_count}
              aria-valuemin={0}
              aria-valuemax={test.target_count}
              aria-label={`${test.metric_count} of ${test.target_count} toward the committed threshold`}
            >
              <div className="h-full rounded-full" style={{ width: `${pct}%`, background: tone }} />
            </div>
          ) : null}
          {rateUnreadable ? (
            <Callout
              tone="warn"
              icon={<TriangleAlert size={18} aria-hidden />}
              label="Unreadable rate"
              title="More people converted than were seen, which cannot both be true"
              detail={`${test.metric_count} fired ${test.metric_event} but only ${test.baseline_count} ${test.baseline_event} arrived — the snippet is not firing on every visit. Paste it on every page of the prototype; until then read the count, not the rate.`}
            />
          ) : null}
        </>
      ) : null}

      <Panel title="Put it on the page">
        <InstrumentSnippet apiKey={apiKey} host={apiBase()} />
      </Panel>

      <Panel title="The decision">
        <VStack gap={3} align="stretch">
          {recorded ? (
            <VStack gap={1} align="stretch">
              <Text weight="medium">
                Closed as {STATE_LABEL[state].toLowerCase()}
                {test.decided_at ? ` ${formatRelative(test.decided_at)}` : ''}
              </Text>
              <Text type="supporting">
                {test.decision_note || 'No note was written. A month from now the why is the part worth having.'}
              </Text>
            </VStack>
          ) : state === 'proposed' ? (
            <Text type="supporting">
              There is nothing to decide yet. Commit to the number above and the window opens.
            </Text>
          ) : state === 'running' ? (
            <Text type="supporting">
              Still running — too early to call, and the product will not let you call it. Drive traffic to the page and
              let the number decide.
            </Text>
          ) : (
            // The window has settled it, but the owner has not written down what
            // it meant. That note is the only part of this row that a person can
            // read a month from now and act on.
            <VStack gap={3} align="stretch">
              <Text type="supporting">
                {state === 'passed'
                  ? 'It cleared the number you agreed to. Close it out, and say what you are building because of it.'
                  : 'The window closed short of the number. Before you call the idea dead, ask which it was — no demand, the wrong message, or too small a sample. A test few people saw has not falsified anything.'}
              </Text>
              <textarea
                className={`${inputCls} min-h-[72px]`}
                value={note}
                placeholder="What did this actually tell you?"
                onChange={(e) => setNote(e.target.value)}
                aria-label="Decision note"
              />
              <HStack gap={2} align="center" className="flex-wrap">
                {state === 'failed' ? (
                  <Button
                    variant="agent"
                    size="sm"
                    icon={<Target size={13} aria-hidden />}
                    onClick={() =>
                      router.push(
                        chatHref(
                          `My prototype "${test.hypothesis}" missed its threshold (${test.metric_count} of ${test.target_count}). Was that no demand, the wrong message, or too small a sample?`,
                        ),
                      )
                    }
                  >
                    Ask which it was
                  </Button>
                ) : (
                  <Button
                    variant="agent"
                    size="sm"
                    icon={<Beaker size={13} aria-hidden />}
                    onClick={() => router.push(chatHref(`My prototype "${test.hypothesis}" passed. What is the smallest thing I should build first?`))}
                  >
                    Plan what to build
                  </Button>
                )}
                <Button
                  variant={state === 'passed' ? 'primary' : 'outline'}
                  size="sm"
                  disabled={deciding}
                  onClick={() => decide(state, note.trim() || (state === 'passed' ? 'Threshold cleared' : 'Missed the committed threshold'))}
                >
                  {deciding ? 'Recording…' : `Close it as ${state}`}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={deciding}
                  onClick={() => decide('abandoned', note.trim() || 'Stopped before the window closed')}
                >
                  Abandon
                </Button>
              </HStack>
            </VStack>
          )}
        </VStack>
      </Panel>
    </AppShell>
  );
}
