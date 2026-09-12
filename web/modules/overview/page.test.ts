import { describe, expect, it } from 'vitest';
import { APIError, type OverviewResult } from '@/lib/api';
import { freshnessLabel, overviewViewState } from './page';

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
  } as unknown as OverviewResult;
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

// overviewViewState is the single mutually-exclusive state the page renders.
// Each case pins one transition of the contract: loading retains layout, a
// 403 names missing access, other failures retry, first-run wins over
// receipt-only, and an active filter turns "nothing arrived" into a reset
// offer instead of a bare empty state.
function overviewRes(over: {
  events?: number;
  qualifying?: number;
  everReceived?: boolean;
}): OverviewResult {
  return {
    data_status: {
      events_in_range: over.events ?? 0,
      qualifying_in_range: over.qualifying ?? 0,
      ever_received: over.everReceived ?? true,
      state: 'fresh',
      sources: [],
      sources_truncated: false,
      pipeline_lag: 'unavailable',
      schema_status: 'unavailable',
    },
  } as unknown as OverviewResult;
}

const baseInput = {
  projectID: 'p1',
  isLoading: false,
  error: null,
  res: null as OverviewResult | null,
  showFirstEvent: false,
  platform: '',
  period: '7d',
};

describe('overviewViewState', () => {
  it('keeps the loading layout until the first result lands', () => {
    expect(overviewViewState({ ...baseInput, isLoading: true })).toBe('loading');
    expect(overviewViewState({ ...baseInput, projectID: undefined })).toBe('loading');
  });

  it('names missing access on 403 instead of offering a blind retry', () => {
    expect(overviewViewState({ ...baseInput, error: new APIError(403, 'forbidden') })).toBe('no_access');
    expect(overviewViewState({ ...baseInput, error: new APIError(500, 'boom') })).toBe('error');
    expect(overviewViewState({ ...baseInput, error: new Error('network') })).toBe('error');
  });

  it('treats a never-received or verification-only project as first run', () => {
    expect(overviewViewState({ ...baseInput, res: overviewRes({ everReceived: false }) })).toBe('first_run');
    // A verification receipt arrived but no product event: still first run,
    // not receipt_only — the guide stays up until real activity lands.
    expect(overviewViewState({ ...baseInput, res: overviewRes({ events: 1 }), showFirstEvent: true })).toBe('first_run');
  });

  it('splits receipt-only from filtered-empty from bare empty', () => {
    expect(overviewViewState({ ...baseInput, res: overviewRes({ events: 3 }) })).toBe('receipt_only');
    expect(overviewViewState({ ...baseInput, res: overviewRes({}), platform: 'ios' })).toBe('filtered_empty');
    expect(overviewViewState({ ...baseInput, res: overviewRes({}), period: 'today' })).toBe('filtered_empty');
    expect(overviewViewState({ ...baseInput, res: overviewRes({}) })).toBe('empty');
  });

  it('reports data when qualifying events exist in range', () => {
    expect(overviewViewState({ ...baseInput, res: overviewRes({ events: 5, qualifying: 2 }) })).toBe('data');
  });
});
