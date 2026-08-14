import { describe, expect, it } from 'vitest';
import {
  FREE_EVENT_CEILING,
  PLANS,
  UPGRADE_THRESHOLD,
  formatEvents,
  formatPrice,
  nextPlan,
  planByID,
  usageMeter,
} from './plans';

describe('the plan ladder', () => {
  it('keeps the 1M free ceiling', () => {
    // This one is a competitive floor rather than a preference: PostHog and
    // Mixpanel both give 1M free, so a smaller number reads as unserious.
    expect(FREE_EVENT_CEILING).toBe(1_000_000);
    expect(planByID('free').eventsPerMonth).toBe(1_000_000);
  });

  it('prices every paid tier under its US/EU comparable', () => {
    expect(planByID('solo').usdPerMonth).toBe(19); // Langfuse Core $29, Amplitude Plus $49
    expect(planByID('team').usdPerMonth).toBe(79); // Langfuse Pro $199
  });

  it('reads back free for an unknown or missing plan', () => {
    expect(planByID(undefined).id).toBe('free');
    expect(planByID('enterprise').id).toBe('free');
    expect(planByID(null).id).toBe('free');
  });

  it('walks the ladder and stops at the top', () => {
    expect(nextPlan('free')?.id).toBe('solo');
    expect(nextPlan('solo')?.id).toBe('team');
    expect(nextPlan('team')).toBeNull();
  });

  it('leaves agent runs unlimited on every tier', () => {
    // Runs are BYO-AI-key, so the user pays the token cost. Metering them would
    // charge for something that costs us nothing.
    for (const plan of PLANS) {
      expect(plan.features.join(' ')).not.toMatch(/runs? (?:per|\/) month/i);
    }
  });
});

describe('usageMeter', () => {
  const free = planByID('free');

  it('keeps the bar inside its track when a workspace is over the ceiling', () => {
    const meter = usageMeter(4_000_000, free);
    expect(meter.ratio).toBe(1);
    expect(meter.overCeiling).toBe(true);
  });

  it('treats a missing or nonsense count as zero rather than NaN', () => {
    expect(usageMeter(Number.NaN, free).used).toBe(0);
    expect(usageMeter(-5, free).ratio).toBe(0);
  });

  it('fires the upgrade moment once, at 80%', () => {
    expect(usageMeter(FREE_EVENT_CEILING * 0.79, free).nearCeiling).toBe(false);
    expect(usageMeter(FREE_EVENT_CEILING * UPGRADE_THRESHOLD, free).nearCeiling).toBe(true);
  });

  it('projects the fill date from the month-to-date rate', () => {
    // 500k by the 10th = 50k/day, so 1M lands on day 20.
    const now = new Date(Date.UTC(2026, 7, 10, 12, 0, 0));
    const meter = usageMeter(500_000, free, now);
    expect(meter.projectedFullOn?.getUTCDate()).toBe(20);
  });

  it('has no date to promise when the run rate never reaches the ceiling', () => {
    const now = new Date(Date.UTC(2026, 7, 10, 12, 0, 0));
    expect(usageMeter(1_000, free, now).projectedFullOn).toBeNull();
    expect(usageMeter(0, free, now).projectedFullOn).toBeNull();
  });
});

describe('formatting', () => {
  it('keeps seven-figure counts legible', () => {
    expect(formatEvents(1_240_000)).toBe('1.2M');
    expect(formatEvents(15_000_000)).toBe('15M');
    expect(formatEvents(4_300)).toBe('4.3k');
    expect(formatEvents(812)).toBe('812');
    expect(formatEvents(Number.NaN)).toBe('0');
  });

  it('shows VND only as the localized equivalent', () => {
    expect(formatPrice(planByID('solo'), 'en')).toBe('$19');
    expect(formatPrice(planByID('solo'), 'vi')).toMatch(/499/);
    expect(formatPrice(planByID('free'), 'en')).toBe('Free');
    expect(formatPrice(planByID('free'), 'vi')).toBe('Miễn phí');
  });
});
