import { describe, expect, it } from 'vitest';
import type { Chart } from '@/lib/api';
import { parseBoardEvidence, resolveEvidenceState } from './evidence-panel';

const chart = { id: 'chart-1' } as Chart;
const sync = (state: 'ready' | 'syncing' | 'stale' | 'error' | 'incomplete' | 'not_configured') => ({ readiness: { state } }) as never;

describe('dashboard evidence', () => {
  it('parses declared evidence but never invents coverage', () => {
    expect(parseBoardEvidence('').coverage).toBe('Coverage not verified');
    const parsed = parseBoardEvidence('definition: Gross completed topups\nunit: VND\ncoverage: 2026-09-01 through 2026-10-03\nrecipe_ref: lohi-evidence-v1/R01');
    expect(parsed).toMatchObject({ definition: 'Gross completed topups', unit: 'VND', recipeRef: 'lohi-evidence-v1/R01' });
  });

  it.each([
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], evidence: parseBoardEvidence('') }, 'ready'],
    [{ loading: true, denied: false, charts: [chart], syncs: [sync('ready')], evidence: parseBoardEvidence('') }, 'syncing'],
    [{ loading: false, denied: false, charts: [], syncs: [], evidence: parseBoardEvidence('') }, 'empty'],
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('stale')], evidence: parseBoardEvidence('') }, 'stale'],
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('incomplete')], evidence: parseBoardEvidence('') }, 'error'],
    [{ loading: false, denied: true, charts: [chart], syncs: [sync('ready')], evidence: parseBoardEvidence('') }, 'read-only-denied'],
    [{ loading: false, denied: false, charts: [chart], syncs: [sync('ready')], evidence: parseBoardEvidence('cohort_eligibility: Not ready · cohort under 14 days') }, 'immature'],
  ] as const)('resolves every required evidence state', (input, expected) => {
    expect(resolveEvidenceState(input)).toBe(expected);
  });
});
