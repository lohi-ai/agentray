'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { Activity, Filter, LineChart, Sparkles, Table2 } from 'lucide-react';
import { useRouter } from 'next/navigation';
import type { ReactNode } from 'react';
import type { InsightResult } from '@/lib/api';
import { formatCompact, formatPercent } from '@/lib/format';
import { Card } from '@astryxdesign/core/Card';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack } from '@astryxdesign/core/HStack';
import { VStack } from '@astryxdesign/core/VStack';
import { Heading } from '@astryxdesign/core/Heading';
import { Text } from '@astryxdesign/core/Text';
import { SelectableCard } from '@astryxdesign/core/SelectableCard';
import { Chart } from '@/modules/shared/components/charts';
import { funnelStepNames } from '@/lib/ia';
import { useEventNames, useInsight } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { RelatedSurfacesLabel } from '@/modules/shared/components/related-surfaces';
import { Button, EmptyState, Intro, Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';

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
  const emptyCatalog = !namesLoading && eventNames.length === 0;
  const [active, setActive] = useState<Mode | null>(null);
  const [running, setRunning] = useState(false);
  const didAuto = useRef(false);

  async function ask(mode: Mode) {
    setActive(mode);
    setRunning(true);
    const steps = mode === 'funnel' ? funnelStepNames(eventNames) : [];
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

  return (
    <AppShell active="product">
      <Intro title="Product" sub="Answer behavior questions without writing SQL first." action={<Button variant="agent" icon={<Sparkles size={15} />} onClick={() => router.push('/chat')}>Ask Growth Lead</Button>} />
      <div className="mb-3"><RelatedSurfacesLabel parentHref="/product" /></div>

      {/* Astryx migration: the question picker is now an Astryx <Card> wrapping a
          responsive <Grid> of <SelectableCard>s — native controlled selection
          (accent inset border on the active question), keyboard + a11y, and the
          running state mapped to isDisabled. The icon keeps its agent-purple tint. */}
      <Card padding={4} className="mb-4">
        <Text type="supporting" className="mb-3 block font-medium uppercase tracking-[0.08em]">Ask about your product</Text>
        <Grid columns={{ minWidth: 280, max: 2 }} gap={3}>
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
        </Grid>
      </Card>

      {running ? (
        <Loading label="Running insight…" />
      ) : insight && active ? (
        <ResultView insight={insight} />
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
      <HStack align="end" justify="between" gap={3} className="mb-[14px]">
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

function headlineStats(insight: InsightResult): Parameters<typeof StatsStrip>[0]['stats'] {
  if (insight.series?.length) {
    const counts = insight.series.map((p) => p.count);
    const total = counts.reduce((s, c) => s + c, 0);
    const peak = Math.max(...counts, 0);
    const first = counts[0] ?? 0;
    const last = counts[counts.length - 1] ?? 0;
    const delta = first > 0 ? ((last - first) / first) * 100 : 0;
    return [
      { label: 'Total events', value: formatCompact(total) },
      { label: 'Peak / bucket', value: formatCompact(peak) },
      { label: 'Latest', value: formatCompact(last), delta: `${Math.abs(delta).toFixed(0)}%`, deltaTone: delta >= 0 ? 'up' : 'down' },
    ];
  }
  if (insight.funnel?.length) {
    const first = insight.funnel[0];
    const last = insight.funnel[insight.funnel.length - 1];
    const conv = last?.conversion ?? 0;
    return [
      { label: 'Entered', value: formatCompact(first?.users ?? 0) },
      { label: 'Completed', value: formatCompact(last?.users ?? 0) },
      { label: 'Conversion', value: formatPercent(conv, 0), tone: conv >= 50 ? 'success' : conv >= 20 ? 'warning' : 'danger' },
    ];
  }
  if (insight.retention?.length) {
    const rates = insight.retention.map((r) => r.rate);
    const avg = rates.reduce((s, r) => s + r, 0) / rates.length;
    const best = Math.max(...rates, 0);
    return [
      { label: 'Periods', value: String(insight.retention.length) },
      { label: 'Avg retention', value: formatPercent(avg * 100, 0) },
      { label: 'Best period', value: formatPercent(best * 100, 0), tone: 'success' },
    ];
  }
  if (insight.rows?.length) {
    return [
      { label: 'Rows', value: formatCompact(insight.rows.length) },
      { label: 'Columns', value: String(Object.keys(insight.rows[0] ?? {}).length) },
    ];
  }
  return [];
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
    return (
      <>
        <Panel title="Retention curve">
          <Chart spec={{
            type: 'line',
            x: insight.retention.map((r) => r.period),
            series: [{ name: 'Retention %', data: insight.retention.map((r) => Math.round(r.rate * 100)) }],
            unit: '%',
            height: 240,
          }} />
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
    { key: 'step', header: 'Step', sortValue: (f) => f.step, renderCell: (f) => <span className="font-mono tabular-nums">{f.step + 1}</span> },
    { key: 'event_name', header: 'Event', renderCell: (f) => <span className="font-mono">{f.event_name}</span> },
    { key: 'users', header: 'Users', renderCell: (f) => <span className="font-mono tabular-nums">{f.users}</span> },
    { key: 'conversion', header: 'Conv.', renderCell: (f) => <span className="font-mono tabular-nums">{f.conversion.toFixed(0)}%</span> },
  ], []);
  return <DataTable title="Funnel steps" columns={columns} data={funnel} idKey="step" pageSize={10} />;
}

// RetentionTable renders the retention curve breakdown.
function RetentionTable({ retention }: { retention: RetentionPeriod[] }) {
  const columns = useMemo<DataColumn<RetentionPeriod>[]>(() => [
    { key: 'period', header: 'Period' },
    { key: 'users', header: 'Users', renderCell: (r) => <span className="font-mono tabular-nums">{r.users}</span> },
    { key: 'rate', header: 'Rate', renderCell: (r) => <span className="font-mono tabular-nums">{(r.rate * 100).toFixed(0)}%</span> },
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
