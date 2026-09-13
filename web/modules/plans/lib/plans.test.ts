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

  // An object query_ref is a reproducible spec, not a label: the identifier (or
  // the SQL itself) and the version are what make the number re-checkable. A
  // line that prints only the kind cannot be re-run.
  it('renders the object query_ref identity and version', () => {
    expect(
      evidenceLine({
        created_at: '2026-09-12T00:00:00Z',
        evidence_json: JSON.stringify({ query_ref: { kind: 'saved_query', id_or_definition: 'activation_funnel', version: 3 } }),
      }),
    ).toBe('saved query activation_funnel v3');
  });

  it('prints a sql definition verbatim, never truncated', () => {
    const definition = "select count(*) from events where event_name = 'user.pageview' and platform = 'ios'";
    const line = evidenceLine({
      created_at: '2026-09-12T00:00:00Z',
      evidence_json: JSON.stringify({ query_ref: { kind: 'sql', id_or_definition: definition, version: 1 } }),
    });
    expect(line).toBe(`sql ${definition} v1`);
  });

  it('keeps the bare-string form and an identity-less object honest', () => {
    expect(evidenceLine({ created_at: '2026-09-12T00:00:00Z', evidence_json: JSON.stringify({ query_ref: 'activation_funnel' }) })).toBe('activation_funnel');
    // No id and no version: the kind alone is all the envelope carries, so the
    // line stops there rather than inventing an identity.
    expect(evidenceLine({ created_at: '2026-09-12T00:00:00Z', evidence_json: JSON.stringify({ query_ref: { kind: 'metric' } }) })).toBe('metric');
  });
});

describe('evidenceAvailable', () => {
  // The boolean and the rendered line must never disagree: a caller that
  // treats a finding as evidence-backed while the panel prints "evidence
  // unavailable" is presenting a guess as provenance.
  it('agrees with evidenceLine on what counts as provenance', () => {
    const rows = [
      { created_at: '2026-09-12T00:00:00Z', evidence_json: JSON.stringify({ query_ref: 'activation_funnel' }) },
      { created_at: '2026-09-12T00:00:00Z', evidence_json: JSON.stringify({ query_ref: { kind: 'metric', id_or_definition: 'activation', version: 2 } }) },
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
