import { describe, expect, it } from 'vitest';
import type { Chart } from '@/lib/api';
import { parseBoardEvidence, resolveEvidenceState, type ChartEvidenceStatus } from './evidence-panel';

const chart = { id: 'chart-1' } as Chart;
const sync = (state: 'ready' | 'syncing' | 'stale' | 'error' | 'incomplete' | 'not_configured') => ({ readiness: { state } }) as never;

describe('dashboard evidence', () => {
  it('parses declared evidence but never invents coverage', () => {
    expect(parseBoardEvidence('').coverage).toBe('Coverage not verified');
    const parsed = parseBoardEvidence('definition: Gross completed topups\nunit: VND\ncoverage: 2026-09-01 through 2026-10-03\nrecipe_ref: lohi-evidence-v1/R01');
    expect(parsed).toMatchObject({ definition: 'Gross completed topups', unit: 'VND', recipeRef: 'lohi-evidence-v1/R01' });
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

  it('requires every configured source and execution to be queryable', () => {
    expect(resolveEvidenceState({ loading: false, denied: false, charts: [chart], syncs: [sync('ready'), sync('not_configured')], chartStatuses: ['ready'] })).toBe('empty');
    expect(resolveEvidenceState({ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], chartStatuses: ['empty'] })).toBe('empty');
  });
});
