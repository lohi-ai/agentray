'use client';

import { useState } from 'react';
import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, Beaker, Target, TriangleAlert } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import { VStack } from '@astryxdesign/core/VStack';
import type { TestOutcomeEntry } from '@/lib/api';
import { formatDate, formatRelative } from '@/lib/format';
import { useProjectAccess } from '@/modules/app/hooks';
import { conversionReadout } from '@/modules/start/validation-readout';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Modal, PromptDialog } from '@/modules/shared/components/modal';
import { Button, Callout, Loading, Panel, StatsStrip, StatusPill } from '@/modules/shared/components/signal-primitives';
import { chatHref, isRecorded, progressPct, stateOf, STATE_LABEL, STATE_PILL, STATE_TONE } from '@/modules/prototypes/lib/prototype';
import { useExperiment } from '../hooks';
import { baselineLine, evidenceLine, outcomeEntries } from '../lib/plans';

// /plans/[testId] — one experiment in full: the terms, every resumable field
// the row carries, the append-only outcome list, and the owner acts. The
// verdict shown here is the server's — this page never re-scores a count.

const inputCls =
  'w-full rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-2 text-sm text-[var(--color-text)] outline-none focus:border-[var(--color-border-strong)]';

export function PlanDetailPage() {
  const params = useParams<{ testId: string }>();
  const id = params?.testId ?? '';
  const router = useRouter();
  const access = useProjectAccess();
  const { test, waitlistCount, isLoading, error, commit, committing, decide, deciding, abandon, abandoning, recordOutcome, recording } =
    useExperiment(id);
  const [note, setNote] = useState('');
  const [abandoningOpen, setAbandoningOpen] = useState(false);
  const [outcomeOpen, setOutcomeOpen] = useState(false);

  if (isLoading && !test) {
    return (
      <AppShell active="plans">
        <Loading label="Opening the experiment…" />
      </AppShell>
    );
  }

  if (error || !test) {
    return (
      <AppShell active="plans" title="Experiment" sub="One falsifiable bet on one idea.">
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Not found"
          title="We couldn’t open that experiment"
          detail="It may have been deleted, or it belongs to another project. Nothing has been changed."
          action={
            <Button variant="outline" size="sm" onClick={() => router.push('/plans')}>
              Back to Plans
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
  const outcomes = outcomeEntries(test.outcome_json);

  return (
    <AppShell
      active="plans"
      title={
        <HStack gap={2} align="center">
          <span>Experiment</span>
          <StatusPill status={STATE_PILL[state]} label={STATE_LABEL[state]} grow={false} />
        </HStack>
      }
      sub={
        test.committed_at
          ? `Committed ${formatRelative(test.committed_at)} · ${test.window_days}-day window`
          : `Proposed ${formatRelative(test.created_at)} · nothing is counted yet`
      }
      actions={
        <Button variant="ghost" size="sm" icon={<ArrowLeft size={13} aria-hidden />} onClick={() => router.push('/plans')}>
          Plans
        </Button>
      }
    >
      {abandoningOpen ? (
        <PromptDialog
          title="Abandon this proposal?"
          label="Why is it being closed?"
          placeholder="e.g. the idea changed before we committed"
          submitLabel={abandoning ? 'Abandoning…' : 'Abandon proposal'}
          // The write can fail (a revision conflict reloads the row), so the
          // dialog closes only once it has actually landed — the reason the
          // owner typed is not thrown away by a failed submission.
          closeOnSubmit={false}
          onSubmit={async (reason) => {
            await abandon(reason.trim() || 'Closed before commitment');
            setAbandoningOpen(false);
          }}
          onClose={() => setAbandoningOpen(false)}
        />
      ) : null}
      {outcomeOpen ? (
        <OutcomeDialog
          busy={recording}
          onSubmit={async (v) => {
            await recordOutcome(v);
            setOutcomeOpen(false);
          }}
          onClose={() => setOutcomeOpen(false)}
        />
      ) : null}

      <Panel title="The bet">
        <VStack gap={3} align="stretch">
          <Text weight="medium">{test.hypothesis}</Text>
          <HStack gap={2} align="center" className="flex-wrap">
            <Badge variant="neutral" label={<code>{test.metric_event}</code>} />
            {test.baseline_event ? <Badge variant="neutral" label={<code>of {test.baseline_event}</code>} /> : null}
            <Text type="supporting">{baselineLine(test)}</Text>
          </HStack>
          {state === 'proposed' ? (
            <div className="rounded-[var(--radius-md)] border border-[var(--agent)] p-3">
              <Text type="supporting">
                Nothing is counted until you commit — and once you do, you have agreed to the number before you can see
                the result, which is the only way it means anything.
              </Text>
              <HStack gap={2} className="mt-3 flex-wrap">
                <Button
                  variant="agent"
                  size="sm"
                  disabled={committing || !access.canWrite}
                  tooltip={access.reason || undefined}
                  onClick={commit}
                >
                  {committing ? 'Committing…' : 'Commit to this number'}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={abandoning || !access.canWrite}
                  tooltip={access.reason || undefined}
                  onClick={() => setAbandoningOpen(true)}
                >
                  Abandon proposal
                </Button>
              </HStack>
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
              detail={`${test.metric_count} fired ${test.metric_event} but only ${test.baseline_count} ${test.baseline_event} arrived — the snippet is not firing on every visit. Read the count, not the rate.`}
            />
          ) : null}
        </>
      ) : null}

      <Panel title="The plan">
        <VStack gap={2} align="stretch">
          <Field label="Owner" value={test.owner} />
          <Field label="Audience" value={test.audience} />
          <Field label="Success metric" value={test.success_metric} mono />
          <Field label="Guardrail metric" value={test.guardrail_metric} mono />
          <Field label="Review by" value={test.review_date ? formatDate(test.review_date) : ''} />
          <Field
            label="Baseline"
            value={
              typeof test.baseline_value === 'number'
                ? `${test.baseline_value}${test.baseline_unit ? ` ${test.baseline_unit}` : ''}${test.baseline_window ? ` · ${test.baseline_window}` : ''}`
                : ''
            }
          />
          <Field label="Evidence" value={evidenceLine({ evidence_json: test.evidence_json, created_at: test.created_at })} mono />
          {test.observation_id ? <Field label="Answers finding" value={test.observation_id} mono /> : null}
        </VStack>
      </Panel>

      <Panel
        title="Outcomes"
        action={
          state !== 'proposed' ? (
            <Button
              variant="outline"
              size="sm"
              disabled={!access.canWrite}
              tooltip={access.reason || undefined}
              onClick={() => setOutcomeOpen(true)}
            >
              Record an outcome
            </Button>
          ) : undefined
        }
      >
        {outcomes.length === 0 ? (
          <Text type="supporting">
            {state === 'proposed'
              ? 'Outcomes append once the experiment is committed — there is nothing to measure against yet.'
              : 'No measured outcomes yet. Entries are append-only: a correction is a new entry, never an overwrite.'}
          </Text>
        ) : (
          <VStack gap={2} align="stretch">
            {outcomes.map((o, i) => (
              <OutcomeRow key={`${o.recorded_at}-${i}`} entry={o} />
            ))}
          </VStack>
        )}
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
                          `My experiment "${test.hypothesis}" missed its threshold (${test.metric_count} of ${test.target_count}). Was that no demand, the wrong message, or too small a sample?`,
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
                    onClick={() => router.push(chatHref(`My experiment "${test.hypothesis}" passed. What is the smallest thing I should build first?`))}
                  >
                    Plan what to build
                  </Button>
                )}
                <Button
                  variant={state === 'passed' ? 'primary' : 'outline'}
                  size="sm"
                  disabled={deciding || !access.canWrite}
                  tooltip={access.reason || undefined}
                  onClick={() => decide(state, note.trim() || (state === 'passed' ? 'Threshold cleared' : 'Missed the committed threshold'))}
                >
                  {deciding ? 'Recording…' : `Close it as ${state}`}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={deciding || !access.canWrite}
                  tooltip={access.reason || undefined}
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

function Field({ label, value, mono = false }: { label: string; value?: string; mono?: boolean }) {
  return (
    <HStack gap={3} align="start" justify="between">
      <Text type="supporting" className="flex-none">{label}</Text>
      <Text type="supporting" className={`text-end ${mono ? 'font-mono' : ''}`}>
        {value?.trim() ? value : '—'}
      </Text>
    </HStack>
  );
}

function OutcomeRow({ entry }: { entry: TestOutcomeEntry }) {
  return (
    <div className="rounded-[var(--radius-md)] bg-[var(--color-background-muted)] px-3 py-2">
      <HStack gap={2} align="center" justify="between" className="flex-wrap">
        <Text weight="medium" className="tabular-nums">
          {entry.value}
          {entry.unit ? ` ${entry.unit}` : ''}
        </Text>
        <Text type="supporting">
          {entry.window ? `${entry.window} · ` : ''}
          {entry.author_kind === 'user' ? 'you' : 'agent'} · {formatRelative(entry.recorded_at)}
        </Text>
      </HStack>
      {entry.evidence_ref ? (
        <Text type="supporting" className="mt-1 block font-mono">{entry.evidence_ref}</Text>
      ) : null}
    </div>
  );
}

function OutcomeDialog({
  busy,
  onSubmit,
  onClose,
}: {
  busy: boolean;
  onSubmit: (v: { value: number; unit: string; window: string; evidence_ref: string }) => void | Promise<unknown>;
  onClose: () => void;
}) {
  const [value, setValue] = useState('');
  const [unit, setUnit] = useState('');
  const [window_, setWindow] = useState('');
  const [evidenceRef, setEvidenceRef] = useState('');

  const parsed = Number(value);
  const valid = value.trim() !== '' && Number.isFinite(parsed);

  // The dialog closes only after the append lands (the page does that once the
  // promise resolves). A rejection is the page's error to surface; swallowing
  // it here keeps the entered observation on screen to retry.
  async function submit() {
    if (!valid) return;
    try {
      await onSubmit({ value: parsed, unit: unit.trim(), window: window_.trim(), evidence_ref: evidenceRef.trim() });
    } catch {
      return;
    }
  }

  return (
    <Modal title="Record an outcome" onClose={onClose}>
      <VStack gap={4} align="stretch">
        <Text type="supporting">
          One measured observation — evidence, not a verdict. Entries are append-only; a correction is a new entry.
        </Text>
        <TextInput label="Value" value={value} placeholder="e.g. 42" onChange={setValue} width="100%" />
        <TextInput label="Unit" value={unit} placeholder="e.g. signups, weekly return %" onChange={setUnit} width="100%" />
        <TextInput label="Window" value={window_} placeholder="e.g. Sep 1–8" onChange={setWindow} width="100%" />
        <TextInput
          label="Evidence ref"
          value={evidenceRef}
          placeholder="saved query id, metric name+version, or bounded SQL"
          onChange={setEvidenceRef}
          width="100%"
        />
        <HStack gap={2} justify="end">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
          <Button variant="primary" size="sm" disabled={!valid || busy} onClick={() => void submit()}>
            {busy ? 'Recording…' : 'Record outcome'}
          </Button>
        </HStack>
      </VStack>
    </Modal>
  );
}
