import { describe, expect, it } from 'vitest';
import type { Chart } from '@/lib/api';
import { chartEvidenceStatus, parseSavedLimitation, queryLimitation, resolveEvidenceState, type ChartEvidenceStatus } from './evidence-panel';

const chart = { id: 'chart-1' } as Chart;
const sync = (state: 'ready' | 'syncing' | 'stale' | 'error' | 'incomplete' | 'not_configured') => ({ readiness: { state } }) as never;

describe('dashboard evidence', () => {
  it('parses only the saved limitation aliases and preserves the fallback', () => {
    expect(parseSavedLimitation('')).toBe('No limitation was supplied with this saved board.');
    expect(parseSavedLimitation('definition: Gross completed topups\nlimitations: Excludes refunds')).toBe('Excludes refunds');
    expect(parseSavedLimitation('{"limitation":"Delayed settlements"}')).toBe('Delayed settlements');
    expect(parseSavedLimitation('{" LIMITATIONS ":"Delayed settlements"}')).toBe('Delayed settlements');
  });

  it.each([
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: ['ready'] }, 'ready'],
    [{ loading: true, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: ['ready'] }, 'syncing'],
    [{ loading: false, denied: false, charts: [], syncs: [], chartStatuses: [] }, 'empty'],
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('stale')], chartStatuses: ['ready'] }, 'stale'],
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('incomplete')], chartStatuses: ['ready'] }, 'error'],
    [{ loading: false, denied: true, charts: [chart], syncs: [sync('ready')], chartStatuses: ['ready'] }, 'read-only-denied'],
    [{ loading: false, denied: false, readinessError: true, charts: [chart], syncs: [sync('ready')], chartStatuses: ['ready'] }, 'readiness-error'],
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: ['denied'] }, 'query-denied'],
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: ['ready'], cohortEligibility: 'Not ready · cohort under 14 days' }, 'immature'],
  ] as const)('resolves every required evidence state', (input, expected) => {
    expect(resolveEvidenceState(input)).toBe(expected);
  });

  it.each(['error', 'unsupported', 'capacity', 'non_plottable'] satisfies ChartEvidenceStatus[])(
    'never reports ready when execution is %s',
    (status) => {
      expect(resolveEvidenceState({ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: [status] })).toBe('error');
    },
  );

  it.each(['bounded', 'unknown'] satisfies ChartEvidenceStatus[])(
    'never reports ready when execution completeness is %s',
    (status) => {
      expect(resolveEvidenceState({ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: [status] })).toBe('error');
    },
  );

  it('maps bounded and unavailable query metadata to honest evidence statuses and limitations', () => {
    const baseMeta = {
      query_ref: 'query/1', query_digest: 'digest', executed_at: '2026-10-04T00:00:00Z',
      serving_data_watermark: null, truncated: null, availability_reason: null,
    } as const;
    const boundedMeta = { ...baseMeta, result_completeness: 'bounded' as const };
    expect(chartEvidenceStatus('ready', boundedMeta)).toBe('bounded');
    expect(queryLimitation({ status: 'bounded', filterKey: 'filters', meta: boundedMeta }, 'saved')).toMatch(/bounded.*additional rows/i);

    const unknownMeta = { ...baseMeta, result_completeness: 'unknown' as const, availability_reason: 'query_evidence_unavailable' };
    expect(chartEvidenceStatus('ready', unknownMeta)).toBe('unknown');
    expect(chartEvidenceStatus('ready')).toBe('unknown');
    expect(queryLimitation({ status: 'unknown', filterKey: 'filters', meta: unknownMeta }, 'saved')).toMatch(/completeness is unknown.*unavailable/i);
  });

  it('requires every configured source and execution to be queryable', () => {
    expect(resolveEvidenceState({ loading: false, denied: false, charts: [chart], syncs: [sync('ready'), sync('not_configured')], chartStatuses: ['ready'] })).toBe('empty');
    expect(resolveEvidenceState({ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: ['empty'] })).toBe('empty');
  });
});
