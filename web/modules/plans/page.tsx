'use client';

import { useMemo } from 'react';
import { useRouter } from 'next/navigation';
import { Beaker, Lightbulb, Target, TriangleAlert } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { Card } from '@astryxdesign/core/Card';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import type { AgentRecommendation, MeasuredTest } from '@/lib/api';
import { formatCompact, formatDate, formatRelative } from '@/lib/format';
import { useProjectAccess } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { Button, Callout, EmptyState, Loading, Panel, StatsStrip, StatusPill } from '@/modules/shared/components/signal-primitives';
import { chatHref, groupTests, stateOf } from '@/modules/prototypes/lib/prototype';
import { useExperiments, useFindings } from './hooks';
import { baselineLine, evidenceLine, EXPERIMENT_LABEL, EXPERIMENT_PILL, FINDING_LABEL, FINDING_PILL } from './lib/plans';

// /plans — findings and experiments: what the agents noticed, and the bets the
// owner has agreed to be judged by. Experiments are the validation_tests rows
// the prototypes surface already renders; findings are the agent's written
// observations (agent_recommendations). The two belong on one screen because
// an experiment is how a finding gets answered.

const DESIGN_PROMPT = 'Design the cheapest test that could prove my next feature wrong.';

export function PlansPage() {
  const router = useRouter();
  const access = useProjectAccess();
  const experiments = useExperiments();
  const findings = useFindings();

  const { waiting, running, decided } = groupTests(experiments.tests);
  const openFindings = findings.findings.filter((f) => f.status === 'open');
  const settledFindings = findings.findings.filter((f) => f.status !== 'open');

  const openExperiment = (t: MeasuredTest) => router.push(`/plans/${encodeURIComponent(t.id)}`);

  const columns = useMemo<DataColumn<MeasuredTest>[]>(() => [
    {
      key: 'hypothesis',
      header: 'Experiment',
      width: { type: 'proportional', value: 3, minWidth: 200 },
      renderCell: (t) => (
        <VStack gap={1} className="min-w-0">
          <Text weight="medium">{t.hypothesis}</Text>
          <Text type="supporting" className="font-mono">{baselineLine(t)}</Text>
        </VStack>
      ),
    },
    {
      key: 'status',
      header: 'Status',
      width: { type: 'proportional', value: 1, minWidth: 130 },
      sortValue: (t) => t.status,
      renderCell: (t) => (
        <StatusPill
          status={EXPERIMENT_PILL[t.status] ?? 'paused'}
          label={EXPERIMENT_LABEL[t.status] ?? t.status}
          grow={false}
        />
      ),
    },
    {
      key: 'owner',
      header: 'Owner',
      width: { type: 'proportional', value: 1, minWidth: 80 },
      renderCell: (t) => <span className="text-[var(--color-text-secondary)]">{t.owner || '—'}</span>,
    },
    {
      key: 'review_date',
      header: 'Review by',
      width: { type: 'proportional', value: 1, minWidth: 90 },
      sortValue: (t) => t.review_date ?? '',
      renderCell: (t) => (
        <span className="text-[var(--color-text-secondary)]">
          {t.review_date ? formatDate(t.review_date, { month: 'short', day: 'numeric' }) : '—'}
        </span>
      ),
    },
    {
      key: 'created_at',
      header: 'Opened',
      width: { type: 'proportional', value: 1, minWidth: 80 },
      sortValue: (t) => t.created_at,
      renderCell: (t) => <span className="text-[var(--color-text-secondary)]">{formatRelative(t.created_at)}</span>,
    },
  ], []);

  const isLoading = experiments.isLoading || findings.isLoading;
  const error = experiments.error ?? findings.error;
  const empty = experiments.tests.length === 0 && findings.findings.length === 0;

  return (
    <AppShell
      title="Plans"
      sub="What the agents noticed, and the bets you have agreed to be judged by. A finding becomes an experiment; an experiment ends in a decision."
      actions={
        <Button variant="agent" icon={<Beaker size={15} aria-hidden />} onClick={() => router.push(chatHref(DESIGN_PROMPT))}>
          Design an experiment
        </Button>
      }
    >
      <StatsStrip
        stats={[
          { label: 'Open findings', value: String(openFindings.length), tone: openFindings.length ? 'agent' : undefined },
          { label: 'Awaiting agreement', value: String(waiting.length), tone: waiting.length ? 'warning' : undefined },
          { label: 'Running', value: String(running.length) },
          { label: 'Decided', value: String(decided.length) },
          { label: 'On the waitlist', value: String(experiments.waitlistCount) },
        ]}
      />

      {error ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Can’t read"
          title="We couldn’t load your plans"
          detail="The API didn’t answer. Nothing has been decided or reset — every committed experiment is still counting."
          action={
            <Button variant="outline" size="sm" onClick={() => { experiments.refetch(); findings.refetch(); }}>
              Try again
            </Button>
          }
        />
      ) : null}

      {waiting.length > 0 ? (
        <Callout
          tone="agentic"
          icon={<Target size={18} aria-hidden />}
          label="Waiting on you"
          title={
            waiting.length === 1
              ? 'A teammate proposed an experiment you have not agreed to'
              : `${waiting.length} proposed experiments are waiting for you to agree to a number`
          }
          detail="Nothing is counted until you commit. A threshold picked after the result is not a threshold."
          action={
            <Button variant="agent" size="sm" onClick={() => openExperiment(waiting[0])}>
              Review it
            </Button>
          }
        />
      ) : null}

      {experiments.truncated ? (
        <Text type="supporting" className="block">
          Showing the {experiments.tests.length} most recent of {experiments.total} experiments — proposals and running tests first.
        </Text>
      ) : null}

      {isLoading && empty ? (
        <Loading label="Reading findings and experiments…" />
      ) : empty ? (
        <Panel title="Plans">
          {error ? (
            <EmptyState
              icon={<Beaker size={18} aria-hidden />}
              title="Nothing to show while we’re disconnected"
              detail="This page is empty because the request failed — not because you have no plans."
            />
          ) : (
            <EmptyState
              icon={<Beaker size={18} aria-hidden />}
              title="No plans yet"
              detail="A plan is a finding the agent wrote down, or an experiment — one idea with a kill/keep number on it. Ask a teammate for the cheapest test that could prove your next feature wrong, then commit to the number here."
              action={
                <Button variant="agent" size="sm" onClick={() => router.push(chatHref(DESIGN_PROMPT))}>
                  Design the experiment
                </Button>
              }
            />
          )}
        </Panel>
      ) : (
        <VStack gap={4} align="stretch">
          <Panel title="Experiments">
            {experiments.tests.length === 0 ? (
              <EmptyState
                icon={<Beaker size={18} aria-hidden />}
                title="No experiments yet"
                detail="An experiment is one idea with a kill/keep number agreed before the data arrives. Ask a teammate to design the cheapest test that could prove your next feature wrong."
              />
            ) : (
              <DataTable columns={columns} data={experiments.tests} pageSize={10} onRowClick={openExperiment} />
            )}
          </Panel>

          <Panel title="Findings">
            {findings.findings.length === 0 ? (
              <EmptyState
                icon={<Lightbulb size={18} aria-hidden />}
                title="No findings yet"
                detail="Findings are what a scheduled or chat agent noticed in your data and wrote down — each one is a candidate experiment."
              />
            ) : (
              <VStack gap={3} align="stretch">
                {openFindings.map((f) => (
                  <FindingCard
                    key={f.id}
                    finding={f}
                    canWrite={access.canWrite}
                    accessReason={access.reason}
                    busy={findings.ackingID === f.id}
                    onAck={(status) => findings.ack(f.id, status)}
                  />
                ))}
                {settledFindings.map((f) => (
                  <FindingCard key={f.id} finding={f} canWrite={false} busy={false} onAck={() => undefined} />
                ))}
                {findings.hasMore ? (
                  <Button variant="outline" size="sm" disabled={findings.loadingMore} onClick={findings.loadMore}>
                    {findings.loadingMore ? 'Loading…' : 'Load older findings'}
                  </Button>
                ) : null}
              </VStack>
            )}
          </Panel>
        </VStack>
      )}
    </AppShell>
  );
}

function FindingCard({
  finding,
  canWrite,
  accessReason,
  busy,
  onAck,
}: {
  finding: AgentRecommendation;
  canWrite: boolean;
  accessReason?: string;
  busy: boolean;
  onAck: (status: 'accepted' | 'dismissed') => void;
}) {
  const open = finding.status === 'open';
  return (
    <Card padding={3}>
      <HStack align="start" justify="between" gap={3}>
        <VStack gap={1} className="min-w-0">
          <HStack gap={2} align="center" className="flex-wrap">
            <StatusPill
              status={FINDING_PILL[finding.status] ?? 'paused'}
              label={FINDING_LABEL[finding.status] ?? finding.status}
              grow={false}
            />
            {finding.category ? <Badge variant="neutral" label={finding.category} /> : null}
            {finding.seen_count > 1 ? (
              <Text type="supporting">seen {formatCompact(finding.seen_count)}×</Text>
            ) : null}
          </HStack>
          <Text weight="medium">{finding.title}</Text>
          <Text type="supporting" className="leading-[1.45]">{finding.rationale}</Text>
          <Text type="supporting" className="font-mono">
            {evidenceLine(finding)} · last seen {formatRelative(finding.last_seen_at)}
          </Text>
        </VStack>
        {open ? (
          <HStack gap={2} className="flex-none">
            <Button
              variant="primary"
              size="sm"
              disabled={busy || !canWrite}
              tooltip={accessReason || undefined}
              onClick={() => onAck('accepted')}
            >
              Act
            </Button>
            <Button
              variant="ghost"
              size="sm"
              disabled={busy || !canWrite}
              tooltip={accessReason || undefined}
              onClick={() => onAck('dismissed')}
            >
              Skip
            </Button>
          </HStack>
        ) : null}
      </HStack>
    </Card>
  );
}
