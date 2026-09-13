import { describe, expect, it } from 'vitest';
import { APIError, type AgentRecommendation, type OverviewResult } from '@/lib/api';
import { bestNextStep, freshnessLabel, overviewViewState } from './page';

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

// bestNextStep is the value-first panel's single decision: show the finding
// only when it is display-complete, otherwise explain the capability. A row
// that is missing its evidence envelope or its rationale cannot be presented
// as an evidence-backed finding — the comparison would be a guess.
function finding(over: Partial<AgentRecommendation> = {}): AgentRecommendation {
  return {
    id: 'f1',
    project_id: 'p1',
    category: 'activation',
    title: 'Activation fell 12% week over week',
    rationale: 'Signup → first project dropped from 41% to 29%.',
    evidence_json: JSON.stringify({ query_ref: 'activation_funnel', range: 'last 7 days', metric_version: 'v3' }),
    impact_score: 9,
    status: 'open',
    ack_note: '',
    created_at: '2026-09-12T00:00:00Z',
    seen_count: 1,
    last_seen_at: '2026-09-12T00:00:00Z',
    revision: 1,
    ...over,
  };
}

describe('bestNextStep', () => {
  it('surfaces the open finding with its comparison and evidence line', () => {
    const step = bestNextStep(finding());
    expect(step.kind).toBe('finding');
    if (step.kind !== 'finding') return;
    expect(step.title).toBe('Activation fell 12% week over week');
    expect(step.observation).toContain('41% to 29%');
    expect(step.evidence).toContain('activation_funnel');
    expect(step.evidence).toContain('metric v3');
  });

  it('falls back to the capability explanation when the finding is not display-complete', () => {
    // A legacy row with no evidence envelope: the provenance line would read
    // "evidence unavailable", so the panel must not call it a finding.
    expect(bestNextStep(finding({ evidence_json: '' })).kind).toBe('capability');
    expect(bestNextStep(finding({ evidence_json: '{not json' })).kind).toBe('capability');
    // An envelope full of unrelated keys is not provenance either: evidenceLine
    // renders "evidence unavailable" for it, and the panel must agree with the
    // line it would print rather than with the fact that JSON parsed.
    expect(bestNextStep(finding({ evidence_json: JSON.stringify({ events: 202, sessions: 8, window_hours: 24 }) })).kind).toBe('capability');
    expect(bestNextStep(finding({ rationale: '   ' })).kind).toBe('capability');
    expect(bestNextStep(finding({ title: '' })).kind).toBe('capability');
    // Only an open finding is a next step; a dismissed one is history.
    expect(bestNextStep(finding({ status: 'dismissed' })).kind).toBe('capability');
    expect(bestNextStep(null).kind).toBe('capability');
    // A failed Plans read degrades this panel alone.
    expect(bestNextStep(finding(), true)).toEqual({ kind: 'capability', reason: 'unavailable' });
  });
});
