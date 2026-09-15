import type { BoardTile, OverviewResult } from '@/lib/api';
import { metricTile, retentionTile, revenueTile, targetBadge, tileProvenance } from '@/modules/overview/page';

export type AnalysisStat = {
  label: string;
  value: string;
  delta?: string;
  deltaTone?: 'up' | 'down';
  badge?: { status: string; label: string };
  provenance: string;
};

export type AnalysisBars = {
  label: string;
  unit: string;
  rows: Array<{ value: string; count: number }>;
  provenance: string;
  empty: string;
};

export type AnalysisSeries = {
  label: string;
  points: Array<{ label: string; value: number }>;
  provenance: string;
};

function titleOf(tile: BoardTile, fallback: string): string {
  return tile.title?.trim() || fallback;
}

export function analysisStat(res: OverviewResult, tile: BoardTile): AnalysisStat | null {
  const metric = tile.metric;
  if (!metric) return null;
  if (metric === 'new_users') {
    const m = res.metrics.new_users;
    return { ...metricTile(titleOf(tile, 'First-time downloads'), m), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'metric', metric: m }) };
  }
  if (metric === 'active_users') {
    const m = res.metrics.active_users;
    return { ...metricTile(titleOf(tile, 'Active devices'), m), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'metric', metric: m }) };
  }
  if (metric === 'sessions') {
    const m = res.metrics.sessions;
    return { ...metricTile(titleOf(tile, 'Sessions'), m), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'metric', metric: m }) };
  }
  if (metric === 'activation') {
    const m = res.metrics.activation;
    return { ...metricTile(titleOf(tile, 'Activation'), m), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'metric', metric: m }) };
  }
  if (metric === 'revenue') {
    const labelled = { ...revenueTile(res.metrics.revenue, res.metrics.revenue_detail), label: titleOf(tile, 'Proceeds') };
    return { ...labelled, badge: targetBadge(res.metrics.revenue.target), provenance: tileProvenance(res, { kind: 'money', metric: res.metrics.revenue, detail: res.metrics.revenue_detail }) };
  }
  if (metric === 'retention_d1') {
    return { ...retentionTile(titleOf(tile, 'Average retention D1'), res.retention.d1), badge: targetBadge(res.retention.d1.target), provenance: tileProvenance(res, { kind: 'retention', day: 1, point: res.retention.d1 }) };
  }
  if (metric === 'retention_d7') {
    return { ...retentionTile(titleOf(tile, 'Average retention D7'), res.retention.d7), badge: targetBadge(res.retention.d7.target), provenance: tileProvenance(res, { kind: 'retention', day: 7, point: res.retention.d7 }) };
  }
  if (metric === 'retention_d30') {
    return { ...retentionTile(titleOf(tile, 'Average retention D30'), res.retention.d30), badge: targetBadge(res.retention.d30.target), provenance: tileProvenance(res, { kind: 'retention', day: 30, point: res.retention.d30 }) };
  }
  return null;
}

export function analysisBars(res: OverviewResult, tile: BoardTile): AnalysisBars | null {
  if (tile.metric === 'top_pages') {
    return {
      label: titleOf(tile, 'Top pages'),
      unit: res.content.top_pages.unit,
      rows: res.content.top_pages.rows,
      provenance: tileProvenance(res, { kind: 'unserved' }),
      empty: 'No pageviews in this range',
    };
  }
  if (tile.metric === 'top_sources') {
    return {
      label: titleOf(tile, 'Top acquisition sources'),
      unit: res.content.top_sources.unit,
      rows: res.content.top_sources.rows,
      provenance: tileProvenance(res, { kind: 'unserved' }),
      empty: 'No attributed sources in this range',
    };
  }
  return null;
}

export function analysisSeries(res: OverviewResult, tile: BoardTile): AnalysisSeries | null {
  if (tile.metric !== 'active_users_daily') return null;
  return {
    label: titleOf(tile, 'Active devices per day'),
    points: res.trend.map((p) => ({ label: p.day, value: p.active_users })),
    provenance: tileProvenance(res, { kind: 'metric', metric: res.metrics.active_users }),
  };
}

export function unservedStat(res: OverviewResult, label: string): AnalysisStat {
  return { label, value: 'Not available', provenance: tileProvenance(res, { kind: 'unserved' }) };
}
