'use client';

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertTriangle, ArrowUpRight, Clock, RefreshCw } from 'lucide-react';
import { AgentRayAPI, apiBase, type OverviewMetric, type OverviewResult } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { formatCompact } from '@/lib/format';
import { platformLabel } from '@/lib/platform';
import { firstValuePath, settingsPath } from '@/lib/ia';
import { useActivity, useEventNames } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { PageShell } from '@/modules/shared/components/page-shell';
import { Chart } from '@/modules/shared/components/charts';
import { BarRows, Button, Callout, EmptyState, Loading, Panel, Segment, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { Selector } from '@astryxdesign/core/Selector';
import { FirstEventQuickstart } from '@/modules/dashboard/first-event-quickstart';

const PERIODS = [
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: '90d', label: '90 days' },
];

// metricTile renders one headline number honestly: a metric that is not "ok"
// shows its state, never a fabricated zero. "unconfigured" is an action —
// the metric is defined but the project has not told us what to measure.
function metricTile(label: string, m: OverviewMetric): { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } {
  if (m.state !== 'ok' || m.value === undefined) {
    const stateLabel =
      m.state === 'no_data' ? 'No data yet'
      : m.state === 'not_ready' ? 'Not enough data'
      : m.state === 'unconfigured' ? 'Not configured'
      : 'Unavailable';
    return { label, value: '—', delta: stateLabel };
  }
  const tile: { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } = {
    label,
    value: formatCompact(m.value),
  };
  if (m.previous !== undefined && m.previous > 0) {
    const pct = ((m.value - m.previous) / m.previous) * 100;
    tile.delta = `${pct >= 0 ? '+' : ''}${pct.toFixed(0)}% vs prior`;
    tile.deltaTone = pct >= 0 ? 'up' : 'down';
  }
  return tile;
}

function retentionLine(label: string, p: { state: string; rate: number; returned: number; eligible: number }): string {
  if (p.state !== 'ok') return `${label}: not enough mature cohorts yet`;
  return `${label}: ${(p.rate * 100).toFixed(0)}% — ${formatCompact(p.returned)} of ${formatCompact(p.eligible)} people returned`;
}

// trendMeaning distinguishes "the chart is flat because nothing qualified"
// from "the chart is flat because nothing arrived" — a padded zero series
// must never read as a populated trend.
function trendMeaning(res: OverviewResult): 'data' | 'receipt_only' | 'empty' {
  if (res.data_status.qualifying_in_range > 0) return 'data';
  if (res.data_status.events_in_range > 0 || res.data_status.ever_received) return 'receipt_only';
  return 'empty';
}

// freshnessLabel ages from the absolute last_event_at at render time, not the
// cached age_seconds — a cached snapshot would say "Live" forever on an open
// page. `now` is injectable so staleness is testable.
export function freshnessLabel(res: OverviewResult, now = Date.now()): { text: string; stale: boolean } {
  const ds = res.data_status;
  if (ds.state === 'no_events' || !ds.last_event_at) return { text: 'No events received yet', stale: true };
  const age = Math.max(0, (now - new Date(ds.last_event_at).getTime()) / 1000);
  const text =
    age < 120 ? 'Live — last event just now'
    : age < 3600 ? `Last event ${Math.round(age / 60)} min ago`
    : age < 86400 ? `Last event ${Math.round(age / 3600)} h ago`
    : `Last event ${Math.round(age / 86400)} d ago`;
  return { text, stale: age >= 86400 };
}

export function OverviewPage() {
  const projectID = useAuthStore((s) => s.project?.id);
  const [period, setPeriod] = useState('7d');
  const [platform, setPlatform] = useState('');

  const { summary } = useActivity();
  const { names: eventNames, loading: catalogLoading } = useEventNames();
  const catalogReady = !catalogLoading && !!projectID;
  const firstValue = firstValuePath({ eventNames, catalogReady });

  const query = useQuery({
    queryKey: ['overview', projectID, period, platform],
    queryFn: () => new AgentRayAPI(projectID!).overview(period, platform),
    enabled: !!projectID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
  const res = query.data ?? null;

  // The platform facet is data-driven like FilterBar's: a one-platform product
  // does not get a control that can only ever say "web".
  const platforms = summary?.platforms ?? [];
  const showPlatform = platforms.length > 1 || !!platform;

  const freshness = res ? freshnessLabel(res) : null;
  const stats = res
    ? [
        metricTile('Active people', res.metrics.active_users),
        metricTile('New people', res.metrics.new_users),
        metricTile('Sessions', res.metrics.sessions),
        metricTile('Activation', res.metrics.activation),
        metricTile('Revenue', res.metrics.revenue),
      ]
    : [];

  const trendSpec = useMemo(() => {
    if (!res || res.trend.length === 0 || trendMeaning(res) !== 'data') return null;
    return {
      type: 'area' as const,
      x: res.trend.map((p) => p.day),
      series: [{ name: 'Active people', data: res.trend.map((p) => p.active_users) }],
      smooth: false,
      integerY: true,
      height: 220,
    };
  }, [res]);
  const trend = res ? trendMeaning(res) : 'empty';

  const rangeLabel = res
    ? `${res.context.range.from.slice(0, 10)} → ${res.context.range.to.slice(0, 10)} · ${res.context.range.days} complete days · ${res.context.timezone}${res.context.timezone_source === 'fallback' ? ' (UTC fallback — no project timezone set)' : ''}`
    : '';

  return (
    <AppShell>
      <PageShell
        title="Overview"
        sub={rangeLabel || 'The last complete days, at a glance.'}
        actions={
          <div className="flex items-center gap-2">
            {showPlatform ? (
              <Selector
                size="sm"
                label="Platform"
                value={platform || 'all'}
                onChange={(v) => setPlatform(v === 'all' ? '' : String(v))}
                options={[
                  { value: 'all', label: 'All platforms' },
                  ...platforms.map((p) => ({ value: p, label: platformLabel(p) })),
                ]}
              />
            ) : null}
            <Segment options={PERIODS} value={period} onChange={setPeriod} />
          </div>
        }
      >
        {query.isLoading ? <Loading label="Loading overview…" /> : null}

        {query.isError ? (
          <Callout
            tone="warn"
            icon={<AlertTriangle size={16} />}
            label="Overview unavailable"
            title="Could not load the overview"
            detail={query.error instanceof Error ? query.error.message : 'The overview request failed.'}
            action={<Button variant="outline" size="sm" icon={<RefreshCw size={14} />} onClick={() => void query.refetch()}>Retry</Button>}
          />
        ) : null}

        {firstValue.showFirstEvent ? <FirstEventQuickstart /> : null}

        {res && !firstValue.showFirstEvent ? (
          <>
            {freshness?.stale ? (
              <Callout
                tone="warn"
                icon={<Clock size={16} />}
                label="Data freshness"
                title={freshness.text}
                detail="Numbers below cover the selected range but the source has gone quiet — check that events are still being sent."
              />
            ) : null}

            <StatsStrip stats={stats} />

            {/* Trust metadata: every metric's definition and notes stay one
                disclosure away — the numbers are only as honest as what they
                exclude. */}
            <details className="text-xs text-[var(--color-text-secondary)]">
              <summary className="cursor-pointer select-none">How these numbers are computed</summary>
              <dl className="mt-2 flex flex-col gap-2">
                {([
                  ['Active people', res.metrics.active_users],
                  ['New people', res.metrics.new_users],
                  ['Sessions', res.metrics.sessions],
                  ['Activation', res.metrics.activation],
                  ['Revenue', res.metrics.revenue],
                ] as Array<[string, OverviewMetric]>).map(([label, m]) => (
                  <div key={label}>
                    <dt className="font-medium text-[var(--color-text-primary)]">{label}</dt>
                    <dd>{m.definition}</dd>
                    {m.notes?.map((n) => <dd key={n} className="text-[var(--color-text-disabled)]">· {n}</dd>)}
                  </div>
                ))}
              </dl>
            </details>

            <div className="grid grid-cols-3 gap-4 [@media(max-width:980px)]:grid-cols-1">
              <div className="col-span-2 [@media(max-width:980px)]:col-span-1">
                <Panel title="Active people per day">
                  {trendSpec ? (
                    <>
                      <Chart spec={trendSpec} />
                      {/* Textual equivalent: the chart is the shape, this is the data. */}
                      <p className="mt-2 text-xs text-[var(--color-text-secondary)]">
                        {res.trend.map((p) => `${p.day.slice(5)}: ${p.active_users}`).join(' · ')}
                      </p>
                    </>
                  ) : trend === 'receipt_only' ? (
                    <EmptyState
                      title="Connected — no qualifying activity yet"
                      detail="Events are arriving, but none count as human product activity in this range (verification pings, bots, and agent events are excluded). The trend draws once real usage lands."
                    />
                  ) : (
                    <EmptyState title="No activity in this range" detail="Qualifying human events will draw the trend once they arrive." />
                  )}
                </Panel>
              </div>
              <Panel title="Retention">
                <div className="flex flex-col gap-2 text-sm">
                  <p>{retentionLine('Day 1', res.retention.d1)}</p>
                  <p>{retentionLine('Day 7', res.retention.d7)}</p>
                  <p>{retentionLine('Day 30', res.retention.d30)}</p>
                  <p className="text-xs text-[var(--color-text-secondary)]">
                    {res.retention.cohort_window === 'lifetime' ? 'Lifetime cohorts — a person counts from their first-ever event, not the selected range.' : `Cohort window: ${res.retention.cohort_window}`}
                  </p>
                </div>
              </Panel>
            </div>

            <div className="grid grid-cols-2 gap-4 [@media(max-width:980px)]:grid-cols-1">
              <Panel title="Top pages">
                <BarRows
                  rows={res.content.top_pages.rows}
                  valueHead="Page"
                  countHead={res.content.top_pages.unit}
                  mono
                  empty="No pageviews in this range"
                />
              </Panel>
              <Panel title="Top sources">
                <BarRows
                  rows={res.content.top_sources.rows}
                  valueHead="Source"
                  countHead={res.content.top_sources.unit}
                  empty="No attributed sources in this range"
                />
              </Panel>
            </div>

            <Panel title="Data status" action={freshness ? <span className="text-xs text-[var(--color-text-secondary)]">{freshness.text}</span> : null}>
              <div className="flex flex-wrap gap-x-8 gap-y-2 text-sm">
                <span>{formatCompact(res.data_status.events_in_range)} events in range</span>
                <span>{formatCompact(res.data_status.qualifying_in_range)} qualifying (human product activity)</span>
                <span className="text-[var(--color-text-secondary)]">
                  Pipeline lag: {res.data_status.pipeline_lag === 'unavailable' ? 'not measured yet' : res.data_status.pipeline_lag}
                </span>
              </div>
            </Panel>

            <Panel title="Next step">
              <div className="flex flex-wrap items-center gap-3 text-sm">
                <span className="text-[var(--color-text-secondary)]">Dig into what changed, or point your coding agent at this project over MCP.</span>
                <Button variant="outline" size="sm" icon={<ArrowUpRight size={14} />} onClick={() => { window.location.href = settingsPath('ai'); }}>Connect your agent (MCP)</Button>
                <Button variant="outline" size="sm" onClick={() => { window.location.href = '/chat'; }}>Ask in chat</Button>
                <Button variant="ghost" size="sm" onClick={() => { window.location.href = '/events'; }}>Browse events</Button>
              </div>
            </Panel>
          </>
        ) : null}
      </PageShell>
    </AppShell>
  );
}
