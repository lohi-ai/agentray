import type { BoardTile, OverviewMetric, OverviewResult } from '@/lib/api';
import { formatDuration, formatPercent } from '@/lib/format';
import { platformLabel } from '@/lib/platform';
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

// rateTile renders a metric whose honest value is a float — a share or rate on
// the percent scale, a duration in seconds — never a count. The unit decides
// the format; a metric that is not "ok" shows its state, never a fabricated 0.
export function rateTile(label: string, m: OverviewMetric, unit: 'percent' | 'seconds'): { label: string; value: string } {
  if (m.state !== 'ok' || m.rate === undefined) {
    const stateLabel =
      m.state === 'unconfigured' ? 'Set up'
      : m.state === 'not_ready' ? 'Not ready'
      : m.state === 'no_data' ? 'No data'
      : 'Not available';
    return { label, value: stateLabel };
  }
  return { label, value: unit === 'percent' ? formatPercent(m.rate) : formatDuration(m.rate) };
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
  if (metric === 'pageviews') {
    const m = res.metrics.pageviews;
    return { ...metricTile(titleOf(tile, 'Pageviews'), m), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'eventsMetric', metric: m }) };
  }
  if (metric === 'conversions') {
    const m = res.metrics.conversions;
    return { ...metricTile(titleOf(tile, 'Conversions'), m), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'eventsMetric', metric: m }) };
  }
  if (metric === 'ai_share') {
    const m = res.metrics.ai_share;
    return { ...rateTile(titleOf(tile, 'AI traffic'), m, 'percent'), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'eventsMetric', metric: m }) };
  }
  if (metric === 'bounce_rate') {
    const m = res.metrics.bounce_rate;
    return { ...rateTile(titleOf(tile, 'Bounce rate'), m, 'percent'), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'eventsMetric', metric: m }) };
  }
  if (metric === 'avg_session_duration') {
    const m = res.metrics.avg_session_duration;
    return { ...rateTile(titleOf(tile, 'Avg session'), m, 'seconds'), badge: targetBadge(m.target), provenance: tileProvenance(res, { kind: 'eventsMetric', metric: m }) };
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
  if (tile.metric === 'top_utm_sources') {
    return {
      label: titleOf(tile, 'Top UTM sources'),
      unit: res.content.top_utm_sources.unit,
      rows: res.content.top_utm_sources.rows,
      provenance: tileProvenance(res, { kind: 'unserved' }),
      empty: 'No UTM-tagged visits in this range',
    };
  }
  if (tile.metric === 'top_campaigns') {
    return {
      label: titleOf(tile, 'Top campaigns'),
      unit: res.content.top_campaigns.unit,
      rows: res.content.top_campaigns.rows,
      provenance: tileProvenance(res, { kind: 'unserved' }),
      empty: 'No campaign-tagged visits in this range',
    };
  }
  if (tile.metric === 'top_referrers') {
    return {
      label: titleOf(tile, 'Top referrers'),
      unit: res.content.top_referrers.unit,
      rows: res.content.top_referrers.rows,
      provenance: tileProvenance(res, { kind: 'unserved' }),
      empty: 'No external referrers in this range',
    };
  }
  if (tile.metric === 'traffic_by_class') {
    return {
      label: titleOf(tile, 'Traffic by type'),
      unit: res.content.traffic_by_class.unit,
      rows: res.content.traffic_by_class.rows,
      provenance: tileProvenance(res, { kind: 'events' }),
      empty: 'No pageviews in this range',
    };
  }
  if (tile.metric === 'ai_top_paths') {
    return {
      label: titleOf(tile, 'AI-cited pages'),
      unit: res.content.ai_top_paths.unit,
      rows: res.content.ai_top_paths.rows,
      provenance: tileProvenance(res, { kind: 'events' }),
      empty: 'No AI crawler traffic yet',
    };
  }
  if (tile.metric === 'traffic_by_platform') {
    return {
      label: titleOf(tile, 'Pageviews by platform'),
      unit: res.content.traffic_by_platform.unit,
      rows: res.content.traffic_by_platform.rows.map((r) => ({ value: platformLabel(r.value), count: r.count })),
      provenance: tileProvenance(res, { kind: 'events' }),
      empty: 'No events in this range',
    };
  }
  if (tile.metric === 'top_events') {
    return {
      label: titleOf(tile, 'Top events'),
      unit: res.content.top_events.unit,
      rows: res.content.top_events.rows,
      provenance: tileProvenance(res, { kind: 'events' }),
      empty: 'No events in this range',
    };
  }
  return null;
}

export function analysisSeries(res: OverviewResult, tile: BoardTile): AnalysisSeries | null {
  if (tile.metric === 'active_users_daily') {
    return {
      label: titleOf(tile, 'Active devices per day'),
      points: res.trend.map((p) => ({ label: p.day, value: p.active_users })),
      provenance: tileProvenance(res, { kind: 'metric', metric: res.metrics.active_users }),
    };
  }
  if (tile.metric === 'event_volume_daily') {
    return {
      label: titleOf(tile, 'Events per day'),
      points: res.trend.map((p) => ({ label: p.day, value: p.events })),
      provenance: tileProvenance(res, { kind: 'events' }),
    };
  }
  return null;
}

export function unservedStat(res: OverviewResult, label: string): AnalysisStat {
  return { label, value: 'Not available', provenance: tileProvenance(res, { kind: 'unserved' }) };
}
