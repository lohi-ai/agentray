import { describe, expect, it } from 'vitest';
import { defaultFilters } from '@/lib/api';
import { projectChartRows, resolveChartQuery } from './chart-query';

const absolute = {
  ...defaultFilters,
  from: '2026-09-01T00:00:00+07:00',
  to: '2026-09-01T12:30:00+07:00',
};

describe('resolveChartQuery', () => {
  it('binds an exact inclusive/exclusive UTC range without rounding sub-day precision', () => {
    const result = resolveChartQuery(
      `SELECT * FROM external_rows WHERE connector_id = 'c1' AND table_name = 'orders' AND synced_at >= '{{from}}' AND synced_at < '{{to}}' AND {{hours}} > 0`,
      absolute,
    );
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.sql).toContain("synced_at >= '2026-08-31T17:00:00.000Z'");
    expect(result.sql).toContain("synced_at < '2026-09-01T05:30:00.000Z'");
    expect(result.sql).toContain('12.5 > 0');
    expect(result.label).toContain('end exclusive');
  });

  it.each([
    ['unbound', `SELECT count(*) FROM events`],
    ['partial', `SELECT count(*) FROM events WHERE timestamp >= '{{from}}'`],
    ['hours only', `SELECT count(*) FROM events WHERE timestamp > now() - INTERVAL '{{hours}} hours'`],
    ['unknown token', `SELECT count(*) FROM events WHERE timestamp >= '{{start}}' AND timestamp < '{{to}}'`],
    ['unquoted token', `SELECT count(*) FROM events WHERE timestamp >= {{from}} AND timestamp < '{{to}}'`],
  ])('fails %s SQL closed', (_, sql) => {
    expect(resolveChartQuery(sql, absolute)).toMatchObject({ ok: false });
  });

  it('does not count or substitute placeholders that appear only in comments', () => {
    const sql = `SELECT count(*) FROM events -- '{{from}}' '{{to}}'\n/* {{hours}} */`;
    const result = resolveChartQuery(sql, absolute);
    expect(result).toMatchObject({ ok: false });
    expect(sql).toContain('{{from}}');
  });

  it.each([
    ['nested comment', `SELECT count(*) FROM events /* outer /* inner */ WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}' */`],
    ['quoted identifier', `SELECT count(*) AS "'{{from}}' '{{to}}'" FROM events`],
    ['larger string literal', `SELECT count(*) FROM events WHERE event_name = 'prefix {{from}}' AND timestamp < '{{to}}'`],
    ['dollar-quoted string', `SELECT count(*) AS value, $$'{{from}}' '{{to}}'$$ AS note FROM events`],
    ['tagged dollar-quoted string', `SELECT count(*) AS value, $range$'{{from}}' '{{to}}'$range$ AS note FROM events`],
  ])('rejects date tokens in a %s', (_, sql) => {
    expect(resolveChartQuery(sql, absolute)).toMatchObject({ ok: false });
  });

  it('rejects invalid or reversed applied bounds before executing SQL', () => {
    const sql = `SELECT count(*) FROM events WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`;
    expect(resolveChartQuery(sql, { ...absolute, from: 'invalid' })).toMatchObject({ ok: false });
    expect(resolveChartQuery(sql, { ...absolute, from: absolute.to, to: absolute.from })).toMatchObject({ ok: false });
  });
});

describe('projectChartRows', () => {
  it('preserves finite values and labels', () => {
    expect(projectChartRows([
      { date: '2026-09-01', value: 1.5 },
      { date: '2026-09-02', value: '2.25' },
    ], 'value', 'date')).toEqual({
      status: 'ready',
      values: [1.5, 2.25],
      labels: ['2026-09-01', '2026-09-02'],
    });
  });

  it.each([
    null,
    undefined,
    'not-a-number',
    '9007199254740993',
    '9007199254740993.00',
    '9.007199254740993e15',
    '1e-400',
    Number.NaN,
    Number.POSITIVE_INFINITY,
  ])(
    'does not silently plot %s as zero',
    (value) => {
      expect(projectChartRows([{ date: '2026-09-01', value }], 'value', 'date')).toMatchObject({ status: 'non_plottable' });
    },
  );

  it('distinguishes an empty successful result', () => {
    expect(projectChartRows([])).toEqual({ status: 'empty' });
  });
});
