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
