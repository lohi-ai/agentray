'use client';

import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useRouter } from 'next/navigation';
import { Pencil, Sparkles, Trash2 } from 'lucide-react';
import { AgentRayAPI, type ActivitySummary, type Chart, type Filters } from '@/lib/api';
import { useFiltersStore } from '@/lib/app-state';
import { formatCompact, formatCost } from '@/lib/format';
import { Chart as Graph, type ChartSpec } from '@/modules/shared/components/charts';

// withTimeWindow injects the dashboard's applied range into a chart's SQL so
// SQL-backed charts honour the global filter, the same way metric charts do.
// Substitution is opt-in and backward compatible: SQL without any {{…}} token
// runs verbatim. The agent's create_chart can write windowed SQL like
//   WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'
// and {{hours}} expands to the window length for INTERVAL-style queries.
function withTimeWindow(sql: string, filters: Filters): string {
  if (!/\{\{\s*(from|to|hours)\s*\}\}/.test(sql)) return sql;
  const to = filters.to ? new Date(filters.to) : new Date();
  const from = filters.from ? new Date(filters.from) : new Date(to.getTime() - filters.hours * 3600_000);
  const hours = filters.from && filters.to
    ? Math.max(1, Math.round((to.getTime() - from.getTime()) / 3600_000))
    : filters.hours;
  return sql
    .replace(/\{\{\s*from\s*\}\}/g, from.toISOString())
    .replace(/\{\{\s*to\s*\}\}/g, to.toISOString())
    .replace(/\{\{\s*hours\s*\}\}/g, String(hours));
}

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
function SeriesChart({ values, labels, type }: { values: number[]; labels?: (string | number)[]; type: ChartSpec['type'] }) {
  if (values.length === 0) {
    return <div className="block w-full grid place-items-center text-[var(--color-text-secondary)] text-xs" style={{ height: 168 }}>No data in range</div>;
  }
  const latest = values[values.length - 1];
  return (
    <div>
      <Graph spec={{ type, x: labels, series: [{ data: values }], height: 168 }} />
      <div className="text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)] tracking-normal mt-1.5">
        latest <span className="font-mono tabular-nums text-primary font-semibold">{formatCompact(latest)}</span>
      </div>
    </div>
  );
}

// SqlGraph runs the chart's saved query and plots its y column against an x label
// column (the first non-numeric field, or x_field if set).
function SqlGraph({ chart, projectID }: { chart: Chart; projectID: string }) {
  const api = useMemo(() => new AgentRayAPI(projectID), [projectID]);
  const applied = useFiltersStore((s) => s.appliedFilters);
  const sql = useMemo(() => withTimeWindow(chart.sql, applied), [chart.sql, applied]);
  const [data, setData] = useState<{ values: number[]; labels: (string | number)[] } | null>(null);

  useEffect(() => {
    let active = true;
    api.runSQL(sql)
      .then((res) => {
        if (!active) return;
        const first = res.rows[0] ?? {};
        const yField = chart.y_field || Object.keys(first).find((k) => typeof first[k] === 'number') || '';
        const xField = chart.x_field || Object.keys(first).find((k) => k !== yField && typeof first[k] !== 'number') || '';
        setData({
          values: res.rows.map((r) => Number(r[yField]) || 0),
          labels: res.rows.map((r, i) => (xField ? String(r[xField]) : i + 1)),
        });
      })
      .catch(() => { if (active) setData({ values: [], labels: [] }); });
    return () => { active = false; };
  }, [api, sql, chart.y_field, chart.x_field]);

  if (data === null) return <div className="block w-full grid place-items-center text-[var(--color-text-secondary)] text-xs" style={{ height: 140 }}>Running query…</div>;
  return <SeriesChart values={data.values} labels={data.labels} type={specType(chart.kind)} />;
}

// ChartCard renders one saved chart. In `preview` mode (used by the editor) the
// action row is hidden so the same render path drives both the live board and
// the editor preview — one source of truth for how a chart looks.
export function ChartCard({ chart, summary, projectID, onDelete, onEdit, handle, preview = false }: { chart: Chart; summary: ActivitySummary | null; projectID?: string; onDelete?: () => void; onEdit?: () => void; handle?: ReactNode; preview?: boolean }) {
  const router = useRouter();

  // Hand the chart to the agent chat to explain what it shows. Only offered for
  // SQL-backed charts — there's a real query for the agent to reason about and run.
  function explain() {
    if (!chart.sql) return;
    const q = `Explain what this chart ("${chart.name || 'Untitled'}") shows, in plain language. The query behind it is:\n\n${chart.sql}`;
    router.push(`/chat?q=${encodeURIComponent(q)}`);
  }

  return (
    <div className="rounded-xl bg-[var(--color-background-card)] p-4">
      <div className="flex items-center mb-3">
        {handle}
        <h3 className="m-0 text-[13px] font-semibold">{chart.name || 'Untitled chart'}</h3>
        {preview ? null : (
          <div className="ms-auto flex items-center gap-0.5">
            {chart.sql ? (
              <button className="grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-agent" title="Ask the agent to explain this chart" onClick={explain}><Sparkles size={14} /></button>
            ) : null}
            {onEdit ? (
              <button className="grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]" title="Edit chart" onClick={onEdit}><Pencil size={14} /></button>
            ) : null}
            {onDelete ? (
              <button className="grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]" title="Delete chart" onClick={onDelete}><Trash2 size={14} /></button>
            ) : null}
          </div>
        )}
      </div>
      {chart.kind === 'stat' ? (
        <div className="font-mono tabular-nums text-[28px] font-semibold text-primary">{statValue(chart.metric, summary)}</div>
      ) : chart.sql ? (
        projectID ? <SqlGraph chart={chart} projectID={projectID} /> : <SeriesChart values={[]} type={specType(chart.kind)} />
      ) : (
        <SeriesChart
          values={(summary?.timeline ?? []).map((p) => p.count)}
          labels={(summary?.timeline ?? []).map((p) => p.hour)}
          type={specType(chart.kind)}
        />
      )}
    </div>
  );
}
