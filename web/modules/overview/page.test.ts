import { describe, expect, it } from 'vitest';
import { APIError, type AgentRecommendation, type ListFindingsResult, type OverviewMetric, type OverviewResult, type OverviewRevenueDetail } from '@/lib/api';
import { acquisitionStats, bestNextStep, firstEvidenceBackedFinding, freshnessLabel, metricTile, overviewViewState, rangeLabel, retentionTile, revenueBreakdownRows, revenueTile, sourcePill, tileProvenance } from './page';

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

describe('freshnessLabel served verdict', () => {
  const now = new Date('2026-09-12T12:00:00Z').getTime();

  it('trusts the served verdict over the client clock', () => {
    // The stale modifier's trigger is data_status.state == 'quiet' (spec
    // §States). A packet the server judged quiet must render stale even when
    // the client clock says the receipt is young — the served verdict is the
    // contract, the age check is only the fallback for a stale cached packet.
    const l = freshnessLabel(res('2026-09-12T11:59:00Z', '2026-09-12T11:59:00Z', 'quiet'), now);
    expect(l.stale).toBe(true);
    expect(l.word).toBe('Quiet');
  });

  it('still flags a receipt older than the quiet threshold when the served state is fresh', () => {
    const l = freshnessLabel(res('2026-09-10T12:00:00Z', '2026-09-10T12:00:00Z', 'fresh'), now);
    expect(l.stale).toBe(true);
    expect(l.word).toBe('Quiet');
  });

  it('names the freshness word the StatusPill renders', () => {
    expect(freshnessLabel(res('2026-09-12T11:59:00Z', '2026-09-12T11:59:00Z', 'fresh'), now).word).toBe('Data fresh');
    expect(freshnessLabel(res(undefined, undefined, 'no_events'), now).word).toBe('No capture receipts yet');
  });
});

describe('sourcePill', () => {
  it('maps every served source state to a word + tone, never color alone', () => {
    expect(sourcePill('healthy')).toEqual({ status: 'healthy', label: 'Healthy' });
    expect(sourcePill('partial')).toEqual({ status: 'attention', label: 'Partial data' });
    expect(sourcePill('error')).toEqual({ status: 'attention', label: 'Needs attention' });
    expect(sourcePill('paused')).toEqual({ status: 'paused', label: 'Paused' });
    expect(sourcePill('not_ready')).toEqual({ status: 'idle', label: 'Not run yet' });
    expect(sourcePill('not_configured')).toEqual({ status: 'idle', label: 'Set up a table' });
  });
});

describe('rangeLabel', () => {
  it('prints the tile range format with the project timezone', () => {
    // The header sub must match the tiles' own range wording — the spec's
    // "Sep 5–11 · 7 complete days · Asia/Ho_Chi_Minh", not raw ISO instants.
    expect(rangeLabel(servedRes())).toBe('Sep 5–11 · 7 complete days · Asia/Ho_Chi_Minh');
  });

  it('names a UTC fallback instead of implying a project timezone', () => {
    expect(rangeLabel(servedRes({ timezoneSource: 'fallback' }))).toBe('Sep 5–11 · 7 complete days · UTC fallback — no project timezone set');
  });

  it('says a partial day carries no comparison', () => {
    expect(rangeLabel(servedRes({ completeDays: false }))).toBe('Today so far · Asia/Ho_Chi_Minh · partial day, no comparison');
  });
});

describe('acquisitionStats', () => {
  it('leads the Acquisition group with the New people tile', () => {
    const stats = acquisitionStats(servedRes());
    expect(stats.map((s) => s.label)).toEqual(['New people']);
    expect(stats[0].provenance).toContain('metric overview.v3');
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

  it('falls back to the capability branch when no display-complete finding exists', () => {
    // §Value-first story: no complete finding → a capability explanation,
    // never an omitted panel and never a fabricated number.
    expect(bestNextStep(undefined)).toEqual({ kind: 'capability', reason: 'no_finding' });
    expect(bestNextStep([])).toEqual({ kind: 'capability', reason: 'no_finding' });
    // A failed Plans read degrades this panel alone — still the capability
    // branch, with its own reason.
    expect(bestNextStep([finding()], true)).toEqual({ kind: 'capability', reason: 'unavailable' });
  });

  it('names an incomplete finding rather than pretending none exists', () => {
    // A legacy row with no evidence envelope: the provenance line would read
    // "evidence unavailable", so the dashboard must not call it a finding —
    // but rows DID arrive, so the reason is incomplete_finding, not no_finding.
    expect(bestNextStep([finding({ evidence_json: '' })])).toEqual({ kind: 'capability', reason: 'incomplete_finding' });
    expect(bestNextStep([finding({ evidence_json: '{not json' })])).toEqual({ kind: 'capability', reason: 'incomplete_finding' });
    // An envelope full of unrelated keys is not provenance either: evidenceLine
    // renders "evidence unavailable" for it, and the dashboard must agree.
    expect(bestNextStep([finding({ evidence_json: JSON.stringify({ events: 202, sessions: 8, window_hours: 24 }) })])).toEqual({ kind: 'capability', reason: 'incomplete_finding' });
    expect(bestNextStep([finding({ rationale: '   ' })])).toEqual({ kind: 'capability', reason: 'incomplete_finding' });
    expect(bestNextStep([finding({ title: '' })])).toEqual({ kind: 'capability', reason: 'incomplete_finding' });
    // Only an open finding is a next step; a dismissed one is history.
    expect(bestNextStep([finding({ status: 'dismissed' })])).toEqual({ kind: 'capability', reason: 'incomplete_finding' });
    // A real finding later in the ranked list still wins over the broken row.
    expect(bestNextStep([finding({ evidence_json: '' }), finding({ id: 'f2', impact_score: 8 })]).kind).toBe('finding');
  });

  it('carries the project goal through the no_finding branch only', () => {
    // The stored goal rewrites the no-finding capability copy; it must not
    // leak into the other reasons or suppress a real finding.
    expect(bestNextStep(undefined, false, 'retention')).toEqual({ kind: 'capability', reason: 'no_finding', goal: 'retention' });
    expect(bestNextStep([finding({ evidence_json: '' })], false, 'retention')).toEqual({ kind: 'capability', reason: 'incomplete_finding', goal: 'retention' });
    expect(bestNextStep(undefined, true, 'retention')).toEqual({ kind: 'capability', reason: 'unavailable' });
    // 'skipped' means the owner declined — the branch behaves as if unset.
    expect(bestNextStep(undefined, false, 'skipped')).toEqual({ kind: 'capability', reason: 'no_finding' });
    expect(bestNextStep([finding()], false, 'retention').kind).toBe('finding');
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

// The served money block for a VND-headline project: a booking that was partly
// reversed, a separate USD row that must never be added to it, and one legacy
// LT row the read excluded as a platform credit rather than money.
function moneyDetail(over: Partial<OverviewRevenueDetail> = {}): OverviewRevenueDetail {
  return {
    window: { from: '2026-09-04T17:00:00Z', to: '2026-09-11T17:00:00Z', days: 7, complete_days: true },
    currency: 'VND',
    gross: 80000,
    reversed: 30000,
    net: 50000,
    previous_net: 40000,
    deduped_rows: 3,
    excluded_rows: 1,
    excluded_currencies: ['LT'],
    by_currency: [
      { currency: 'VND', gross: 80000, reversed: 30000, net: 50000, rows: 2 },
      { currency: 'USD', gross: 100, reversed: 0, net: 100, rows: 1 },
    ],
    ...over,
  };
}

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
  revenueState?: OverviewMetric['state'];
  revenueNotes?: string[];
  revenueDetail?: OverviewRevenueDetail | null;
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
      metric_version: 'overview.v3',
    },
    metrics: {
      active_users: metric('ok'),
      new_users: metric('ok'),
      sessions: metric('ok'),
      activation: metric('unconfigured', ['no activation condition is stored for projects yet — configure it before this metric can compute']),
      revenue: metric(over.revenueState ?? 'ok', over.revenueNotes),
      revenue_detail: over.revenueDetail === undefined ? moneyDetail() : over.revenueDetail,
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
      'metric overview.v3 · Sep 5–11 · 7 complete days · Asia/Ho_Chi_Minh · coverage 120 of 400 events in range · Last received 2026-09-12 09:59 UTC',
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
    expect(tileProvenance(servedRes(), { kind: 'metric', metric: servedRes().metrics.activation })).toContain('no activation condition is stored');
    // A project whose money source is still unconfigured keeps the served
    // reason on the tile rather than a coverage count it never measured.
    const unconfigured = servedRes({ revenueState: 'unconfigured', revenueNotes: ['no trusted deduplicated revenue source exists'], revenueDetail: null });
    expect(tileProvenance(unconfigured, { kind: 'metric', metric: unconfigured.metrics.revenue })).toContain('no trusted deduplicated revenue source exists');
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

// The revenue tile is the money contract's face: a signed net in the currency
// its sender declared, and a provenance line carrying the deduplicated rows the
// arithmetic actually ran on — not the page's event count. Every case below
// pins a reading that would otherwise become a false claim: a bare number with
// no currency, a net reversal rendered as "No data", a measured zero rendered
// as "no data", or an exclusion that disappears from the coverage line.
describe('revenue tile', () => {
  const ok = (value?: number): OverviewMetric => ({ state: 'ok', ...(value === undefined ? {} : { value }), definition: 'Measured by AgentRay.' });

  it('prints the signed net with the currency its sender declared', () => {
    // No prior window in this fixture, so the reading is exactly this pair.
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: 0 }))).toEqual({ label: 'Net revenue', value: '50,000 VND' });
  });

  it('shows a net reversal as the signed negative it is, never as missing data', () => {
    // OverviewMetric.value is unsigned, so a refund-only window has no
    // `value` at all — reading the tile off it would print "No data" over a
    // measured loss.
    const reversed = moneyDetail({ gross: 0, reversed: 30, net: -30, deduped_rows: 1, by_currency: [{ currency: 'VND', gross: 0, reversed: 30, net: -30, rows: 1 }] });
    expect(revenueTile(ok(), reversed).value).toBe('\u221230 VND');
  });

  it('keeps a measured zero distinct from no data', () => {
    const zero = moneyDetail({ gross: 0, reversed: 0, net: 0, deduped_rows: 1, by_currency: [{ currency: 'VND', gross: 0, reversed: 0, net: 0, rows: 1 }] });
    expect(revenueTile(ok(0), zero).value).toBe('0 VND');
    expect(revenueTile({ state: 'no_data', definition: '' }, moneyDetail({ currency: undefined, gross: 0, reversed: 0, net: 0, deduped_rows: 0, excluded_rows: 0, by_currency: [] })).value).toBe('No data');
  });

  it('never prints a bare amount when the unit is missing', () => {
    // An older server can answer `ok` without the money detail. A number with
    // no currency cannot be read as money, so the tile says so instead.
    expect(revenueTile(ok(50000), null).value).toBe('Not available');
    expect(revenueTile({ state: 'unconfigured', definition: '', notes: ['no trusted deduplicated revenue source exists'] }, null).value).toBe('Set up');
  });

  it('compares against the same currency’s previous net, and only over a positive base', () => {
    // The page's design contract gives an `ok` headline tile a delta. The base
    // is the previous window's net in the SAME currency — there is no FX — and
    // a zero or reversal base is not a percentage: metricTile's rule, applied
    // where the signed figure lives.
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: 40000 })).delta).toBe('+25%');
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: 40000 })).deltaTone).toBe('up');
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: 100000 })).delta).toBe('-50%');
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: 100000 })).deltaTone).toBe('down');
    // A window with no prior money, or a prior reversal, has no comparison —
    // and a signed net still renders its absolute value beside it.
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: undefined })).delta).toBeUndefined();
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: 0 })).delta).toBeUndefined();
    expect(revenueTile(ok(50000), moneyDetail({ previous_net: -30 })).delta).toBeUndefined();
    // A signed net keeps its absolute reading and still gets the comparison:
    // reversals exceeded bookings by 130% of last window's net.
    const reversed = moneyDetail({ gross: 0, reversed: 30, net: -30, previous_net: 100, by_currency: [{ currency: 'VND', gross: 0, reversed: 30, net: -30, rows: 1 }] });
    expect(revenueTile(ok(), reversed)).toMatchObject({ value: '\u221230 VND', delta: '-130%', deltaTone: 'down' });
  });

  it('reports the money read’s own coverage, not the page’s event count', () => {
    const r = servedRes();
    const line = tileProvenance(r, { kind: 'money', metric: r.metrics.revenue, detail: r.metrics.revenue_detail });
    expect(line).toContain('3 deduplicated rows, 1 excluded');
    expect(line).not.toContain('400 events');
    expect(line).toContain('metric overview.v3 · Sep 5–11 · 7 complete days');
  });

  it('says zero valid rows when nothing qualified, and names the exclusion', () => {
    const empty = moneyDetail({ currency: undefined, gross: 0, reversed: 0, net: 0, deduped_rows: 0, excluded_rows: 2, by_currency: [] });
    const r = servedRes({ revenueState: 'no_data', revenueDetail: empty });
    expect(tileProvenance(r, { kind: 'money', metric: r.metrics.revenue, detail: empty })).toContain('0 valid money rows, 2 excluded');
  });

  it('never claims page-event coverage for a money tile', () => {
    // Without the money detail there is no money population to report. The
    // page's event coverage is a different tile's input and must not stand in
    // for it; an unconfigured project still gets its served reason.
    const r = servedRes({ revenueState: 'ok', revenueDetail: null });
    expect(tileProvenance(r, { kind: 'money', metric: r.metrics.revenue, detail: null })).toContain('money coverage not reported');
    expect(tileProvenance(r, { kind: 'money', metric: r.metrics.revenue, detail: null })).not.toContain('400 events');
    const unconfigured = servedRes({ revenueState: 'unconfigured', revenueNotes: ['no trusted deduplicated revenue source exists'], revenueDetail: null });
    expect(tileProvenance(unconfigured, { kind: 'money', metric: unconfigured.metrics.revenue, detail: null })).toContain('no trusted deduplicated revenue source exists');
  });

  it('lists one row per declared currency, with the headline marked', () => {
    // The rows are the only comparable set: the tile never adds VND to USD.
    expect(revenueBreakdownRows(moneyDetail())).toEqual([
      { currency: 'VND', gross: '80,000', reversed: '30,000', net: '50,000', headline: true },
      { currency: 'USD', gross: '100', reversed: '0', net: '100', headline: false },
    ]);
    expect(revenueBreakdownRows(moneyDetail({ currency: 'USD' }))[1]?.headline).toBe(true);
    expect(revenueBreakdownRows(null)).toEqual([]);
  });
});
