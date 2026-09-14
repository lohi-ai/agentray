import { describe, expect, it } from 'vitest';
import type { BoardTile, OverviewMetric, OverviewResult } from '@/lib/api';
import { UNSERVED_TILES } from '@/lib/analysis';
import { analysisBars, analysisStat, analysisSeries, unservedStat } from './tiles';

function metric(state: OverviewMetric['state'], value?: number): OverviewMetric {
  return { state, definition: 'test', ...(value === undefined ? {} : { value }) };
}

function res(over: Partial<OverviewResult> = {}): OverviewResult {
  return {
    context: {
      project_id: 'p1',
      timezone: 'UTC',
      timezone_source: 'project',
      range: { from: '2026-09-05T00:00:00Z', to: '2026-09-12T00:00:00Z', days: 7, complete_days: true },
      previous_range: { from: '2026-08-29T00:00:00Z', to: '2026-09-05T00:00:00Z', days: 7, complete_days: true },
      platform: '',
      generated_at: '2026-09-12T00:00:00Z',
      metric_version: 'overview.v3',
    },
    metrics: {
      active_users: metric('ok', 1240),
      new_users: metric('ok', 320),
      sessions: metric('ok', 2860),
      activation: metric('unconfigured'),
      revenue: metric('unconfigured'),
    },
    trend: [{ day: '2026-09-05', active_users: 180 }],
    retention: {
      cohort_window: 'lifetime',
      d1: { state: 'ok', rate: 0.41, returned: 132, eligible: 320 },
      d7: { state: 'ok', rate: 0.24, returned: 48, eligible: 200 },
      d30: { state: 'not_ready', rate: 0, returned: 0, eligible: 0 },
    },
    content: {
      top_pages: { unit: 'pageviews', rows: [{ value: '/pricing', count: 612 }] },
      top_sources: { unit: 'pageviews', rows: [{ value: 'Direct / unknown', count: 540 }] },
    },
    data_status: {
      last_received_at: '2026-09-12T00:02:00Z',
      pipeline_lag: 'unavailable',
      schema_status: 'unavailable',
      events_in_range: 8412,
      qualifying_in_range: 6930,
      ever_received: true,
      state: 'fresh',
      sources: [],
      sources_truncated: false,
    },
    ...over,
  };
}

function tile(metric: string, title: string): BoardTile {
  return { key: metric, metric, title, kind: 'metric', display: 'stat' };
}

describe('analysisStat', () => {
  it('uses App Store Connect labels on AgentRay values', () => {
    const overview = res();
    expect(analysisStat(overview, tile('new_users', 'First-time downloads'))).toEqual(expect.objectContaining({
      label: 'First-time downloads',
      value: '320',
    }));
    expect(analysisStat(overview, tile('active_users', 'Active devices'))?.label).toBe('Active devices');
    expect(analysisStat(overview, tile('revenue', 'Proceeds'))).toEqual(expect.objectContaining({
      label: 'Proceeds',
      value: 'Set up',
    }));
    expect(analysisStat(overview, tile('retention_d1', 'Average retention D1'))?.value).toBe('41.0%');
    expect(analysisStat(overview, tile('retention_d30', 'Average retention D30'))?.value).toBe('Not ready');
  });

  it('never fabricates a zero for an unconfigured or missing metric', () => {
    const overview = res();
    expect(analysisStat(overview, tile('revenue', 'Proceeds'))?.value).toBe('Set up');
    expect(unservedStat(overview, 'Redownloads').value).toBe('Not available');
    expect(unservedStat(overview, 'Crashes by app version').value).not.toBe('0');
  });
});

describe('analysisBars', () => {
  it('keeps Direct / unknown in top sources', () => {
    const bars = analysisBars(res(), { key: 'top-sources', metric: 'top_sources', title: 'Top acquisition sources', kind: 'metric', display: 'bar' });
    expect(bars?.rows.map((r) => r.value)).toContain('Direct / unknown');
  });
});

describe('analysisSeries', () => {
  it('projects the daily active-people trend', () => {
    const series = analysisSeries(res(), { key: 'trend', metric: 'active_users_daily', title: 'Active devices per day', kind: 'metric', display: 'area' });
    expect(series?.points).toEqual([{ label: '2026-09-05', value: 180 }]);
  });
});

describe('UNSERVED_TILES', () => {
  it('names Apple-shaped metrics that AgentRay does not compute', () => {
    expect(UNSERVED_TILES.acquisition.map((t) => t.label)).toEqual([
      'Redownloads', 'Conversion rate', 'Impressions / day', 'Product page views', 'Updates',
    ]);
    expect(UNSERVED_TILES.monetization.map((t) => t.label)).toContain('Paying users');
    expect(UNSERVED_TILES.usage.map((t) => t.label)).toEqual([
      'Average retention D14', 'Crashes by app version',
    ]);
  });
});
