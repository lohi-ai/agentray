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

  it('prints the absolute last-event timestamp — honest without a re-render', () => {
    // A relative label ("2 min ago") or a "Live" claim goes stale on an
    // untouched tab that never re-renders; the absolute stamp is true
    // whenever it is read, so it is the only wording used.
    expect(freshnessLabel(res('2026-09-12T11:59:30Z'), now).text).toBe('Last event 2026-09-12 11:59 UTC');
    expect(freshnessLabel(res('2026-09-12T11:50:00Z'), now).text).toBe('Last event 2026-09-12 11:50 UTC');
  });

  it('ages a cached snapshot instead of trusting age_seconds', () => {
    // age_seconds says 0 (fresh at fetch time) but the event is 2 days old —
    // the stale flag must reflect the absolute timestamp, not the cached age.
    const l = freshnessLabel(res('2026-09-10T12:00:00Z'), now);
    expect(l.text).toBe('Last event 2026-09-10 12:00 UTC');
    expect(l.stale).toBe(true);
  });

  it('reports no events honestly', () => {
    const l = freshnessLabel(res(undefined, 'no_events'), now);
    expect(l.text).toBe('No events received yet');
    expect(l.stale).toBe(true);
  });
});
