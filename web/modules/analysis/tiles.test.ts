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
      metric_version: 'overview.v6',
    },
    metrics: {
      active_users: metric('ok', 1240),
      new_users: metric('ok', 320),
      sessions: metric('ok', 2860),
      activation: metric('unconfigured'),
      revenue: metric('unconfigured'),
      pageviews: metric('ok', 5128),
      conversions: metric('ok', 41),
      ai_share: { state: 'ok', rate: 6.4, definition: 'test' },
      bounce_rate: { state: 'ok', rate: 38.2, definition: 'test' },
      avg_session_duration: { state: 'ok', rate: 161, definition: 'test' },
      sessions_per_user: { state: 'ok', rate: 2.31, definition: 'test' },
      paying_users: metric('unconfigured'),
      proceeds_per_paying: metric('unconfigured'),
    },
    trend: [{ day: '2026-09-05', active_users: 180, sessions: 410, events: 940 }],
    retention: {
      cohort_window: 'lifetime',
      d1: { state: 'ok', rate: 0.41, returned: 132, eligible: 320 },
      d7: { state: 'ok', rate: 0.24, returned: 48, eligible: 200 },
      d30: { state: 'not_ready', rate: 0, returned: 0, eligible: 0 },
    },
    paid_conversion: {
      cohort_window: 'lifetime',
      d1: { state: 'unconfigured', rate: 0, returned: 0, eligible: 0 },
      d7: { state: 'unconfigured', rate: 0, returned: 0, eligible: 0 },
      d35: { state: 'unconfigured', rate: 0, returned: 0, eligible: 0 },
    },
    content: {
      top_pages: { unit: 'pageviews', rows: [{ value: '/pricing', count: 612 }] },
      top_sources: { unit: 'pageviews', rows: [{ value: 'Direct / unknown', count: 540 }] },
      top_utm_sources: { unit: 'pageviews', rows: [{ value: 'newsletter', count: 210 }] },
      top_campaigns: { unit: 'pageviews', rows: [{ value: 'launch-week', count: 180 }] },
      top_referrers: { unit: 'pageviews', rows: [{ value: 'google.com', count: 300 }] },
      traffic_by_class: { unit: 'pageviews', rows: [{ value: 'human', count: 4800 }, { value: 'ai-platform', count: 328 }] },
      ai_top_paths: { unit: 'pageviews', rows: [{ value: '/pricing', count: 88 }] },
      traffic_by_platform: { unit: 'pageviews', rows: [{ value: 'web', count: 5128 }] },
      top_events: { unit: 'events', rows: [{ value: 'user.pageview', count: 5128 }] },
      first_read_discovery: {
        unit: 'people',
        rows: [
          { value: 'home', count: 120 },
          { value: 'search', count: 60 },
          { value: 'communication', count: 40 },
          { value: 'direct', count: 20 },
        ],
      },
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

  it('computes % per surface on first_read_discovery rows', () => {
    const overview = res();
    const bars = analysisBars(overview, {
      key: 'first-read-discovery',
      metric: 'first_read_discovery',
      title: 'First-read discovery',
      kind: 'metric',
      display: 'bar',
    });
    expect(bars?.label).toBe('First-read discovery');
    expect(bars?.unit).toBe('people');
    // Total = 120 + 60 + 40 + 20 = 240
    // home: 120/240 = 50%
    // search: 60/240 = 25%
    // communication: 40/240 = 17%
    // direct: 20/240 = 8%
    expect(bars?.rows).toEqual([
      { value: 'home (50%)', count: 120 },
      { value: 'search (25%)', count: 60 },
      { value: 'communication (17%)', count: 40 },
      { value: 'direct (8%)', count: 20 },
    ]);
  });

  it('reports empty state when no activation event is configured', () => {
    const overview = res({
      metrics: {
        ...res().metrics,
        activation: metric('unconfigured'),
      },
      content: {
        ...res().content,
        first_read_discovery: { unit: 'people', rows: [] },
      },
    });
    const bars = analysisBars(overview, {
      key: 'first-read-discovery',
      metric: 'first_read_discovery',
      title: 'First-read discovery',
      kind: 'metric',
      display: 'bar',
    });
    expect(bars?.empty).toBe('No activation event configured');
  });
});

describe('analysisSeries', () => {
  it('projects the daily active-people trend', () => {
    const series = analysisSeries(res(), { key: 'trend', metric: 'active_users_daily', title: 'Active devices per day', kind: 'metric', display: 'area' });
    expect(series?.points).toEqual([{ label: '2026-09-05', value: 180 }]);
  });

  it('projects the daily sessions trend', () => {
    const series = analysisSeries(res(), { key: 'sessions-daily', metric: 'sessions_daily', title: 'Sessions per day', kind: 'metric', display: 'area' });
    expect(series?.points).toEqual([{ label: '2026-09-05', value: 410 }]);
  });
});

describe('UNSERVED_TILES', () => {
  it('names Apple-shaped metrics that AgentRay does not compute', () => {
    expect(UNSERVED_TILES.acquisition.map((t) => t.label)).toEqual([
      'Redownloads', 'Conversion rate', 'Impressions / day', 'Product page views', 'Updates',
    ]);
    // Paying users and download→paid are served metrics now — only the
    // purchase-count tile stays unserved.
    expect(UNSERVED_TILES.monetization.map((t) => t.label)).toEqual(['In-app purchases / day']);
    expect(UNSERVED_TILES.usage.map((t) => t.label)).toEqual([
      'Average retention D14', 'Crashes by app version', 'Deletions',
    ]);
  });
});
