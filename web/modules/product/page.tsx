'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { Activity, Filter, LineChart, Sparkles, Table2 } from 'lucide-react';
import { useRouter } from 'next/navigation';
import type { ReactNode } from 'react';
import type { InsightResult } from '@/lib/api';
import { formatFractionAsPercent } from '@/lib/format';
import { Card } from '@astryxdesign/core/Card';
import { HStack } from '@astryxdesign/core/HStack';
import { VStack } from '@astryxdesign/core/VStack';
import { Heading } from '@astryxdesign/core/Heading';
import { Text } from '@astryxdesign/core/Text';
import { SelectableCard } from '@astryxdesign/core/SelectableCard';
import { Chart } from '@/modules/shared/components/charts';
import { funnelStepNames, retentionAnchorEvent } from '@/lib/ia';
import { useAuthStore, useFiltersStore } from '@/lib/app-state';
import { useActivity, useEventNames, useFunnelByPlatform, useInsight } from '@/modules/app/hooks';
import { platformLabel } from '@/lib/platform';
import { AppShell } from '@/modules/shared/components/app-shell';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { FilterBar } from '@/modules/shared/components/filter-bar';
import { Button, EmptyState, Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { headlineStats } from './headline';
import { AutoGrid } from '@/modules/shared/components/page-shell';

type Mode = 'trend' | 'funnel' | 'retention' | 'table';

// Each question is the primary affordance on this page — the mode is derived
// from it, not chosen separately. icon/blurb make the chip read like a question
// you'd actually ask, not a tab.
const QUESTIONS: Array<{ mode: Mode; label: string; blurb: string; icon: ReactNode }> = [
  { mode: 'trend', label: 'How is activity trending?', blurb: 'Event volume over time', icon: <LineChart size={15} /> },
  { mode: 'funnel', label: 'Where do new users drop off?', blurb: 'Step-by-step conversion', icon: <Filter size={15} /> },
  { mode: 'retention', label: 'How well do users retain?', blurb: 'Return rate by period', icon: <Activity size={15} /> },
  { mode: 'table', label: 'What are the top events?', blurb: 'Ranked raw breakdown', icon: <Table2 size={15} /> },
];

export function ProductPage() {
  const router = useRouter();
  const { insight, runInsight } = useInsight();
  const { names: eventNames, loading: namesLoading } = useEventNames();
  const { summary } = useActivity();
  const activationEvent = useAuthStore((s) => s.project?.activation_event);
  const emptyCatalog = !namesLoading && eventNames.length === 0;
  const [active, setActive] = useState<Mode | null>(null);
  const [running, setRunning] = useState(false);
  const didAuto = useRef(false);

  // The same steps the funnel question runs, split per app. A product with one
  // platform gets nothing extra; one with a site and a native app gets the two
  // curves the blended funnel was averaging.
  // Under a platform filter there is only one app to ask about, so the split
  // drops to a single series and PlatformFunnels' own two-or-more guard hides it
  // — the funnel above is already that app's. Without this the comparison table
  // would keep listing every platform, contradicting the filter above it.
  const applied = useFiltersStore((s) => s.appliedFilters);
  const platforms = useMemo(
    () => (applied.platform ? [applied.platform] : summary?.platforms ?? []),
    [applied.platform, summary?.platforms],
  );
  const funnelSteps = useMemo(() => (emptyCatalog ? [] : funnelStepNames(eventNames, activationEvent)), [emptyCatalog, eventNames, activationEvent]);
  const { splits: platformFunnels, loading: splitsLoading } = useFunnelByPlatform(
    active === 'funnel' ? funnelSteps : [],
    platforms,
  );

  async function ask(mode: Mode) {
    setActive(mode);
    setRunning(true);
    // Retention needs an event *name* to anchor the cohort on, and `steps` is how
    // the API takes one. It used to send none, so the server fell back to the
    // metric string and asked about an event called "events" — nothing emits
    // that, so the curve was 0% forever under a hard-coded "Week 0: 100%".
    const steps =
      mode === 'funnel' ? funnelStepNames(eventNames, activationEvent)
      : mode === 'retention' ? [retentionAnchorEvent(eventNames, activationEvent)]
      : [];
    try {
      await runInsight(mode, 'events', steps);
    } finally {
      setRunning(false);
    }
  }

  useEffect(() => {
    if (didAuto.current || namesLoading || emptyCatalog) return;
    didAuto.current = true;
    void ask('funnel');
    // Catalog-ready auto-run of the drop-off question. ask closes over eventNames.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [namesLoading, emptyCatalog, eventNames]);

  // The insight endpoint has always taken the applied filters — nothing on this
  // page ever asked it again, so changing the range or the platform left the
  // previous answer on screen looking like the new one. Re-run the question that
  // is already open; skip the first pass, which the auto-run above owns.
  const lastFilters = useRef<string>('');
  useEffect(() => {
    const key = JSON.stringify(applied);
    const first = lastFilters.current === '';
    lastFilters.current = key;
    if (first || !active) return;
    void ask(active);
    // ask closes over eventNames, which the catalog effect already tracks.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [applied]);

  return (
    <AppShell
      title="Product"
      sub="Answer behavior questions without writing SQL first."
      actions={<Button variant="agent" icon={<Sparkles size={15} />} onClick={() => router.push('/chat')}>Ask Growth Lead</Button>}
    >
      <FilterBar showEventType={false} showErrors={false} />

      {/* Astryx migration: the question picker is now an Astryx <Card> wrapping a
          responsive <Grid> of <SelectableCard>s — native controlled selection
          (accent inset border on the active question), keyboard + a11y, and the
          running state mapped to isDisabled. The icon keeps its agent-purple tint. */}
      <Card padding={4}>
        <Text type="supporting" className="mb-3 block font-medium uppercase tracking-[0.08em]">Ask about your product</Text>
        <AutoGrid min={280} max={2} gap={3}>
          {QUESTIONS.map((q) => (
            <SelectableCard
              key={q.mode}
              label={q.label}
              isSelected={active === q.mode}
              isDisabled={running}
              onChange={() => void ask(q.mode)}
              variant="muted"
              padding={3}
            >
              <HStack align="center" gap={3}>
                <span className="grid h-8 w-8 flex-none place-items-center rounded-sm bg-[color-mix(in_srgb,var(--agent)_16%,transparent)] text-agent">{q.icon}</span>
                <VStack gap={0.5} className="min-w-0">
                  <Text type="body" weight="semibold">{q.label}</Text>
                  <Text type="supporting">{q.blurb}</Text>
                </VStack>
              </HStack>
            </SelectableCard>
          ))}
        </AutoGrid>
      </Card>

      {running ? (
        <Loading label="Running insight…" />
      ) : insight && active ? (
        <>
          <ResultView insight={insight} />
          {active === 'funnel' ? (
            <PlatformFunnels splits={platformFunnels} loading={splitsLoading} />
          ) : null}
        </>
      ) : (
        <EmptyState
          icon={<Sparkles size={22} style={{ color: 'var(--agent)' }} />}
          title="Pick a question to begin"
          detail={emptyCatalog
            ? 'No product yet? Prove the idea first → Prototypes. Market a landing page, paste the snippet, collect the waitlist — then this page has something to chart.'
            : 'Each question runs against this project\'s events and returns a chart plus the underlying numbers — no SQL required.'}
          action={emptyCatalog ? <Button variant="outline" size="sm" onClick={() => router.push('/prototypes')}>Prototypes</Button> : undefined}
        />
      )}
    </AppShell>
  );
}

// PlatformFunnels puts each app's funnel beside the others. The blended funnel
// above it is the average of these curves and describes none of them — a site
// that converts at 25% and an app that converts at 50% report "one third" and
// send you to fix the wrong one.
//
// Rows are the steps of the funnel that ran; each platform contributes a people
// count and its conversion from the first step. A platform whose insight failed
// is absent rather than shown as zero.
function PlatformFunnels({
  splits,
  loading,
}: {
  splits: Array<{ platform: string; funnel: FunnelStep[] }>;
  loading: boolean;
}) {
  const usable = splits.filter((s) => s.funnel.length > 0);
  const columns = useMemo<DataColumn<PlatformFunnelRow>[]>(() => {
    if (usable.length === 0) return [];
    return [
      { key: 'event_name', header: 'Step', renderCell: (r) => <span className="font-mono">{r.event_name}</span> },
      ...usable.map((split) => ({
        key: split.platform,
        header: platformLabel(split.platform),
        renderCell: (r: PlatformFunnelRow) => {
          const cell = r.byPlatform[split.platform];
          if (!cell) return <Text type="supporting">—</Text>;
          return (
            <span className="font-mono tabular-nums">
              {cell.users}
              <span className="ms-2 text-[var(--color-text-secondary)]">{formatFractionAsPercent(cell.conversion)}</span>
            </span>
          );
        },
      })),
    ];
  }, [usable]);

  if (loading) return <Loading label="Comparing platforms…" />;
  if (usable.length < 2) return null;

  // Steps come from the longest funnel returned, so a platform missing a step
  // shows an em dash there instead of shortening the table for everyone.
  const longest = usable.reduce((best, s) => (s.funnel.length > best.funnel.length ? s : best), usable[0]);
  const rows: PlatformFunnelRow[] = longest.funnel.map((step, index) => ({
    step: step.step,
    event_name: step.event_name,
    byPlatform: Object.fromEntries(
      usable
        .map((s) => [s.platform, s.funnel[index]] as const)
        .filter(([, cell]) => !!cell && cell.event_name === step.event_name),
    ),
  }));

  return <DataTable title="Same funnel, per platform" columns={columns} data={rows} idKey="step" pageSize={10} />;
}

type PlatformFunnelRow = {
  step: number;
  event_name: string;
  byPlatform: Record<string, FunnelStep | undefined>;
};

// ResultView is the single composed result block: a headline stat strip, then a
// chart, then the supporting table — the same rhythm for every insight type.
function ResultView({ insight }: { insight: InsightResult }) {
  const router = useRouter();
  const ask = (
    <Button variant="agent" size="sm" icon={<Sparkles size={15} />} onClick={() => router.push('/chat')}>
      Ask the agent about this
    </Button>
  );

  return (
    <>
      <HStack align="end" justify="between" gap={3}>
        <VStack gap={0.5}>
          <Text type="supporting" className="font-medium uppercase tracking-[0.08em]">Result</Text>
          <Heading level={3} className="tracking-[-0.01em]">{insight.title || 'Insight'}</Heading>
        </VStack>
        {ask}
      </HStack>
      <Headline insight={insight} />
      <ResultBody insight={insight} />
    </>
  );
}

function Headline({ insight }: { insight: InsightResult }) {
  const stats = useMemo(() => headlineStats(insight), [insight]);
  if (!stats.length) return null;
  return <StatsStrip stats={stats} />;
}

function ResultBody({ insight }: { insight: InsightResult }) {
  if (insight.series?.length) {
    return (
      <Panel title="Activity over time">
        <Chart spec={{
          type: 'area',
          x: insight.series.map((p) => p.hour),
          series: [{ name: 'Events', data: insight.series.map((p) => p.count) }],
          height: 240,
        }} />
      </Panel>
    );
  }

  if (insight.funnel?.length) {
    return (
      <>
        <Panel title="Conversion by step">
          <Chart spec={{
            type: 'bar',
            x: insight.funnel.map((f) => f.event_name),
            series: [{ name: 'Users', data: insight.funnel.map((f) => f.users) }],
            height: 240,
          }} />
        </Panel>
        <FunnelTable funnel={insight.funnel} />
      </>
    );
  }

  if (insight.retention?.length) {
    // Week 0 is the cohort itself — 100% by definition. It belongs on the curve
    // as the anchor, but only when there is at least one *measured* period after
    // it; on its own it is a one-point chart of a tautology.
    const measured = insight.retention.filter((r, i) => i > 0 && r.mature !== false);
    const measuredRetention = measured.length > 0 ? [insight.retention[0], ...measured] : [];
    return (
      <>
        <Panel title="Retention curve">
          {/* Charting an immature period draws a cliff to zero that is an artefact
              of the window, not of the product. Plot only what has elapsed. */}
          {measuredRetention.length > 0 ? (
            <Chart spec={{
              type: 'line',
              x: measuredRetention.map((r) => r.period),
              series: [{ name: 'Retention %', data: measuredRetention.map((r) => Math.round(r.rate * 100)) }],
              unit: '%',
              height: 240,
            }} />
          ) : (
            <Text type="supporting">
              No period has finished elapsing yet, so there is nothing to plot. The cohort is{' '}
              {insight.retention[0]?.users ?? 0} people — come back after a week, or widen the range.
            </Text>
          )}
        </Panel>
        <RetentionTable retention={insight.retention} />
      </>
    );
  }

  if (insight.rows?.length) return <RowsTable rows={insight.rows} />;

  return (
    <Panel title={insight.title || 'Insight'}>
      <Text type="supporting">No data returned for this insight.</Text>
    </Panel>
  );
}

type FunnelStep = InsightResult['funnel'][number];
type RetentionPeriod = InsightResult['retention'][number];
type Row = Record<string, unknown>;

// FunnelTable renders the per-step funnel breakdown with sortable columns.
function FunnelTable({ funnel }: { funnel: FunnelStep[] }) {
  const columns = useMemo<DataColumn<FunnelStep>[]>(() => [
    { key: 'step', header: 'Step', sortValue: (f) => f.step, renderCell: (f) => <span className="font-mono tabular-nums">{f.step}</span> },
    { key: 'event_name', header: 'Event', renderCell: (f) => <span className="font-mono">{f.event_name}</span> },
    { key: 'users', header: 'Users', renderCell: (f) => <span className="font-mono tabular-nums">{f.users}</span> },
    { key: 'conversion', header: 'Conv.', renderCell: (f) => <span className="font-mono tabular-nums">{formatFractionAsPercent(f.conversion)}</span> },
  ], []);
  return <DataTable title="Funnel steps" columns={columns} data={funnel} idKey="step" pageSize={10} />;
}

// RetentionTable renders the retention curve breakdown.
function RetentionTable({ retention }: { retention: RetentionPeriod[] }) {
  const columns = useMemo<DataColumn<RetentionPeriod>[]>(() => [
    { key: 'period', header: 'Period' },
    { key: 'users', header: 'Users', renderCell: (r) => <span className="font-mono tabular-nums">{r.users}</span> },
    // An immature period has no rate to show. Printing "0%" there is the defect:
    // it reads as total churn when it means the week has not happened yet.
    { key: 'rate', header: 'Rate', renderCell: (r) => (
      r.mature === false
        ? <Text type="supporting">too early</Text>
        : <span className="font-mono tabular-nums">{(r.rate * 100).toFixed(0)}%</span>
    ) },
  ], []);
  return <DataTable title="Retention by period" columns={columns} data={retention} idKey="period" pageSize={10} />;
}

// RowsTable renders an arbitrary SQL-style result with columns inferred from the
// first row; numeric values render in mono, left-aligned so each header label
// sits directly above its data (the sort caret lives on the right of the label).
function RowsTable({ rows }: { rows: Row[] }) {
  const keys = Object.keys(rows[0] ?? {});
  const keySig = keys.join('|');
  const columns = useMemo<DataColumn<Row>[]>(() => keySig.split('|').filter(Boolean).map((k) => ({
    key: k,
    header: k,
    renderCell: (row) => {
      const v = row[k];
      return typeof v === 'number'
        ? <span className="font-mono tabular-nums">{v.toLocaleString()}</span>
        : <span>{String(v ?? '')}</span>;
    },
  })), [keySig]);
  return <DataTable title="Result" columns={columns} data={rows} searchPlaceholder="Search rows…" pageSize={20} />;
}
