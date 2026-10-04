import { describe, expect, it } from 'vitest';
import type { SourceReadiness } from '@/lib/api';
import { sourceReadinessView } from './source-readiness';

const readiness = (state: SourceReadiness['state'], extra: Partial<SourceReadiness> = {}): SourceReadiness => ({
  state, published_at: null, landed_at: null, queryable_at: null, generation: null,
  capture_started_at: null, capture_finished_at: null, reason: null, last_complete_at: null, ...extra,
});

describe('sourceReadinessView', () => {
  it.each([
    ['ready', 'Ready to query'], ['syncing', 'Syncing'], ['not_configured', 'Not configured'],
    ['stale', 'Stale'], ['incomplete', 'Not fully queryable'], ['error', 'Readiness error'],
  ] as const)('renders the %s state honestly', (state, label) => {
    expect(sourceReadinessView(readiness(state)).label).toBe(label);
  });

  it('keeps a stale result tied to its last complete timestamp', () => {
    const view = sourceReadinessView(readiness('stale', { last_complete_at: '2026-10-03T11:14:00Z' }));
    expect(view.detail).toMatch(/Previous complete sync.*2026/);
  });

  it('reports unknown evidence as unverified rather than ready', () => {
    expect(sourceReadinessView(null).label).toBe('Coverage not verified');
    expect(sourceReadinessView(null, true).state).toBe('denied');
  });

  it('shows the capture interval without exposing generation/debug ids', () => {
    const view = sourceReadinessView(readiness('ready', { capture_started_at: '2026-10-03T10:00:00Z', capture_finished_at: '2026-10-03T11:00:00Z', generation: 'secret-sequence' }));
    expect(view.captureInterval).toMatch(/2026.*2026/);
    expect(JSON.stringify(view)).not.toContain('secret-sequence');
  });
});
