import { describe, expect, it } from 'vitest';
import type { OverviewResult } from '@/lib/api';
import { freshnessLabel } from './page';

// freshnessLabel must age from the absolute receipt timestamp, not the cached
// age or client occurrence time — delayed/offline events still prove capture
// has resumed.
function res(lastEventAt: string | undefined, lastReceivedAt: string | undefined, state: 'fresh' | 'quiet' | 'no_events' = 'fresh'): OverviewResult {
  return {
    data_status: {
      last_event_at: lastEventAt,
      last_received_at: lastReceivedAt,
      age_seconds: 0, // deliberately stale cache — the label must ignore it
      pipeline_lag: 'unavailable',
      schema_status: 'unavailable',
      events_in_range: 0,
      qualifying_in_range: 0,
      ever_received: !!lastReceivedAt,
      state,
      sources: [],
      sources_truncated: false,
    },
  } as OverviewResult;
}

describe('freshnessLabel', () => {
  const now = new Date('2026-09-12T12:00:00Z').getTime();

  it('prints the absolute receipt timestamp — honest without a re-render', () => {
    // A relative label ("2 min ago") or a "Live" claim goes stale on an
    // untouched tab that never re-renders; the absolute stamp is true
    // whenever it is read, so it is the only wording used.
    expect(freshnessLabel(res('2026-09-01T00:00:00Z', '2026-09-12T11:59:30Z'), now).text).toBe('Last received 2026-09-12 11:59 UTC');
    expect(freshnessLabel(res('2026-09-01T00:00:00Z', '2026-09-12T11:50:00Z'), now).text).toBe('Last received 2026-09-12 11:50 UTC');
  });

  it('ages the receipt instead of trusting the cached age or occurrence time', () => {
    // This occurrence is two days old, but it was received moments ago after an
    // offline client reconnected: source freshness remains fresh.
    const l = freshnessLabel(res('2026-09-10T12:00:00Z', '2026-09-12T11:59:00Z'), now);
    expect(l.text).toBe('Last received 2026-09-12 11:59 UTC');
    expect(l.stale).toBe(false);
  });

  it('reports no capture receipts honestly', () => {
    const l = freshnessLabel(res(undefined, undefined, 'no_events'), now);
    expect(l.text).toBe('No capture receipts yet');
    expect(l.stale).toBe(true);
  });
});
