import { describe, expect, it } from 'vitest';
import { APIError, type AgentRecommendation, type ListFindingsResult, type OverviewMetric, type OverviewResult } from '@/lib/api';
import { bestNextStep, firstEvidenceBackedFinding, freshnessLabel, metricTile, overviewViewState, retentionTile, tileProvenance } from './page';

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

// bestNextStep shows a finding only when it is evidence-complete. A row that
// lacks its envelope or rationale is omitted rather than converted into a
// generic CTA or a fabricated recommendation.
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
    const step = bestNextStep([finding()]);
    expect(step.kind).toBe('finding');
    if (step.kind !== 'finding') return;
    expect(step.title).toBe('Activation fell 12% week over week');
    expect(step.observation).toContain('41% to 29%');
    expect(step.evidence).toContain('activation_funnel');
    expect(step.evidence).toContain('metric v3');
  });

  it('omits a finding that is not display-complete', () => {
    // A legacy row with no evidence envelope: the provenance line would read
    // "evidence unavailable", so the dashboard must not call it a finding.
    expect(bestNextStep([finding({ evidence_json: '' })]).kind).toBe('none');
    expect(bestNextStep([finding({ evidence_json: '{not json' })]).kind).toBe('none');
    // An envelope full of unrelated keys is not provenance either: evidenceLine
    // renders "evidence unavailable" for it, and the dashboard must agree.
    expect(bestNextStep([finding({ evidence_json: JSON.stringify({ events: 202, sessions: 8, window_hours: 24 }) })]).kind).toBe('none');
    expect(bestNextStep([finding({ rationale: '   ' })]).kind).toBe('none');
    expect(bestNextStep([finding({ title: '' })]).kind).toBe('none');
    // Only an open finding is a next step; a dismissed one is history.
    expect(bestNextStep([finding({ status: 'dismissed' })]).kind).toBe('none');
    expect(bestNextStep(undefined).kind).toBe('none');
    // A failed Plans read degrades this panel alone.
    expect(bestNextStep([finding()], true)).toEqual({ kind: 'none' });
    expect(bestNextStep([finding({ evidence_json: '' }), finding({ id: 'f2', impact_score: 8 })]).kind).toBe('finding');
  });
});

describe('firstEvidenceBackedFinding', () => {
  it('follows the keyset cursor past an incomplete finding', async () => {
    const pages: Record<string, ListFindingsResult> = {
      first: { findings: [finding({ evidence_json: '' })], next_cursor: 'next' },
      next: { findings: [finding({ id: 'f2', impact_score: 8 })] },
    };
    const calls: string[] = [];
    const result = await firstEvidenceBackedFinding(async (cursor) => {
      const key = cursor || 'first';
      calls.push(key);
      return pages[key]!;
    });
    expect(result?.id).toBe('f2');
    expect(calls).toEqual(['first', 'next']);
  });

  it('rejects a repeated cursor instead of looping forever', async () => {
    await expect(firstEvidenceBackedFinding(async () => ({
      findings: [finding({ evidence_json: '' })],
      next_cursor: 'repeat',
    }))).rejects.toThrow('cursor repeated');
  });

  it('stops when the open-first ordering reaches settled history', async () => {
    let calls = 0;
    await expect(firstEvidenceBackedFinding(async () => {
      calls += 1;
      return {
        findings: [finding({ status: 'accepted', evidence_json: '' })],
        next_cursor: 'must-not-fetch',
      };
    })).resolves.toBeNull();
    expect(calls).toBe(1);
  });
});

describe('metricTile', () => {
  const metric = (state: OverviewMetric['state'], value?: number): OverviewMetric => ({
    state,
    ...(value === undefined ? {} : { value }),
    definition: 'Measured by AgentRay.',
  });

  it('keeps configured values and shows honest state labels without fabricating zeroes', () => {
    expect(metricTile('Revenue', metric('unconfigured')).value).toBe('Set up');
    expect(metricTile('Purchases', metric('unavailable')).value).toBe('Not available');
    expect(metricTile('Active people', metric('no_data')).value).toBe('No data');
    expect(metricTile('Sessions', metric('ok', 42)).value).toBe('42');
  });
});

describe('retentionTile', () => {
  it('keeps a measured zero distinct from an immature cohort', () => {
    expect(retentionTile('D1 retention', { state: 'ok', rate: 0, returned: 0, eligible: 8 }).value).toBe('0.0%');
    expect(retentionTile('D7 retention', { state: 'not_ready', rate: 0, returned: 0, eligible: 0 }).value).toBe('Not ready');
  });
});

// tileProvenance is the hard contract: every tile names the metric version, its
// own range, the project timezone, its coverage and its freshness — in that
// order, from served fields only. Each case pins a field that would otherwise
// become a false claim: a range stamped on lifetime cohorts, a version invented
// per group, a coverage borrowed from a metric the operation never computed.
function servedRes(over: {
  qualifying?: number;
  events?: number;
  everReceived?: boolean;
  timezoneSource?: string;
  timezone?: string;
  rangeFrom?: string;
  rangeTo?: string;
  cohortWindow?: string;
  d7Eligible?: number;
  completeDays?: boolean;
} = {}): OverviewResult {
  const metric = (state: OverviewMetric['state'], notes?: string[]): OverviewMetric => ({ state, definition: 'Measured by AgentRay.', notes });
  return {
    context: {
      project_id: 'p1',
      timezone: over.timezone ?? 'Asia/Ho_Chi_Minh',
      timezone_source: over.timezoneSource ?? 'project',
      // Sep 5–11 in the project timezone: the UTC instants are the previous day.
      range: {
        from: over.rangeFrom ?? '2026-09-04T17:00:00Z',
        to: over.rangeTo ?? '2026-09-11T17:00:00Z',
        days: 7,
        complete_days: over.completeDays ?? true,
      },
      previous_range: { from: '2026-08-28T17:00:00Z', to: '2026-09-04T17:00:00Z', days: 7, complete_days: true },
      platform: '',
      generated_at: '2026-09-12T10:00:00Z',
      metric_version: 'overview.v2',
    },
    metrics: {
      active_users: metric('ok'),
      new_users: metric('ok'),
      sessions: metric('ok'),
      activation: metric('unconfigured', ['no activation condition is stored for projects yet — configure it before this metric can compute']),
      revenue: metric('unconfigured', ['no trusted deduplicated revenue source exists — SDK revenue events are not deduplicated at read time']),
    },
    trend: [],
    retention: {
      cohort_window: over.cohortWindow ?? 'lifetime',
      d1: { state: 'ok', rate: 0.4, returned: 4, eligible: 10 },
      d7: { state: 'not_ready', rate: 0, returned: 0, eligible: over.d7Eligible ?? 0 },
      d30: { state: 'not_ready', rate: 0, returned: 0, eligible: 0 },
    },
    content: { top_pages: { unit: 'pageviews', rows: [] }, top_sources: { unit: 'pageviews', rows: [] } },
    data_status: {
      last_event_at: '2026-09-12T09:59:00Z',
      last_received_at: over.everReceived === false ? undefined : '2026-09-12T09:59:00Z',
      pipeline_lag: 'unavailable',
      schema_status: 'unavailable',
      events_in_range: over.events ?? 400,
      qualifying_in_range: over.qualifying ?? 120,
      ever_received: over.everReceived ?? true,
      state: 'fresh',
      sources: [],
      sources_truncated: false,
    },
  } as unknown as OverviewResult;
}

describe('tileProvenance', () => {
  it('names version, range, timezone, coverage and freshness in that order', () => {
    const r = servedRes();
    expect(tileProvenance(r, { kind: 'metric', metric: r.metrics.active_users })).toBe(
      'metric overview.v2 · Sep 5–11 · 7 complete days · Asia/Ho_Chi_Minh · coverage 120 of 400 events in range · Last received 2026-09-12 09:59 UTC',
    );
  });

  it('dates the range in the project timezone, not in UTC', () => {
    // Both boundaries are the previous day in UTC. Slicing the served instants
    // would print Sep 4–11 and claim a day outside the window.
    const line = tileProvenance(servedRes(), { kind: 'metric', metric: { state: 'ok', definition: '' } });
    expect(line).toContain('Sep 5–11');
    expect(line).not.toContain('Sep 4');
  });

  it('steps back one calendar day, not 24 hours, across a DST transition', () => {
    // New York springs forward on 2026-03-08, so that local day is 23 hours
    // long. `to` minus a fixed 24h is Mar 7 23:00 local, which would print
    // "Mar 2–7 · 7 complete days" — a six-day span claiming seven.
    const r = servedRes({
      timezone: 'America/New_York',
      rangeFrom: '2026-03-02T05:00:00Z',
      rangeTo: '2026-03-09T04:00:00Z',
    });
    const line = tileProvenance(r, { kind: 'metric', metric: { state: 'ok', definition: '' } });
    expect(line).toContain('Mar 2–8 · 7 complete days');
    expect(line).not.toContain('Mar 7');
  });

  it('never claims relative freshness', () => {
    // A cached figure cannot honestly say "refreshed 2 min ago" or "Live": the
    // absolute receipt time is true whenever it is read.
    const line = tileProvenance(servedRes(), { kind: 'metric', metric: { state: 'ok', definition: '' } });
    expect(line).toContain('Last received 2026-09-12 09:59 UTC');
    expect(line).not.toMatch(/refreshed|Live|ago\b/);
  });

  it('reports a serving project’s fallback timezone as a fallback', () => {
    const line = tileProvenance(servedRes({ timezoneSource: 'fallback' }), { kind: 'metric', metric: { state: 'ok', definition: '' } });
    expect(line).toContain('UTC fallback — no project timezone set');
  });

  it('compacts a partial day without implying a comparison', () => {
    const line = tileProvenance(servedRes({ completeDays: false }), { kind: 'metric', metric: { state: 'ok', definition: '' } });
    expect(line).toContain('Today so far');
    expect(line).not.toContain('complete days');
  });

  it('carries the served reason instead of a coverage claim for an unconfigured metric', () => {
    const r = servedRes();
    expect(tileProvenance(r, { kind: 'metric', metric: r.metrics.activation })).toContain('no activation condition is stored');
    expect(tileProvenance(r, { kind: 'metric', metric: r.metrics.revenue })).toContain('no trusted deduplicated revenue source exists');
  });

  it('says nothing arrived rather than blaming the metric for it', () => {
    const line = tileProvenance(servedRes({ events: 0, qualifying: 0, everReceived: false }), { kind: 'metric', metric: { state: 'no_data', definition: '' } });
    expect(line).toContain('no events received yet');
    expect(line).toContain('No capture receipts yet');
  });

  it('scopes retention provenance to lifetime cohorts and matured members', () => {
    const r = servedRes({ d7Eligible: 0 });
    expect(tileProvenance(r, { kind: 'retention', day: 1, point: r.retention.d1 })).toContain('lifetime cohorts');
    expect(tileProvenance(r, { kind: 'retention', day: 1, point: r.retention.d1 })).toContain('coverage 10 mature members');
    // An immature cohort has no rate, so it reports why instead of a number.
    expect(tileProvenance(r, { kind: 'retention', day: 7, point: r.retention.d7 })).toContain('no mature 7-day cohort yet');
    expect(tileProvenance(servedRes({ cohortWindow: 'range' }), { kind: 'retention', day: 1, point: { state: 'ok', eligible: 3 } })).toContain('range cohorts');
  });

  it('never borrows event coverage for a metric the operation does not serve', () => {
    // Purchases, Subscriptions and Crashes have no served metric. Showing the
    // page's qualifying-event coverage on them would claim they were measured.
    const line = tileProvenance(servedRes(), { kind: 'unserved' });
    expect(line).toContain('not instrumented — no served metric');
    expect(line).not.toContain('coverage');
  });
});
