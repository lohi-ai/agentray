import { execFileSync } from 'node:child_process';
import { describe, expect, it } from 'vitest';
import { defaultFilters } from '@/lib/api';
import { chartRangeCaption, projectChartRows, resolveChartQuery } from './chart-query';

const absolute = {
  ...defaultFilters,
  from: '2026-09-01T00:00:00+07:00',
  to: '2026-09-01T12:30:00+07:00',
};

function runDuckDB(sql: string): unknown {
  const setup = `CREATE TABLE events(timestamp VARCHAR);
    INSERT INTO events VALUES ('2000-01-01T00:00:00.000Z'), ('2026-08-31T18:00:00.000Z');`;
  return JSON.parse(execFileSync('duckdb', ['-json', ':memory:', '-c', `${setup}\n${sql}`], {
    encoding: 'utf8',
    maxBuffer: 1024 * 1024,
  }));
}

function substituteDateLiterals(sql: string): string {
  const replacements = [
    ["'{{from}}'", "'2026-08-31T17:00:00.000Z'"],
    ["'{{to}}'", "'2026-09-01T05:30:00.000Z'"],
  ] as const;
  return replacements.reduce((resolved, [placeholder, literal]) => {
    const index = resolved.lastIndexOf(placeholder);
    return index < 0
      ? resolved
      : resolved.slice(0, index) + literal + resolved.slice(index + placeholder.length);
  }, sql);
}

describe('resolveChartQuery', () => {
  it('binds an exact inclusive/exclusive UTC range without rounding sub-day precision', () => {
    const result = resolveChartQuery(
      `SELECT * FROM external_rows WHERE connector_id = 'c1' AND table_name = 'orders' AND synced_at >= '{{from}}' AND synced_at < '{{to}}' AND {{hours}} > 0`,
      absolute,
    );
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.status).toBe('applied');
    expect(result.sql).toContain("synced_at >= '2026-08-31T17:00:00.000Z'");
    expect(result.sql).toContain("synced_at < '2026-09-01T05:30:00.000Z'");
    expect(result.sql).toContain('12.5 > 0');
    expect(result.label).toContain('end exclusive');
  });

  it.each([
    ['keywords directly adjacent to literals', `SELECT count(*) AS value FROM events WHERE timestamp>='{{from}}'AND timestamp<'{{to}}'`],
    ['BETWEEN keywords directly adjacent to literals', `SELECT count(*) AS value FROM events WHERE timestamp BETWEEN'{{from}}'AND'{{to}}'`],
    ['aliases directly adjacent to literals', `SELECT '{{from}}'lo, '{{to}}'hi FROM events LIMIT 1`],
    ['binary literal before real bounds', `SELECT count(*) AS value, B'{{from}}' AS note FROM events WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`],
  ])('binds ordinary quoted date values with %s', (_, sql) => {
    expect(() => runDuckDB(sql)).not.toThrow();

    const result = resolveChartQuery(sql, absolute);
    expect(result).toMatchObject({ status: 'applied', ok: true });
    if (!result.ok) return;
    expect(runDuckDB(result.sql)).toEqual(runDuckDB(substituteDateLiterals(sql)));
  });

  it('returns and labels a distinct fixed range without applying the selected range', () => {
    const sql = `SELECT count(*) AS value FROM events
      WHERE timestamp >= '1999-01-01T00:00:00.000Z'
        AND timestamp < '2001-01-01T00:00:00.000Z'`;

    expect(runDuckDB(sql)).toEqual([{ value: 1 }]);
    const result = resolveChartQuery(sql, absolute);
    expect(result).toEqual({
      status: 'fixed',
      ok: true,
      sql,
      dates: ['1999-01-01T00:00:00.000Z', '2001-01-01T00:00:00.000Z'],
      label: '1999-01-01T00:00:00.000Z to 2001-01-01T00:00:00.000Z',
    });
    expect(chartRangeCaption(result)).toBe(
      'Fixed range — selected range not applied: 1999-01-01T00:00:00.000Z to 2001-01-01T00:00:00.000Z',
    );
    expect(resolveChartQuery(sql, { ...absolute, from: '2030-01-01', to: '2030-02-01' })).toEqual(result);
  });

  it.each([
    ['no literal dates', `SELECT count(*) AS value FROM events`],
    ['one literal date', `SELECT count(*) AS value FROM events WHERE timestamp >= '2026-08-31'`],
    ['ambiguous literal dates', `SELECT '2026-08-01', '2026-09-01', '2026-10-01'`],
    ['reversed literal dates', `SELECT '2026-09-02', '2026-09-01'`],
    ['invalid calendar dates', `SELECT '2026-02-30', '2026-03-01'`],
    ['dates only in inert text', `SELECT $$'2026-09-01' '2026-09-02'$$ AS note`],
  ])('does not invent a fixed range for %s', (_, sql) => {
    expect(resolveChartQuery(sql, absolute)).toMatchObject({ status: 'invalid', ok: false });
  });

  it.each([
    ['binary string prefix', `SELECT B'{{from}}' AS lower_bound, '{{to}}' AS upper_bound`],
    ['hex string prefix', `SELECT X'{{from}}' AS lower_bound, '{{to}}' AS upper_bound`],
  ])('does not bind a date placeholder inside a %s', (_, sql) => {
    expect(() => runDuckDB(sql)).not.toThrow();
    expect(resolveChartQuery(sql, absolute)).toMatchObject({ status: 'invalid', ok: false });
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

  it('rejects date tokens in a non-ASCII tagged dollar-quoted string without rewriting them', () => {
    const sql = `SELECT count(*) AS value, $é$'{{from}}' '{{to}}'$é$ AS note FROM events`;

    expect(resolveChartQuery(sql, absolute)).toMatchObject({ ok: false });
    expect(sql).toContain(`$é$'{{from}}' '{{to}}'$é$`);
  });

  it('rejects the round-6 continued-bound reproduction instead of claiming the canonical range', () => {
    const sql = `SELECT count(*) AS value FROM events WHERE timestamp >= '0000'\n'{{from}}' AND timestamp < '{{to}}'`;
    const canonical = `SELECT count(*) AS value FROM events WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`;

    expect(runDuckDB(substituteDateLiterals(sql))).toEqual([{ value: 2 }]);
    expect(runDuckDB(substituteDateLiterals(canonical))).toEqual([{ value: 1 }]);
    const result = resolveChartQuery(sql, absolute);
    expect(result).toMatchObject({ status: 'invalid', ok: false });
    expect(chartRangeCaption(result)).toBeNull();
  });

  it.each([
    ['continued upper suffix', `SELECT count(*) AS value FROM events WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'\n'junk'`],
    ['continued escape prefix', `SELECT E'prefix '\n'{{from}}' AS lo, '{{to}}' AS hi FROM events LIMIT 1`],
    ['continued line-comment prefix', `SELECT 'prefix ' -- note\n'{{from}}' AS lo, '{{to}}' AS hi FROM events LIMIT 1`],
    ['continued token note with real bounds', `SELECT count(*) AS value, 'prefix '\n'{{from}}' AS note FROM events WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`],
    ['continued fixed lower prefix', `SELECT count(*) AS value FROM events WHERE timestamp >= '0000'\n'1999-01-01T00:00:00Z' AND timestamp < '2001-01-01T00:00:00Z'`],
    ['continued fixed upper suffix', `SELECT count(*) AS value FROM events WHERE timestamp >= '1999-01-01T00:00:00Z' AND timestamp < '2001-01-01T00:00:00Z'\n'junk'`],
  ])('rejects ambiguous %s without displaying a range claim', (_, sql) => {
    expect(() => runDuckDB(sql)).not.toThrow();
    const result = resolveChartQuery(sql, absolute);
    expect(result).toMatchObject({ status: 'invalid', ok: false });
    expect(chartRangeCaption(result)).toBeNull();
  });

  it.each([
    ['multi-line ordinary string', `SELECT 'line\nnote', '{{from}}', '{{to}}'`],
    ['Unicode-line-separated ordinary string', `SELECT 'line\u2028note', '{{from}}', '{{to}}'`],
    ['backslash-newline escape string', `SELECT E'line\\\nnote', '{{from}}', '{{to}}'`],
    ['unusual whitespace', `SELECT count(*) FROM events\u00a0WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`],
    ['unterminated ordinary string', `SELECT '1999-01-01', '2001-01-01', 'unterminated`],
    ['unterminated block comment', `SELECT '1999-01-01', '2001-01-01' /* unterminated`],
    ['unterminated dollar string', `SELECT '1999-01-01', '2001-01-01', $tag$unterminated`],
  ])('fails closed for uncertain lexical construct: %s', (_, sql) => {
    const result = resolveChartQuery(sql, absolute);
    expect(result).toMatchObject({ status: 'invalid', ok: false });
    expect(chartRangeCaption(result)).toBeNull();
  });

  it.each([
    {
      name: 'standalone scalar bounds',
      binds: true,
      sql: `SELECT count(*) AS value FROM events
        WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`,
    },
    {
      name: 'identifier-embedded Unicode dollar sequences with real bounds',
      binds: true,
      sql: `SELECT count(*) AS value FROM events AS e$é$
        WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`,
    },
    {
      name: 'nested comment before real bounds',
      binds: true,
      sql: `SELECT count(*) AS value /* outer /* '{{from}}' */ '{{to}}' */ FROM events
        WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`,
    },
    {
      name: 'quoted aliases before real bounds',
      binds: true,
      sql: `SELECT count(*) AS "owner's" FROM events
        WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`,
    },
    {
      name: 'comment-shaped quoted alias before real bounds',
      binds: true,
      sql: `SELECT count(*) AS "--" FROM events
        WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`,
    },
    {
      name: 'Unicode dollar body before real bounds',
      binds: true,
      sql: `SELECT count(*) AS value, $aé$'{{from}}' '{{to}}'$aé$ AS note FROM events
        WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`,
    },
    {
      name: 'ordinary backslash string before real bounds',
      binds: true,
      sql: `SELECT count(*) AS value, '\\' AS note FROM events
        WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'`,
    },
    {
      name: 'identifier-boundary false-applied-range reproduction',
      binds: false,
      sql: `SELECT count(e$é$.timestamp) AS value,
        $é$'{{from}}' '{{to}}'$é$ AS note
        FROM events AS e$é$`,
    },
    {
      name: 'empty dollar body',
      binds: false,
      sql: `SELECT $$'{{from}}' '{{to}}'$$ AS note`,
    },
    {
      name: 'Unicode dollar body',
      binds: false,
      sql: `SELECT $é$'{{from}}' '{{to}}'$é$ AS note`,
    },
    {
      name: 'supplementary Unicode dollar body',
      binds: false,
      sql: `SELECT $😀$'{{from}}' '{{to}}'$😀$ AS note`,
    },
    {
      name: 'nested block comment',
      binds: false,
      sql: `SELECT 1 AS value /* outer /* inner '{{from}}' */ still outer '{{to}}' */`,
    },
    {
      name: 'quoted identifier',
      binds: false,
      sql: `SELECT 1 AS "'{{from}}' '{{to}}'"`,
    },
    {
      name: 'doubled adjacent quotes inside one scalar',
      binds: false,
      sql: `SELECT '''{{from}}''' AS lower_bound, '{{to}}' AS upper_bound`,
    },
    {
      name: 'escape string',
      binds: false,
      sql: `SELECT E'{{from}}' AS lower_bound, '{{to}}' AS upper_bound`,
    },
    {
      name: 'larger regular strings',
      binds: false,
      sql: `SELECT 'prefix {{from}}' AS lower_bound, '{{to}} suffix' AS upper_bound`,
    },
    {
      name: 'line comment',
      binds: false,
      sql: `SELECT 1 AS value -- '{{from}}' '{{to}}'`,
    },
  ])('matches DuckDB for $name', ({ binds, sql }) => {
    expect(() => runDuckDB(sql)).not.toThrow();

    const result = resolveChartQuery(sql, absolute);
    expect(result.ok).toBe(binds);
    if (!binds) {
      expect('label' in result).toBe(false);
      return;
    }

    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(runDuckDB(result.sql)).toEqual(runDuckDB(substituteDateLiterals(sql)));
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

  it('prefers the prior date/category field when automatic X has numeric metadata first', () => {
    expect(projectChartRows([
      { age_days: 7, cohort_date: '2026-09-01', value: 12 },
      { age_days: 7, cohort_date: '2026-09-02', value: 19 },
    ], 'value')).toEqual({
      status: 'ready',
      values: [12, 19],
      labels: ['2026-09-01', '2026-09-02'],
    });
  });

  it('falls back to a numeric X field when no date or category field exists', () => {
    expect(projectChartRows([
      { age_days: 7, value: 12 },
      { age_days: 14, value: 19 },
    ], 'value')).toEqual({
      status: 'ready',
      values: [12, 19],
      labels: [7, 14],
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
