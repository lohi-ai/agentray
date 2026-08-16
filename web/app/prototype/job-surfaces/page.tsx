'use client';

// PROTOTYPE — bs-h0nibs69. Throwaway design surface for the two recurring-job
// screens: /operations (the operate job, run N times) and /prototypes (the
// validate job, run N times). Not linked from navigation, not wired to the API,
// no shared state. `implement` rebuilds these properly inside
// web/modules/operations/ and web/modules/prototypes/.
// Spec: <ticket>/design.md
//
// Everything here is composed from the SAME primitives the real screens use —
// signal-primitives (Intro/StatsStrip/StatusPill/Callout/Panel/EmptyState/
// Segment/Button) over Astryx Table/Badge — so what the human judges is whether
// these look like they were always part of the product. The one deviation from
// production is AppShell: the prototype renders inside a plain <main> so it can
// be opened without a session.

import { useState } from 'react';
import {
  Beaker,
  CircleDot,
  Clock,
  Play,
  Radio,
  Target,
  TriangleAlert,
  Users,
  Webhook,
  Zap,
} from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Table } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import {
  Button,
  Callout,
  EmptyState,
  Intro,
  Panel,
  Segment,
  StatsStrip,
  StatusPill,
  rowNavPlugin,
} from '@/modules/shared/components/signal-primitives';

// ---------------------------------------------------------------------------
// Mock data. Shapes mirror the read models the spec proposes, not new tables:
// an Operator is one agent_triggers row joined to its agent/team + run
// aggregates; a Prototype is one validation_tests row + its progress.
// ---------------------------------------------------------------------------

type Operator = {
  id: string;
  name: string;
  does: string;
  trigger: { kind: 'schedule' | 'webhook' | 'manual'; detail: string };
  runner: { label: string; kind: 'agent' | 'team' };
  enabled: boolean;
  running: number;
  errors: number;
  lastRun: string;
  lastOutcome: string;
};

const OPERATORS: Operator[] = [
  {
    id: 'op-1',
    name: 'Morning health sweep',
    does: 'Perform your scheduled check. Inspect ingestion gaps, error spikes, latency and cost drift.',
    trigger: { kind: 'schedule', detail: 'Every day 07:00' },
    runner: { label: 'Ops Watch', kind: 'agent' },
    enabled: true,
    running: 0,
    errors: 2,
    lastRun: '14m ago',
    lastOutcome: 'Failed — run_sql denied',
  },
  {
    id: 'op-2',
    name: 'Weekly marketing plan',
    does: 'Read last week’s traffic and activation, then draft the content plan and the two posts to publish.',
    trigger: { kind: 'schedule', detail: 'Mondays 09:00' },
    runner: { label: 'Growth pod', kind: 'team' },
    enabled: true,
    running: 1,
    errors: 0,
    lastRun: 'running now',
    lastOutcome: 'Working',
  },
  {
    id: 'op-3',
    name: 'Deploy → tracking check',
    does: 'A deploy landed. Confirm every event in the tracking plan is still arriving, and file what stopped.',
    trigger: { kind: 'webhook', detail: 'POST /hooks/…a41f' },
    runner: { label: 'Tracking Steward', kind: 'agent' },
    enabled: true,
    running: 0,
    errors: 0,
    lastRun: '2d ago',
    lastOutcome: 'Clean — 18 events checked',
  },
  {
    id: 'op-4',
    name: 'Friday retention digest',
    does: 'Summarise who came back this week and what they did first, and send it to Slack.',
    trigger: { kind: 'schedule', detail: 'Fridays 16:00' },
    runner: { label: 'Insight Digest', kind: 'agent' },
    enabled: false,
    running: 0,
    errors: 0,
    lastRun: '3w ago',
    lastOutcome: 'Paused by you',
  },
];

type Prototype = {
  id: string;
  hypothesis: string;
  metricEvent: string;
  metric: number;
  target: number;
  daysLeft: number;
  waitlist: number;
  status: 'proposed' | 'committed' | 'passed' | 'failed';
  proposedBy: string;
};

const PROTOTYPES: Prototype[] = [
  {
    id: 'pr-1',
    hypothesis: 'Readers will pay for an audio version of a chapter they are already halfway through.',
    metricEvent: 'audio.preview_played',
    metric: 0,
    target: 40,
    daysLeft: 14,
    waitlist: 0,
    status: 'proposed',
    proposedBy: 'Product Scout',
  },
  {
    id: 'pr-2',
    hypothesis: 'Solo translators will give an email for a weekly glossary-drift report.',
    metricEvent: 'waitlist.joined',
    metric: 27,
    target: 40,
    daysLeft: 6,
    waitlist: 27,
    status: 'committed',
    proposedBy: 'Product Scout',
  },
  {
    id: 'pr-3',
    hypothesis: 'Existing readers will turn on a per-novel notification if it is offered at the end of a chapter.',
    metricEvent: 'notify.enabled',
    metric: 112,
    target: 60,
    daysLeft: 0,
    waitlist: 0,
    status: 'passed',
    proposedBy: 'Growth Lead',
  },
  {
    id: 'pr-4',
    hypothesis: 'A public leaderboard will pull lapsed readers back within a week.',
    metricEvent: 'leaderboard.viewed',
    metric: 9,
    target: 50,
    daysLeft: 0,
    waitlist: 4,
    status: 'failed',
    proposedBy: 'Growth Lead',
  },
];

// ---------------------------------------------------------------------------
// Shared cell renderers
// ---------------------------------------------------------------------------

const TRIGGER_ICON = {
  schedule: <Clock size={13} aria-hidden />,
  webhook: <Webhook size={13} aria-hidden />,
  manual: <Play size={13} aria-hidden />,
};

// operatorStatus is the one rule the whole screen reads from: an operator is
// armed, working, paused, or needs attention. Copied from the agent-monitor
// screen (modules/agents/monitor/page.tsx) so the two fleets speak one language.
function operatorStatus(op: Operator): { status: string; label: string } {
  if (!op.enabled) return { status: 'paused', label: 'Paused' };
  if (op.errors > 0) return { status: 'attention', label: 'Attention' };
  if (op.running > 0) return { status: 'working', label: 'Working' };
  return { status: 'healthy', label: 'Armed' };
}

function rank(op: Operator): number {
  if (op.enabled && op.errors > 0) return 0;
  if (op.enabled && op.running > 0) return 1;
  if (op.enabled) return 2;
  return 3;
}

// OperatorCard is the sub-768px form of one table row. Astryx's <Table> does
// not scroll at 390px — it *compresses*, and six columns collapse to one
// character per line. So the narrow layout is a stacked card, not a scroller:
// the same five facts, read top to bottom.
function OperatorCard({ op }: { op: Operator }) {
  const { status, label } = operatorStatus(op);
  return (
    <VStack gap={2} align="stretch" className="border-t border-[var(--color-border)] pt-3 first:border-t-0 first:pt-0">
      <HStack gap={2} align="center" justify="between">
        <Text weight="medium">{op.name}</Text>
        <StatusPill status={status} label={label} grow={false} />
      </HStack>
      <Text type="supporting" maxLines={2}>
        {op.does}
      </Text>
      <HStack gap={2} align="center" className="flex-wrap text-[var(--color-text-secondary)]">
        {TRIGGER_ICON[op.trigger.kind]}
        <span className={op.trigger.kind === 'webhook' ? 'font-mono text-[12px]' : undefined}>{op.trigger.detail}</span>
        <Badge
          variant="neutral"
          label={
            <HStack gap={1} align="center">
              {op.runner.kind === 'team' ? <Users size={12} aria-hidden /> : <CircleDot size={12} aria-hidden />}
              {op.runner.label}
            </HStack>
          }
        />
      </HStack>
      <HStack gap={2} align="center" justify="between">
        <VStack gap={0}>
          <Text>{op.lastRun}</Text>
          <Text type="supporting" className={op.errors > 0 ? '!text-danger' : undefined}>
            {op.lastOutcome}
          </Text>
        </VStack>
        <HStack gap={1}>
          <Button variant="outline" size="sm" icon={<Play size={13} aria-hidden />}>
            Run now
          </Button>
          <Button variant="ghost" size="sm">
            {op.enabled ? 'Pause' : 'Resume'}
          </Button>
        </HStack>
      </HStack>
    </VStack>
  );
}

// ---------------------------------------------------------------------------
// /operations
// ---------------------------------------------------------------------------

function OperationsScreen({ empty = false }: { empty?: boolean }) {
  const rows = empty ? [] : [...OPERATORS].sort((a, b) => rank(a) - rank(b));
  const armed = rows.filter((o) => o.enabled).length;
  const working = rows.filter((o) => o.running > 0).length;
  const failing = rows.filter((o) => o.errors > 0).length;

  return (
    <>
      <Intro
        title="Operations"
        sub="Work that starts without you — on a schedule, from a webhook, or on demand."
        action={
          <Button variant="primary" icon={<Zap size={15} />}>
            New operator
          </Button>
        }
      />
      <StatsStrip
        stats={[
          { label: 'Armed', value: String(armed), tone: armed ? 'success' : undefined },
          { label: 'Working now', value: String(working), tone: working ? 'agent' : undefined },
          { label: 'Needs attention', value: String(failing), tone: failing ? 'danger' : undefined },
          { label: 'Paused', value: String(rows.length - armed) },
          { label: 'Runs (24h)', value: empty ? '0' : '38' },
          { label: 'Spend (24h)', value: empty ? '$0.00' : '$1.42' },
        ]}
      />
      {failing > 0 ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Needs review"
          title="Morning health sweep failed its last 2 runs"
          detail="It has been failing since Tuesday 07:00. Nobody was told, because the operator that tells you is the one that broke."
          action={
            <Button variant="agent" size="sm">
              Inspect
            </Button>
          }
        />
      ) : null}
      <Panel
        title="Operators"
        action={
          rows.length > 0 ? (
            <Text type="supporting" className="uppercase tracking-[0.08em]">
              failures &amp; active first
            </Text>
          ) : null
        }
      >
        {rows.length === 0 ? (
          <EmptyState
            icon={<Clock size={18} aria-hidden />}
            title="Nothing runs unattended yet"
            detail="Every teammate you have hired only works when you message it. Give one a schedule or a webhook and the work happens whether or not you open this tab."
            action={
              <Button variant="agent" size="sm">
                Give a teammate a schedule
              </Button>
            }
          />
        ) : (
          <>
          <VStack gap={3} align="stretch" className="md:hidden">
            {rows.map((op) => (
              <OperatorCard key={op.id} op={op} />
            ))}
          </VStack>
          <div className="hidden md:block">
          <Table
            data={rows}
            idKey="id"
            density="compact"
            hasHover
            plugins={{ nav: rowNavPlugin<Operator>(() => {}) }}
            columns={[
              {
                key: 'name',
                header: 'Operator',
                align: 'start',
                renderCell: (row) => (
                  <VStack gap={0}>
                    <Text weight="medium">{row.name}</Text>
                    <Text type="supporting" maxLines={1}>
                      {row.does}
                    </Text>
                  </VStack>
                ),
              },
              {
                key: 'trigger',
                header: 'Starts on',
                align: 'start',
                renderCell: (row) => (
                  <HStack gap={1.5} align="center" className="text-[var(--color-text-secondary)]">
                    {TRIGGER_ICON[row.trigger.kind]}
                    <span className={row.trigger.kind === 'webhook' ? 'font-mono text-[12px]' : undefined}>
                      {row.trigger.detail}
                    </span>
                  </HStack>
                ),
              },
              {
                key: 'runner',
                header: 'Runs',
                align: 'start',
                renderCell: (row) => (
                  <Badge
                    variant="neutral"
                    label={
                      <HStack gap={1} align="center">
                        {row.runner.kind === 'team' ? <Users size={12} aria-hidden /> : <CircleDot size={12} aria-hidden />}
                        {row.runner.label}
                      </HStack>
                    }
                  />
                ),
              },
              {
                key: 'status',
                header: 'Status',
                align: 'start',
                renderCell: (row) => {
                  const { status, label } = operatorStatus(row);
                  return <StatusPill status={status} label={label} grow={false} />;
                },
              },
              {
                key: 'last',
                header: 'Last run',
                align: 'start',
                renderCell: (row) => (
                  <VStack gap={0}>
                    <Text>{row.lastRun}</Text>
                    <Text type="supporting" className={row.errors > 0 ? '!text-danger' : undefined}>
                      {row.lastOutcome}
                    </Text>
                  </VStack>
                ),
              },
              {
                key: 'act',
                header: 'Actions',
                align: 'end',
                renderCell: (row) => (
                  <HStack gap={1} justify="end">
                    <Button variant="outline" size="sm" icon={<Play size={13} aria-hidden />}>
                      Run now
                    </Button>
                    <Button variant="ghost" size="sm">
                      {row.enabled ? 'Pause' : 'Resume'}
                    </Button>
                  </HStack>
                ),
              },
            ]}
          />
          </div>
          </>
        )}
      </Panel>
    </>
  );
}

// ---------------------------------------------------------------------------
// /prototypes
// ---------------------------------------------------------------------------

const PROTOTYPE_TONE: Record<Prototype['status'], string> = {
  proposed: 'var(--agent)',
  // --data (not a neutral border token): a running test is a measurement in
  // progress, and its bar has to be readable at 10% fill on the dark ramp.
  committed: 'var(--data)',
  passed: 'var(--success)',
  failed: 'var(--danger)',
};

// VERDICT_LABEL keeps the outcome in words. The left rule and the bar are
// colored, but color never carries the verdict on its own (ux-check §37).
const VERDICT_LABEL: Record<Prototype['status'], string> = {
  proposed: 'Waiting on you',
  committed: 'Running',
  passed: 'Passed',
  failed: 'Failed',
};

function PrototypeCard({ p }: { p: Prototype }) {
  const pct = p.target > 0 ? Math.min(100, (p.metric / p.target) * 100) : 0;
  const proposed = p.status === 'proposed';

  return (
    <div
      className="border-s-2 ps-3 py-1"
      style={{ borderInlineStartColor: PROTOTYPE_TONE[p.status] }}
    >
      {/* Below md the action drops under the hypothesis — side by side, the
          sentence gets ~120px and wraps to one word per line. */}
      <HStack align="start" justify="between" gap={3} className="!flex-col md:!flex-row">
        <VStack gap={1} className="min-w-0">
          <HStack gap={2} align="center" className="flex-wrap">
            <StatusPill
              status={p.status === 'passed' ? 'healthy' : p.status === 'failed' ? 'attention' : p.status === 'proposed' ? 'working' : 'idle'}
              label={VERDICT_LABEL[p.status]}
              grow={false}
            />
            <Text weight="medium">{p.hypothesis}</Text>
          </HStack>
          <HStack gap={2} align="center" className="flex-wrap">
            <Badge variant="neutral" label={<code>{p.metricEvent}</code>} />
            <Text type="supporting">
              {proposed
                ? `${p.proposedBy} proposes ${p.target} people in ${p.daysLeft} days`
                : `${p.metric} of ${p.target}${p.daysLeft > 0 ? ` · ${p.daysLeft} days left` : ''}${p.waitlist > 0 ? ` · ${p.waitlist} on the waitlist` : ''}`}
            </Text>
          </HStack>
        </VStack>
        {proposed ? (
          <Button variant="agent" size="sm">
            Commit to this number
          </Button>
        ) : p.status === 'passed' ? (
          <Button variant="primary" size="sm">
            Build it
          </Button>
        ) : p.status === 'failed' ? (
          <Button variant="outline" size="sm">
            Ask which it was
          </Button>
        ) : (
          <Button variant="ghost" size="sm">
            Open
          </Button>
        )}
      </HStack>
      {!proposed ? (
        <div
          className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-[var(--color-background-muted)]"
          role="progressbar"
          aria-valuenow={p.metric}
          aria-valuemin={0}
          aria-valuemax={p.target}
          aria-label={`${p.metric} of ${p.target} toward the committed threshold for ${p.metricEvent}`}
        >
          <div
            className="h-full rounded-full"
            style={{ width: `${pct}%`, background: PROTOTYPE_TONE[p.status] }}
          />
        </div>
      ) : null}
    </div>
  );
}

function PrototypesScreen({ empty = false }: { empty?: boolean }) {
  const all = empty ? [] : PROTOTYPES;
  const waiting = all.filter((p) => p.status === 'proposed');
  const running = all.filter((p) => p.status === 'committed');
  const decided = all.filter((p) => p.status === 'passed' || p.status === 'failed');

  return (
    <>
      <Intro
        title="Prototypes"
        sub="One bet per idea. The number is agreed before the data arrives — and you can’t move it after."
        action={
          <Button variant="agent" icon={<Beaker size={15} />}>
            Start a prototype
          </Button>
        }
      />
      <StatsStrip
        stats={[
          { label: 'Waiting on you', value: String(waiting.length), tone: waiting.length ? 'agent' : undefined },
          { label: 'Running', value: String(running.length) },
          { label: 'Passed', value: String(all.filter((p) => p.status === 'passed').length), tone: 'success' },
          { label: 'Failed', value: String(all.filter((p) => p.status === 'failed').length) },
          { label: 'On waitlists', value: String(all.reduce((s, p) => s + p.waitlist, 0)) },
        ]}
      />
      {waiting.length > 0 ? (
        <Callout
          tone="agentic"
          icon={<Target size={18} aria-hidden />}
          label="Waiting on you"
          title={`${waiting[0].proposedBy} proposed a test you have not agreed to`}
          detail="Nothing is counted until you commit. A threshold picked after the result is not a threshold."
          action={
            <Button variant="agent" size="sm">
              Review it
            </Button>
          }
        />
      ) : null}

      {all.length === 0 ? (
        <Panel title="Prototypes">
          <EmptyState
            icon={<Beaker size={18} aria-hidden />}
            title="No prototypes yet"
            detail="A prototype is one idea with a kill/keep number on it. Ask a teammate for the cheapest test that could prove your next feature wrong, then commit to the number here."
            action={
              <Button variant="agent" size="sm">
                Design the test
              </Button>
            }
          />
        </Panel>
      ) : (
        <VStack gap={4} align="stretch">
          {waiting.length > 0 ? (
            <Panel title="Waiting on you">
              <VStack gap={4} align="stretch">
                {waiting.map((p) => (
                  <PrototypeCard key={p.id} p={p} />
                ))}
              </VStack>
            </Panel>
          ) : null}
          <Panel title="Running">
            {running.length === 0 ? (
              <Text type="supporting">Nothing is being measured right now.</Text>
            ) : (
              <VStack gap={4} align="stretch">
                {running.map((p) => (
                  <PrototypeCard key={p.id} p={p} />
                ))}
              </VStack>
            )}
          </Panel>
          <Panel title="Decided">
            <VStack gap={4} align="stretch">
              {decided.map((p) => (
                <PrototypeCard key={p.id} p={p} />
              ))}
            </VStack>
          </Panel>
        </VStack>
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// Error state — shared by both screens. The rule from ia.ts formatAgentError:
// name what broke and the one control that fixes it, never a raw error.
// ---------------------------------------------------------------------------

function ErrorScreen() {
  return (
    <>
      <Intro title="Operations" sub="Work that starts without you — on a schedule, from a webhook, or on demand." />
      <Callout
        tone="warn"
        icon={<TriangleAlert size={18} aria-hidden />}
        label="Can’t read"
        title="We couldn’t load your operators"
        detail="The API didn’t answer. Nothing has been paused — your operators keep running while this page is blind."
        action={
          <Button variant="outline" size="sm">
            Try again
          </Button>
        }
      />
      <Panel title="Operators">
        <EmptyState
          icon={<Radio size={18} aria-hidden />}
          title="Nothing to show while we’re disconnected"
          detail="This list is empty because the request failed — not because you have no operators."
        />
      </Panel>
    </>
  );
}

// ---------------------------------------------------------------------------

const VIEWS = [
  { value: 'operations', label: 'Operations' },
  { value: 'operations-empty', label: 'Operations · empty' },
  { value: 'prototypes', label: 'Prototypes' },
  { value: 'prototypes-empty', label: 'Prototypes · empty' },
  { value: 'error', label: 'Error' },
];

export default function JobSurfacesPrototype() {
  const [view, setView] = useState('operations');

  return (
    <main className="mx-auto max-w-5xl px-4 py-6">
      <VStack gap={5} align="stretch">
        <VStack gap={1} align="stretch">
          <HStack gap={2} align="center">
            <Badge label="PROTOTYPE" variant="neutral" />
            <Heading level={2} className="!text-sm">
              bs-h0nibs69 — recurring job surfaces
            </Heading>
          </HStack>
          <Text type="supporting" color="secondary">
            /start onboards once. These two screens are the same three jobs done again, and again: one row per
            standing operator, one card per idea under test.
          </Text>
          <div className="pt-2">
            <Segment options={VIEWS} value={view} onChange={setView} />
          </div>
        </VStack>

        <div>
          {view === 'operations' ? <OperationsScreen /> : null}
          {view === 'operations-empty' ? <OperationsScreen empty /> : null}
          {view === 'prototypes' ? <PrototypesScreen /> : null}
          {view === 'prototypes-empty' ? <PrototypesScreen empty /> : null}
          {view === 'error' ? <ErrorScreen /> : null}
        </div>
      </VStack>
    </main>
  );
}
