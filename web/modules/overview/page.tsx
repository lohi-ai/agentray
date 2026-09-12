'use client';

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertTriangle, ArrowUpRight, Clock, Lock, RefreshCw } from 'lucide-react';
import { AgentRayAPI, APIError, type OverviewMetric, type OverviewResult } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { formatCompact } from '@/lib/format';
import { platformLabel } from '@/lib/platform';
import { firstValuePath, settingsPath } from '@/lib/ia';
import { useEventNames } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { PageShell } from '@/modules/shared/components/page-shell';
import { Chart } from '@/modules/shared/components/charts';
import { BarRows, Button, Callout, EmptyState, Loading, Panel, Segment, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { Selector } from '@astryxdesign/core/Selector';
import { FirstEventQuickstart } from '@/modules/dashboard/first-event-quickstart';

// The range control always offers Today plus the complete-day windows. Today
// is the explicit partial period: the backend returns no comparison for it
// and the header says so rather than implying a full day.
const PERIODS = [
  { value: 'today', label: 'Today' },
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: '90d', label: '90 days' },
];

// The platform control is always visible with the full classifier vocabulary —
// a one-platform product still gets the control, so the filter is discoverable
// before a second platform exists and "Unknown" stays reachable.
const PLATFORM_OPTIONS = ['web', 'ios', 'android', 'server', 'unknown'].map((value) => ({
  value,
  label: platformLabel(value),
}));

// metricTile renders one headline number honestly: a metric that is not "ok"
// shows its state, never a fabricated zero. "unconfigured" is an action —
// the metric is defined but the project has not told us what to measure.
function metricTile(label: string, m: OverviewMetric): { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } {
  if (m.state !== 'ok' || m.value === undefined) {
    const stateLabel =
      m.state === 'unconfigured' ? 'Set up'
      : m.state === 'not_ready' ? 'Not ready'
      : m.state === 'no_data' ? 'No data'
      : 'Unavailable';
    return { label, value: stateLabel };
  }
  const tile: { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } = {
    label,
    value: formatCompact(m.value),
  };
  if (m.previous !== undefined && m.previous > 0) {
    const pct = ((m.value - m.previous) / m.previous) * 100;
    tile.delta = `${pct >= 0 ? '+' : ''}${pct.toFixed(0)}%`;
    tile.deltaTone = pct >= 0 ? 'up' : 'down';
  }
  return tile;
}

function retentionLine(label: string, p: { state: string; rate: number; returned: number; eligible: number }): string {
  if (p.state === 'not_ready' || p.eligible === 0) return `${label}: Not ready — not enough mature cohorts yet`;
  if (p.state !== 'ok') return `${label}: ${p.state === 'unconfigured' ? 'Set up' : 'Unavailable'}`;
  return `${label}: ${(p.rate * 100).toFixed(1)}% (${formatCompact(p.returned)} of ${formatCompact(p.eligible)} returned)`;
}

// trendMeaning distinguishes "the chart is flat because nothing qualified"
// from "the chart is flat because nothing arrived" — a padded zero series
// must never read as a populated trend.
function trendMeaning(res: OverviewResult): 'data' | 'receipt_only' | 'empty' {
  if (res.data_status.qualifying_in_range > 0) return 'data';
  if (res.data_status.events_in_range > 0) return 'receipt_only';
  return 'empty';
}

// freshnessLabel ages capture receipt time, not client occurrence time: an
// offline event that arrives late proves the source is currently reachable.
// `now` is injectable so staleness is testable.
export function freshnessLabel(res: OverviewResult, now = Date.now()): { text: string; stale: boolean } {
  const received = res.data_status.last_received_at;
  if (!received) return { text: 'No capture receipts yet', stale: true };
  const at = new Date(received);
  const text = `Last received ${at.toISOString().slice(0, 16).replace('T', ' ')} UTC`;
  return { text, stale: now - at.getTime() > 24 * 60 * 60 * 1000 };
}

// One mutually exclusive view state per the redesign state contract
// (docs/redesign/design.md): loading keeps the layout, a 403 names the missing
// access, other failures offer retry, a project that has never received (or
// whose catalog is verification-only) is first-run, and the data states split
// "nothing arrived" from "arrived but nothing qualified" from "the active
// filter excluded everything". `stale` is a modifier on the data states, not
// a state of its own — last-known figures stay on screen with a timestamp.
export type OverviewViewState =
  | 'loading'
  | 'no_access'
  | 'error'
  | 'first_run'
  | 'filtered_empty'
  | 'receipt_only'
  | 'empty'
  | 'data';

export function overviewViewState(input: {
  projectID: string | undefined;
  isLoading: boolean;
  error: unknown;
  res: OverviewResult | null;
  showFirstEvent: boolean;
  platform: string;
  period: string;
}): OverviewViewState {
  const { projectID, isLoading, error, res, showFirstEvent, platform, period } = input;
  if (!projectID || (isLoading && !res)) return 'loading';
  if (error && !res) {
    return error instanceof APIError && error.status === 403 ? 'no_access' : 'error';
  }
  if (!res) return 'loading';
  if (res.data_status.qualifying_in_range > 0) return 'data';
  if (!res.data_status.ever_received || showFirstEvent) return 'first_run';
  if (res.data_status.events_in_range > 0) return 'receipt_only';
  if (platform || period !== '7d') return 'filtered_empty';
  return 'empty';
}

// The 44px hit-area contract is flow-scoped: shared controls stay compact
// elsewhere, so the overview wraps its controls and raises the interactive
// descendants rather than resizing every consumer of Button/Segment/Selector.
const TARGET_44 = '[&_button]:min-h-[44px] [&_[role=radio]]:min-h-[44px] [&_[role=combobox]]:min-h-[44px]';

export function OverviewPage() {
  const projectID = useAuthStore((s) => s.project?.id);
  const [period, setPeriod] = useState('7d');
  const [platform, setPlatform] = useState('');

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

  const viewState = overviewViewState({
    projectID,
    isLoading: query.isLoading,
    error: query.error,
    res,
    showFirstEvent: firstValue.showFirstEvent,
    platform,
    period,
  });

  const freshness = res ? freshnessLabel(res) : null;
  const occurredAt = res?.data_status.last_event_at
    ? new Date(res.data_status.last_event_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC'
    : 'not available';
  const receivedAt = res?.data_status.last_received_at
    ? new Date(res.data_status.last_received_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC'
    : 'not available';
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
    ? res.context.range.complete_days
      ? `${res.context.range.from.slice(0, 10)} → ${res.context.range.to.slice(0, 10)} · ${res.context.range.days} complete days · ${res.context.timezone}${res.context.timezone_source === 'fallback' ? ' (UTC fallback — no project timezone set)' : ''}`
      : `Today so far · ${res.context.timezone}${res.context.timezone_source === 'fallback' ? ' (UTC fallback — no project timezone set)' : ''} · partial day, no comparison`
    : '';

  const dataStatusPanel = res ? (
    <Panel title="Data status" action={freshness ? <span className="text-xs text-[var(--color-text-secondary)]">{freshness.text}</span> : null}>
      <div className="flex flex-wrap gap-x-8 gap-y-2 text-sm">
        <span>{formatCompact(res.data_status.events_in_range)} events in range</span>
        <span>{formatCompact(res.data_status.qualifying_in_range)} qualifying (human product activity)</span>
        <span>Last occurred: {occurredAt}</span>
        <span>Last received: {receivedAt}</span>
        <span className="text-[var(--color-text-secondary)]">
          Pipeline lag: {res.data_status.pipeline_lag === 'unavailable' ? 'not measured yet' : res.data_status.pipeline_lag}
        </span>
        <span className="text-[var(--color-text-secondary)]">
          Schema health: {res.data_status.schema_status === 'unavailable' ? 'not measured yet' : res.data_status.schema_status}
        </span>
      </div>

      {res.data_status.sources.length === 0 ? (
        <p className="mt-3 text-sm text-[var(--color-text-secondary)]">No connected data sources.</p>
      ) : (
        <div className="mt-3 flex flex-col gap-2">
          {res.data_status.sources.map((source) => (
            <div key={source.sync_id || source.connector_id} className="border-t border-[var(--color-border)] pt-2 text-sm">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span className="font-medium">{source.connector_name}</span>
                <span className="text-[var(--color-text-secondary)]">{source.source_table || 'No table configured'}</span>
                <span className="text-[var(--color-text-secondary)]">
                  {source.state === 'healthy' ? 'Healthy' : source.state === 'partial' ? 'Partial data' : source.state === 'error' ? 'Needs attention' : source.state === 'paused' ? 'Paused' : source.state === 'not_ready' ? 'Not run yet' : 'Set up a table'}
                </span>
              </div>
              {source.sync_configured ? (
                <p className="mt-1 text-xs text-[var(--color-text-secondary)]">
                  Last success {source.last_success_at ? new Date(source.last_success_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : 'never'} · last attempt {source.last_run_at ? new Date(source.last_run_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : 'never'} · resume cursor <span className="font-mono">{source.cursor || '—'}</span>{source.cursor_key ? <> (<span className="font-mono">{source.cursor_key}</span>)</> : null}
                </p>
              ) : null}
              {source.last_error ? <p className="mt-1 text-xs text-[var(--color-text-secondary)]">Last error: {source.last_error}</p> : null}
            </div>
          ))}
          {res.data_status.sources_truncated ? <p className="text-xs text-[var(--color-text-secondary)]">Showing the first 20 connected sources.</p> : null}
        </div>
      )}
    </Panel>
  ) : null;

  return (
    <AppShell>
      <PageShell
        title="Overview"
        sub={rangeLabel || 'The last complete days, at a glance.'}
        actions={
          <div className={`flex flex-wrap items-center gap-2 ${TARGET_44}`}>
            <Selector
              size="sm"
              label="Platform"
              isLabelHidden
              value={platform || 'all'}
              onChange={(v) => setPlatform(v === 'all' ? '' : String(v))}
              options={[{ value: 'all', label: 'All platforms' }, ...PLATFORM_OPTIONS]}
            />
            <Segment options={PERIODS} value={period} onChange={setPeriod} label="Time range" />
          </div>
        }
      >
        {viewState === 'loading' ? (
          <div role="status" aria-label="Loading overview" className="flex flex-col gap-4">
            <Loading label="Loading overview…" />
            <Panel title="Active people per day"><Loading label="" /></Panel>
            <Panel title="Retention"><Loading label="" /></Panel>
          </div>
        ) : null}

        {viewState === 'no_access' ? (
          <Callout
            tone="warn"
            icon={<Lock size={16} />}
            label="No access"
            title="You cannot read this project's analytics"
            detail="The overview needs analytics-read access on this workspace. Ask a workspace owner or admin to grant it, then reload."
          />
        ) : null}

        {viewState === 'error' ? (
          <Callout
            tone="warn"
            icon={<AlertTriangle size={16} />}
            label="Overview unavailable"
            title="Could not load the overview"
            detail={query.error instanceof Error ? query.error.message : 'The overview request failed.'}
            action={<Button variant="outline" size="sm" className="min-h-[44px]" icon={<RefreshCw size={14} />} onClick={() => void query.refetch()}>Retry</Button>}
          />
        ) : null}

        {viewState === 'first_run' ? (
          <>
            <FirstEventQuickstart />
            {res?.data_status.ever_received ? dataStatusPanel : null}
          </>
        ) : null}

        {viewState === 'filtered_empty' && res ? (
          <>
            <EmptyState
              title="No events match this filter"
              detail={`Nothing arrived for ${platform ? platformLabel(platform) : 'this platform'} in the selected range.`}
              action={
                <Button variant="outline" size="sm" className="min-h-[44px]" onClick={() => { setPlatform(''); setPeriod('7d'); }}>
                  Reset to all platforms, 7 days
                </Button>
              }
            />
            {dataStatusPanel}
          </>
        ) : null}

        {(viewState === 'data' || viewState === 'receipt_only' || viewState === 'empty') && res ? (
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
              <summary className="cursor-pointer select-none py-3">How these numbers are computed</summary>
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

            {dataStatusPanel}

            <Panel title="Next step">
              <div className={`flex flex-wrap items-center gap-3 text-sm ${TARGET_44}`}>
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
