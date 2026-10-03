'use client';

import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useRouter } from 'next/navigation';
import { Pencil, Sparkles, Trash2 } from 'lucide-react';
import { Card } from '@astryxdesign/core/Card';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { IconButton } from '@astryxdesign/core/IconButton';
import { Text } from '@astryxdesign/core/Text';
import { AgentRayAPI, APIError, type ActivitySummary, type Chart } from '@/lib/api';
import { useFiltersStore } from '@/lib/app-state';
import { formatCompact, formatCost } from '@/lib/format';
import { Chart as Graph, type ChartAnnotation, type ChartSpec } from '@/modules/shared/components/charts';
import { chartRangeCaption, projectChartRows, resolveChartQuery } from './chart-query';

// specType maps a saved chart's kind to the shared ECharts ChartSpec type. A
// plain line reads as a filled area trend; bars stay bars; everything else falls
// back to a line.
function specType(kind: Chart['kind']): ChartSpec['type'] {
  if (kind === 'bar') return 'bar';
  if (kind === 'pie') return 'pie';
  return 'area';
}

// statValue reads a single scalar for `stat` cards straight off the activity summary.
function statValue(metric: Chart['metric'], summary: ActivitySummary | null): string {
  if (!summary) return '—';
  switch (metric) {
    case 'tokens': return formatCompact(summary.total_tokens_in + summary.total_tokens_out);
    case 'cost': return formatCost(summary.total_cost_usd);
    case 'sessions': return formatCompact(summary.sessions);
    case 'event_breakdown': return formatCompact(summary.event_counts[0]?.count ?? 0);
    default: return formatCompact(summary.event_count);
  }
}

// SeriesChart leads with the graph — that's the point of the card. A single
// compact "latest" figure anchors the trend; peak/avg are left to the ECharts
// hover tooltip rather than crowding the card with always-on labels.
function SeriesChart({ values, labels, type, annotations }: { values: number[]; labels?: (string | number)[]; type: ChartSpec['type']; annotations?: ChartAnnotation[] }) {
  if (values.length === 0) {
    return <div className="grid w-full place-items-center" style={{ height: 168 }}><Text type="supporting">No data in range</Text></div>;
  }
  const latest = values[values.length - 1];
  return (
    <div>
      <Graph spec={{ type, x: labels, series: [{ data: values }], height: 168, annotations }} />
      <div className="mt-2">
        <Text type="supporting">
          latest <span className="font-mono tabular-nums font-semibold text-primary">{formatCompact(latest)}</span>
        </Text>
      </div>
    </div>
  );
}

// SqlGraph runs the chart's saved query and plots its y column against an x label
// column (the first non-numeric field, or x_field if set).
function SqlGraph({ chart, projectID, annotations }: { chart: Chart; projectID: string; annotations?: ChartAnnotation[] }) {
  const api = useMemo(() => new AgentRayAPI(projectID), [projectID]);
  const applied = useFiltersStore((s) => s.appliedFilters);
  const query = useMemo(() => resolveChartQuery(chart.sql, applied), [chart.sql, applied]);
  const requestKey = useMemo(
    () => Symbol(query.ok
      ? `${projectID}\u0000${query.sql}\u0000${chart.y_field ?? ''}\u0000${chart.x_field ?? ''}`
      : `${projectID}\u0000unsupported:${query.message}`),
    [projectID, query, chart.y_field, chart.x_field],
  );
  type State =
    | { key: symbol | null; status: 'loading' }
    | { key: symbol; status: 'ready'; values: number[]; labels: (string | number)[] }
    | { key: symbol; status: 'empty' | 'unsupported' | 'error' | 'capacity' | 'non_plottable'; message: string };
  const [data, setData] = useState<State>({ key: null, status: 'loading' });

  useEffect(() => {
    if (!query.ok) return;
    let active = true;
    api.runSQL(query.sql)
      .then((res) => {
        if (!active) return;
        const projected = projectChartRows(res.rows, chart.y_field, chart.x_field);
        if (projected.status === 'ready') setData({ key: requestKey, ...projected });
        else if (projected.status === 'empty') setData({
          key: requestKey,
          status: 'empty',
          message: query.status === 'fixed' ? 'No data returned' : 'No data in range',
        });
        else setData({ key: requestKey, ...projected });
      })
      .catch((error: unknown) => {
        if (!active) return;
        if (error instanceof APIError && (error.kind === 'retryable' || error.status === 503)) {
          setData({ key: requestKey, status: 'capacity', message: 'Query capacity is busy. Retry shortly.' });
          return;
        }
        setData({ key: requestKey, status: 'error', message: 'Query failed. Review the saved SQL and try again.' });
      });
    return () => { active = false; };
  }, [api, query, requestKey, chart.y_field, chart.x_field]);

  const current: State = data.key === requestKey
    ? data
    : query.ok
      ? { key: requestKey, status: 'loading' }
      : { key: requestKey, status: 'unsupported', message: query.message };
  const body = current.status === 'ready'
    ? <SeriesChart values={current.values} labels={current.labels} type={specType(chart.kind)} annotations={annotations} />
    : (
      <div
        className="grid w-full place-items-center px-3 text-center"
        style={{ height: 168 }}
        role={current.status === 'error' || current.status === 'capacity' || current.status === 'unsupported' || current.status === 'non_plottable' ? 'alert' : undefined}
      >
        <Text type="supporting">{current.status === 'loading' ? 'Running query…' : current.message}</Text>
      </div>
    );
  const rangeCaption = chartRangeCaption(query);
  return (
    <div>
      {body}
      {rangeCaption ? <div className="mt-2"><Text type="supporting">{rangeCaption}</Text></div> : null}
    </div>
  );
}

// ChartCard renders one saved chart. In `preview` mode (used by the editor) the
// action row is hidden so the same render path drives both the live board and
// the editor preview — one source of truth for how a chart looks.
export function ChartCard({ chart, summary, projectID, onDelete, onEdit, handle, preview = false, annotations }: { chart: Chart; summary: ActivitySummary | null; projectID?: string; onDelete?: () => void; onEdit?: () => void; handle?: ReactNode; preview?: boolean; annotations?: ChartAnnotation[] }) {
  const router = useRouter();

  // Hand the chart to the agent chat to explain what it shows. Only offered for
  // SQL-backed charts — there's a real query for the agent to reason about and run.
  function explain() {
    if (!chart.sql) return;
    const q = `Explain what this chart ("${chart.name || 'Untitled'}") shows, in plain language. The query behind it is:\n\n${chart.sql}`;
    router.push(`/chat?q=${encodeURIComponent(q)}`);
  }

  // Astryx migration: the hand-rolled card div is now an Astryx <Card>, the
  // title a <Heading level={5}>, and the three action buttons Astryx
  // <IconButton>s. That last swap is not cosmetic — the raw buttons carried
  // only `title`, which is not an accessible name, and sat at 26px against a
  // 44px touch target. IconButton's `label` is the accessible name and its
  // tooltip keeps the hover affordance.
  return (
    <Card padding={4}>
      <HStack align="center" gap={0} className="mb-3">
        {handle}
        <Heading level={5}>{chart.name || 'Untitled chart'}</Heading>
        {preview ? null : (
          <HStack align="center" gap={0.5} className="ms-auto">
            {chart.sql ? (
              <IconButton label="Explain this chart" tooltip="Ask the agent to explain this chart" variant="ghost" size="sm" icon={<Sparkles size={14} />} onClick={explain} />
            ) : null}
            {onEdit ? (
              <IconButton label="Edit chart" tooltip="Edit chart" variant="ghost" size="sm" icon={<Pencil size={14} />} onClick={onEdit} />
            ) : null}
            {onDelete ? (
              <IconButton label="Delete chart" tooltip="Delete chart" variant="ghost" size="sm" icon={<Trash2 size={14} />} onClick={onDelete} />
            ) : null}
          </HStack>
        )}
      </HStack>
      {chart.kind === 'stat' ? (
        <div className="font-mono tabular-nums text-[28px] font-semibold text-primary">{statValue(chart.metric, summary)}</div>
      ) : chart.sql ? (
        projectID ? <SqlGraph chart={chart} projectID={projectID} annotations={annotations} /> : <SeriesChart values={[]} type={specType(chart.kind)} />
      ) : (
        <SeriesChart
          values={(summary?.timeline ?? []).map((p) => p.count)}
          labels={(summary?.timeline ?? []).map((p) => p.hour)}
          type={specType(chart.kind)}
          annotations={annotations}
        />
      )}
    </Card>
  );
}
