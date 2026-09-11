import { describe, expect, it } from 'vitest';
import type { OverviewResult } from '@/lib/api';
import { freshnessLabel } from './page';

// freshnessLabel must age from the absolute last_event_at, not the cached
// age_seconds — a cached snapshot would claim "Live" forever on an open page.
function res(lastEventAt: string | undefined, state: 'fresh' | 'quiet' | 'no_events' = 'fresh'): OverviewResult {
  return {
    data_status: {
      last_event_at: lastEventAt,
      age_seconds: 0, // deliberately stale cache — the label must ignore it
      pipeline_lag: 'unavailable',
      events_in_range: 0,
      qualifying_in_range: 0,
      ever_received: !!lastEventAt,
      state,
    },
  } as OverviewResult;
}

describe('freshnessLabel', () => {
  const now = new Date('2026-09-12T12:00:00Z').getTime();

  it('says Live only while the last event is under two minutes old', () => {
    expect(freshnessLabel(res('2026-09-12T11:59:30Z'), now).text).toContain('Live');
    expect(freshnessLabel(res('2026-09-12T11:50:00Z'), now).text).toBe('Last event 10 min ago');
  });

  it('ages a cached snapshot instead of trusting age_seconds', () => {
    // age_seconds says 0 (fresh at fetch time) but the event is 2 days old —
    // the label must reflect the absolute timestamp, not the cached age.
    const l = freshnessLabel(res('2026-09-10T12:00:00Z'), now);
    expect(l.text).toBe('Last event 2 d ago');
    expect(l.stale).toBe(true);
  });

  it('reports no events honestly', () => {
    const l = freshnessLabel(res(undefined, 'no_events'), now);
    expect(l.text).toBe('No events received yet');
    expect(l.stale).toBe(true);
  });
});
