'use client';

import type { ReactNode } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import { CircleDot, Clock, Play, Radio, TriangleAlert, Users, Webhook, Zap } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { HStack } from '@astryxdesign/core/HStack';
import { Table } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import type { Operator } from '@/lib/api';
import { formatCost, formatRelative } from '@/lib/format';
import { AppShell } from '@/modules/shared/components/app-shell';
import {
  Button,
  Callout,
  EmptyState,
  Intro,
  Loading,
  Panel,
  StatsStrip,
  StatusPill,
  rowNavPlugin,
} from '@/modules/shared/components/signal-primitives';
import { useOperations } from './hooks';
import { isTeamRun, lastOutcome, operatorStatus, operatorTitle, rank, runnerLabel, startsOn } from './lib/operator';

// /operations — the operate job, run N times.
//
// An operator is one trigger. Not a new object type: an agent with no trigger is
// a teammate you message, and an agent with a trigger is work that happens
// whether or not you open this tab. Until now the only place to see one was tab
// four of a single agent's setup page, which structurally cannot answer the
// question this screen exists for — "what runs without me, and did it work?"

const TRIGGER_ICON: Record<string, ReactNode> = {
  schedule: <Clock size={13} aria-hidden />,
  webhook: <Webhook size={13} aria-hidden />,
};

function triggerIcon(kind: string) {
  return TRIGGER_ICON[kind] ?? <Play size={13} aria-hidden />;
}

function RunnerBadge({ op }: { op: Operator }) {
  return (
    <Badge
      variant="neutral"
      label={
        <HStack gap={1} align="center">
          {isTeamRun(op) ? <Users size={12} aria-hidden /> : <CircleDot size={12} aria-hidden />}
          {runnerLabel(op)}
        </HStack>
      }
    />
  );
}

function StartsOn({ op }: { op: Operator }) {
  return (
    <HStack gap={1.5} align="center" className="text-[var(--color-text-secondary)]">
      {triggerIcon(op.kind)}
      <span className={op.kind === 'webhook' ? 'font-mono text-[12px]' : undefined}>{startsOn(op)}</span>
    </HStack>
  );
}

function LastRun({ op }: { op: Operator }) {
  const outcome = lastOutcome(op);
  return (
    // min-w-0: the summary is a whole sentence from the run, and a flex item's
    // default min-width is its content — without this the line refuses to shrink
    // and runs off the right edge of the 390px card instead of truncating.
    <VStack gap={0} className="min-w-0">
      <Text>{op.last_run_at ? formatRelative(op.last_run_at) : '—'}</Text>
      <Text type="supporting" maxLines={1} className={outcome.failed ? '!text-danger' : undefined}>
        {outcome.text}
      </Text>
    </VStack>
  );
}

function Controls({
  op,
  busy,
  onRun,
  onToggle,
}: {
  op: Operator;
  busy: boolean;
  onRun: () => void;
  onToggle: () => void;
}) {
  // The legacy project schedule is listed for completeness and edited where the
  // setting actually lives — pausing it from here would change the autonomy rung
  // every unattended run in the project is gated on.
  if (op.source === 'config') {
    return (
      <Button variant="ghost" size="sm" onClick={onRun} disabled={busy}>
        Run now
      </Button>
    );
  }
  return (
    <>
      <Button variant="outline" size="sm" icon={<Play size={13} aria-hidden />} onClick={onRun} disabled={busy}>
        Run now
      </Button>
      <Button variant="ghost" size="sm" onClick={onToggle} disabled={busy}>
        {op.enabled ? 'Pause' : 'Resume'}
      </Button>
    </>
  );
}

// OperatorCard is the sub-768px form of one row. Astryx's Table does not scroll
// at 390px — it compresses, and six columns collapse to about one character per
// line — so the narrow layout is a stacked card, not a scroller.
function OperatorCard({
  op,
  busy,
  onOpen,
  onRun,
  onToggle,
}: {
  op: Operator;
  busy: boolean;
  onOpen: () => void;
  onRun: () => void;
  onToggle: () => void;
}) {
  const { status, label } = operatorStatus(op);
  return (
    <VStack gap={2} align="stretch" className="border-t border-[var(--color-border)] pt-3 first:border-t-0 first:pt-0">
      <HStack gap={2} align="center" justify="between">
        <button type="button" onClick={onOpen} className="min-w-0 text-start">
          <Text weight="medium">{operatorTitle(op)}</Text>
        </button>
        <StatusPill status={status} label={label} grow={false} />
      </HStack>
      {op.prompt_template ? (
        <Text type="supporting" maxLines={2}>
          {op.prompt_template}
        </Text>
      ) : null}
      <HStack gap={2} align="center" className="flex-wrap">
        <StartsOn op={op} />
        <RunnerBadge op={op} />
      </HStack>
      <HStack gap={2} align="center" justify="between" className="flex-wrap">
        <div className="min-w-0 flex-1">
          <LastRun op={op} />
        </div>
        <HStack gap={1}>
          <Controls op={op} busy={busy} onRun={onRun} onToggle={onToggle} />
        </HStack>
      </HStack>
    </VStack>
  );
}

const STATUS_FILTERS = ['attention', 'working', 'healthy', 'paused'];

export function OperationsPage() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const { operators, isLoading, error, refetch, setEnabled, savingID, runNow, runningID } = useOperations();

  // Deep-linkable: /operations?status=attention is what the failure Callout in
  // Chat and the alert mail both link to.
  const statusFilter = searchParams.get('status') ?? '';
  const filtered = STATUS_FILTERS.includes(statusFilter)
    ? operators.filter((op) => operatorStatus(op).status === statusFilter)
    : operators;
  const rows = [...filtered].sort((a, b) => rank(a) - rank(b));

  const armed = operators.filter((op) => operatorStatus(op).status !== 'paused').length;
  const working = operators.filter((op) => operatorStatus(op).status === 'working').length;
  const failing = operators.filter((op) => operatorStatus(op).status === 'attention');
  const runs24h = operators.reduce((sum, op) => sum + op.runs_24h, 0);
  const spend24h = operators.reduce((sum, op) => sum + op.cost_24h, 0);
  const needsReview = failing[0];
  const busyID = savingID ?? runningID;

  const open = (op: Operator) => router.push(`/operations/${encodeURIComponent(op.id)}`);

  return (
    <AppShell active="operations">
      <Intro
        title="Operations"
        sub="Work that starts without you — on a schedule, from a webhook, or on demand."
        action={
          <Button variant="primary" icon={<Zap size={15} aria-hidden />} onClick={() => router.push('/agents')}>
            New operator
          </Button>
        }
      />
      <StatsStrip
        stats={[
          { label: 'Armed', value: String(armed), tone: armed ? 'success' : undefined },
          { label: 'Working now', value: String(working), tone: working ? 'agent' : undefined },
          { label: 'Needs attention', value: String(failing.length), tone: failing.length ? 'danger' : undefined },
          { label: 'Paused', value: String(operators.length - armed) },
          { label: 'Runs (24h)', value: String(runs24h) },
          { label: 'Spend (24h)', value: formatCost(spend24h) },
        ]}
      />

      {/* An empty list and a failed read must never look the same. This says what
          broke AND what did not: the operators keep running while the page is
          blind, so nobody reaches for a "restart everything" they don't need. */}
      {error ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Can’t read"
          title="We couldn’t load your operators"
          detail="The API didn’t answer. Nothing has been paused — your operators keep running while this page is blind."
          action={
            <Button variant="outline" size="sm" onClick={refetch}>
              Try again
            </Button>
          }
        />
      ) : null}

      {needsReview ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Needs review"
          title={`${operatorTitle(needsReview)} failed ${needsReview.consecutive_failures > 1 ? `its last ${needsReview.consecutive_failures} runs` : 'its last run'}`}
          detail={
            needsReview.last_summary
              ? `Last error: ${needsReview.last_summary}`
              : 'Nobody was told — the operator that would tell you is the one that broke.'
          }
          action={
            <Button variant="agent" size="sm" onClick={() => open(needsReview)}>
              Inspect
            </Button>
          }
        />
      ) : null}

      {/* The Operators panel carries no `action`: its header sits directly above
          the table header, so a right-aligned caption lands on top of the
          ACTIONS column — colliding with it and squeezing "Run now" into
          "un now". The ordering is legible from the rows themselves. */}
      {isLoading && operators.length === 0 ? (
        <Loading label="Reading what runs unattended…" />
      ) : (
        <Panel title="Operators">
          {rows.length === 0 ? (
            error ? (
              <EmptyState
                icon={<Radio size={18} aria-hidden />}
                title="Nothing to show while we’re disconnected"
                detail="This list is empty because the request failed — not because you have no operators."
              />
            ) : statusFilter ? (
              <EmptyState
                icon={<Clock size={18} aria-hidden />}
                title={`Nothing is ${statusFilter === 'healthy' ? 'armed' : statusFilter}`}
                detail="No operator is in that state right now."
                action={
                  <Button variant="outline" size="sm" onClick={() => router.replace('/operations')}>
                    Show all operators
                  </Button>
                }
              />
            ) : (
              <EmptyState
                icon={<Clock size={18} aria-hidden />}
                title="Nothing runs unattended yet"
                detail="Every teammate you have hired only works when you message it. Give one a schedule or a webhook and the work happens whether or not you open this tab."
                action={
                  <Button variant="agent" size="sm" onClick={() => router.push('/agents')}>
                    Give a teammate a schedule
                  </Button>
                }
              />
            )
          ) : (
            <>
              <VStack gap={3} align="stretch" className="md:hidden">
                {rows.map((op) => (
                  <OperatorCard
                    key={op.id}
                    op={op}
                    busy={busyID === op.id}
                    onOpen={() => open(op)}
                    onRun={() => runNow(op.id)}
                    onToggle={() => setEnabled(op, !op.enabled)}
                  />
                ))}
              </VStack>
              <div className="hidden md:block">
                <Table
                  data={rows}
                  idKey="id"
                  density="compact"
                  hasHover
                  plugins={{ nav: rowNavPlugin<Operator>((row) => open(row)) }}
                  columns={[
                    {
                      key: 'name',
                      header: 'Operator',
                      align: 'start',
                      renderCell: (row) => (
                        <VStack gap={0}>
                          <Text weight="medium">{operatorTitle(row)}</Text>
                          <Text type="supporting" maxLines={1}>
                            {row.prompt_template || 'No prompt set — it runs the default health check.'}
                          </Text>
                        </VStack>
                      ),
                    },
                    { key: 'trigger', header: 'Starts on', align: 'start', renderCell: (row) => <StartsOn op={row} /> },
                    { key: 'runner', header: 'Runs', align: 'start', renderCell: (row) => <RunnerBadge op={row} /> },
                    {
                      key: 'status',
                      header: 'Status',
                      align: 'start',
                      renderCell: (row) => {
                        const { status, label } = operatorStatus(row);
                        return <StatusPill status={status} label={label} grow={false} />;
                      },
                    },
                    { key: 'last', header: 'Last run', align: 'start', renderCell: (row) => <LastRun op={row} /> },
                    {
                      key: 'act',
                      header: 'Actions',
                      align: 'end',
                      // The row navigates on click, so the controls must swallow
                      // theirs: without this, "Pause" pauses AND walks the owner
                      // off the list they were triaging.
                      renderCell: (row) => (
                        <div
                          role="presentation"
                          className="flex justify-end gap-1"
                          onClick={(e) => e.stopPropagation()}
                        >
                          <Controls
                            op={row}
                            busy={busyID === row.id}
                            onRun={() => runNow(row.id)}
                            onToggle={() => setEnabled(row, !row.enabled)}
                          />
                        </div>
                      ),
                    },
                  ]}
                />
              </div>
            </>
          )}
        </Panel>
      )}
    </AppShell>
  );
}
