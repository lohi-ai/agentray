import type { InsightResult } from '@/lib/api';
import { formatCompact, formatFractionAsPercent, formatPercent } from '@/lib/format';

export type HeadlineStat = {
  label: string;
  value: string;
  tone?: 'success' | 'warning' | 'danger';
  delta?: string;
  deltaTone?: 'up' | 'down';
};

// Store funnel conversion is a 0..1 fraction (users at this step / users at
// step 0). Tone thresholds are 0..100 percents — scale before comparing.
function funnelTone(fraction: number): HeadlineStat['tone'] {
  const pct = fraction * 100;
  if (pct >= 50) return 'success';
  if (pct >= 20) return 'warning';
  return 'danger';
}

export function headlineStats(insight: InsightResult): HeadlineStat[] {
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
      { label: 'Conversion', value: formatFractionAsPercent(conv, 0), tone: funnelTone(conv) },
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
