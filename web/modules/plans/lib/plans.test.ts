import { describe, expect, it } from 'vitest';
import { evidenceLine } from './plans';

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
