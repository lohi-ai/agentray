import { describe, expect, it } from 'vitest';
import type { InsightResult } from '@/lib/api';
import { formatFractionAsPercent } from '@/lib/format';
import { headlineStats } from './headline';

function funnelInsight(steps: InsightResult['funnel']): InsightResult {
  return {
    type: 'funnel',
    title: 'Drop-off',
    metric: 'funnel',
    series: [],
    rows: [],
    funnel: steps,
    retention: [],
    generated_at: '2026-08-20T00:00:00Z',
  };
}

describe('Product funnel conversion', () => {
  it('renders 13% for a 6-of-45 funnel, not 0%', () => {
    const stats = headlineStats(funnelInsight([
      { step: 0, event_name: 'user.pageview', users: 45, conversion: 1 },
      { step: 1, event_name: 'user.conversion', users: 6, conversion: 6 / 45 },
    ]));
    expect(stats).toEqual(expect.arrayContaining([
      { label: 'Entered', value: '45' },
      { label: 'Completed', value: '6' },
      expect.objectContaining({ label: 'Conversion', value: '13%', tone: 'danger' }),
    ]));
    // The store fraction 6/45, printed as a percent — the 0% this used to
    // show is the same class of lie formatRate already forbids.
    expect(formatFractionAsPercent(6 / 45, 0)).toBe('13%');
  });

  it('prints 100% on the first step, not 1%', () => {
    expect(formatFractionAsPercent(1, 0)).toBe('100%');
  });
});

function retentionInsight(points: InsightResult['retention']): InsightResult {
  return {
    type: 'retention',
    metric: 'users',
    title: 'Retention',
    series: [],
    rows: [],
    funnel: [],
    retention: points,
    generated_at: '2026-08-20T00:00:00Z',
  };
}

describe('Product retention headline', () => {
  // The defect: on the default 24-hour range every weekly period is in the
  // future, so the curve was 100% then eight zeros — and this averaged all nine
  // into a confident "Avg retention 11%, Best period 100%" for a question the
  // data could not answer.
  it('refuses to average periods that have not elapsed', () => {
    const stats = headlineStats(retentionInsight([
      { period: 'Week 0', users: 90, rate: 1, mature: true },
      ...Array.from({ length: 8 }, (_, i) => ({
        period: `Week ${i + 1}`, users: 0, rate: 0, mature: false,
      })),
    ]));
    expect(stats).toEqual(expect.arrayContaining([
      { label: 'Measured periods', value: '0' },
      { label: 'Avg retention', value: 'Not enough history' },
      { label: 'Cohort', value: '90' },
    ]));
    expect(JSON.stringify(stats)).not.toMatch(/11%|100%/);
  });

  // Week 0 is the cohort, not a measurement. Averaging its definitional 100% in
  // props every curve up by a period's worth of nothing.
  it('excludes the definitional Week 0 from the average', () => {
    const stats = headlineStats(retentionInsight([
      { period: 'Week 0', users: 100, rate: 1, mature: true },
      { period: 'Week 1', users: 20, rate: 0.2, mature: true },
      { period: 'Week 2', users: 10, rate: 0.1, mature: true },
    ]));
    expect(stats).toEqual(expect.arrayContaining([
      { label: 'Measured periods', value: '2' },
      { label: 'Avg retention', value: '15%' },
      expect.objectContaining({ label: 'Best period', value: '20%' }),
    ]));
  });

  it('averages only the elapsed periods when the curve is partly mature', () => {
    const stats = headlineStats(retentionInsight([
      { period: 'Week 0', users: 100, rate: 1, mature: true },
      { period: 'Week 1', users: 40, rate: 0.4, mature: true },
      { period: 'Week 2', users: 0, rate: 0, mature: false },
      { period: 'Week 3', users: 0, rate: 0, mature: false },
    ]));
    expect(stats).toEqual(expect.arrayContaining([
      { label: 'Measured periods', value: '1' },
      { label: 'Avg retention', value: '40%' },
    ]));
  });
});
