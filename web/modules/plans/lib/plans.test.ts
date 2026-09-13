import { describe, expect, it } from 'vitest';
import { evidenceAvailable, evidenceLine } from './plans';

describe('evidenceLine', () => {
  it('keeps every typed provenance field visible', () => {
    expect(
      evidenceLine({
        created_at: '2026-09-12T00:00:00Z',
        evidence_json: JSON.stringify({
          query_ref: { kind: 'saved_query' },
          range: '2026-09-01/2026-09-07',
          metric_version: 'retention.v3',
          dataset_version: 'orders@2026-09-08',
          watermark: '2026-09-08T12:00:00Z',
          timezone: 'Asia/Ho_Chi_Minh',
          filters: { country: 'VN' },
          warnings: ['source reconciles weekly'],
        }),
      }),
    ).toBe(
      'saved query · 2026-09-01/2026-09-07 · metric retention.v3 · dataset orders@2026-09-08 · watermark 2026-09-08T12:00:00Z · Asia/Ho_Chi_Minh · filters: {"country":"VN"} · warning: source reconciles weekly',
    );
  });

  it('does not invent provenance for malformed evidence', () => {
    expect(evidenceLine({ created_at: '2026-09-12T00:00:00Z', evidence_json: '{not json' })).toContain('evidence unavailable');
  });
});

describe('evidenceAvailable', () => {
  // The boolean and the rendered line must never disagree: a caller that
  // treats a finding as evidence-backed while the panel prints "evidence
  // unavailable" is presenting a guess as provenance.
  it('agrees with evidenceLine on what counts as provenance', () => {
    const rows = [
      { created_at: '2026-09-12T00:00:00Z', evidence_json: JSON.stringify({ query_ref: 'activation_funnel' }) },
      { created_at: '2026-09-12T00:00:00Z', evidence_json: JSON.stringify({ events: 202, sessions: 8 }) },
      { created_at: '2026-09-12T00:00:00Z', evidence_json: '{}' },
      { created_at: '2026-09-12T00:00:00Z', evidence_json: '{not json' },
      { created_at: '2026-09-12T00:00:00Z', evidence_json: '' },
      { created_at: '2026-09-12T00:00:00Z' },
    ];
    for (const row of rows) {
      expect(evidenceAvailable(row)).toBe(!evidenceLine(row).startsWith('evidence unavailable'));
    }
  });
});
