import { describe, expect, it } from 'vitest';
import type { Chart, QueryMeta } from '@/lib/api';
import { chartEvidenceStatus, parseSavedLimitation, queryLimitation, resolveEvidenceState, sourceEvidence, type ChartEvidenceStatus } from './evidence-panel';

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

  it('verifies exact declared bindings and renders their shared capture coverage from run watermarks', () => {
    const meta: QueryMeta = {
      query_ref: 'query/1', query_digest: 'digest', executed_at: '2026-10-05T00:00:00Z',
      result_completeness: 'complete', truncated: false, availability_reason: null,
      serving_data_watermark: {
        event_landed_at: null,
        total_sources: 6,
        sources_truncated: false,
        sources: [{ connector_id: 'source-1', table: 'billing', generation: 'v1', capture_started_at: '2026-09-13T00:00:00Z', capture_finished_at: '2026-10-04T00:00:00Z', landed_at: '2026-10-05T00:00:00Z' }],
      },
    };
    const result = sourceEvidence("SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing'", meta);
    expect(result.bindings).toBe('Verified 1 of 1 declared source binding · billing');
    expect(result.coverage).toMatch(/1\/1 required source.*6 source watermarks available.*watermark/i);
    expect(result.coverage).not.toContain('Coverage not verified');

    const ranged = sourceEvidence(`SELECT date, data FROM external_rows
      WHERE connector_id = 'source-1' AND table_name = 'billing'
        AND timestamp >= '2026-09-13' AND timestamp < '2026-10-05'`, meta);
    expect(ranged.bindings).toBe('Verified 1 of 1 declared source binding · billing');
    expect(ranged.coverage).not.toContain('Coverage not verified');

    for (const sql of [
      "SELECT count(*) FROM external_rows e WHERE (e.connector_id = 'source-1') AND ((e.table_name = 'billing')) AND e.timestamp >= '2026-09-13' AND e.timestamp < '2026-10-05' GROUP BY e.table_name;",
      "SELECT data FROM external_rows AS e WHERE e.connector_id = 'source-1' /* CASE OR NOT UNION SELECT */ AND e.table_name = 'billing' -- OR true\n",
    ]) {
      const positive = sourceEvidence(sql, meta);
      expect(positive.bindings).toBe('Verified 1 of 1 declared source binding · billing');
      expect(positive.coverage).not.toContain('Coverage not verified');
    }
    const quotedMeta: QueryMeta = {
      ...meta,
      serving_data_watermark: {
        ...meta.serving_data_watermark!,
        sources: [{ ...meta.serving_data_watermark!.sources[0], connector_id: "source' OR CASE END SELECT AND ( )" }],
      },
    };
    const quoted = sourceEvidence("SELECT data FROM external_rows WHERE connector_id = 'source'' OR CASE END SELECT AND ( )' AND table_name = 'billing'", quotedMeta);
    expect(quoted.bindings).toBe('Verified 1 of 1 declared source binding · billing');
    expect(quoted.coverage).not.toContain('Coverage not verified');
    expect(sourceEvidence("SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing'", undefined))
      .toEqual({ coverage: 'Coverage not verified', bindings: 'Bindings not verified' });

  });

  it.each([
    ["CASE unused branch", "SELECT data FROM external_rows WHERE CASE WHEN false THEN true AND connector_id = 'source-1' AND table_name = 'billing' AND true ELSE true END"],
    ["nested CASE branch", "SELECT data FROM external_rows WHERE CASE WHEN false THEN CASE WHEN false THEN true AND connector_id = 'source-1' AND table_name = 'billing' AND true ELSE true END ELSE true END"],
    ["same bindings in every OR branch", "SELECT data FROM external_rows WHERE (connector_id = 'source-1' AND table_name = 'billing') OR (connector_id = 'source-1' AND table_name = 'billing')"],
    ["NOT atom", "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' AND NOT false"],
    ["UNION unbound branch", "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' UNION ALL SELECT data FROM external_rows"],
    ["IN subselect", "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' AND data IN (SELECT data FROM events)"],
    ["scalar subselect", "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' AND (SELECT true)"],
    ["function arguments containing bindings", "SELECT data FROM external_rows WHERE coalesce(true, connector_id = 'source-1' AND table_name = 'billing')"],
    ["NEW BETWEEN delimiter manufactures conjuncts", "SELECT data FROM external_rows WHERE false BETWEEN false AND (connector_id = 'source-1') AND table_name = 'billing'"],
    ["quoted keywords are only literal content", "SELECT data FROM external_rows WHERE data = 'AND connector_id = ''source-1'' AND table_name = ''billing'' OR true'"],
    ["quoted identifier is not a keyword", "SELECT data FROM external_rows WHERE \"connector_id = 'source-1' AND table_name = 'billing'\""],
    ["dollar quoted keywords", "SELECT data FROM external_rows WHERE data = $$AND connector_id = 'source-1' AND table_name = 'billing'$$"],
    ["line comment with keywords", "SELECT data FROM external_rows WHERE true -- AND connector_id = 'source-1' AND table_name = 'billing'"],
    ["nested block comment with keywords", "SELECT data FROM external_rows WHERE true /* AND connector_id = 'source-1' /* nested */ AND table_name = 'billing' */"],
    ["unsupported extra atom", "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' AND true"],
    ["CTE is outside the flat SELECT whitelist", "WITH bound AS (SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing') SELECT * FROM bound"],
    ["JOIN scope is not verified", "SELECT e.data FROM external_rows e JOIN events v ON true WHERE e.connector_id = 'source-1' AND e.table_name = 'billing'"],
    ["trailing opaque expression", "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' || ''"],
    ['nested conjunction is not a top-level atom', "SELECT data FROM external_rows WHERE (connector_id = 'source-1' AND table_name = 'billing')"],
    ['quoted string cannot manufacture parenthesis depth', "SELECT data FROM external_rows WHERE data = '(' AND connector_id = 'source-1' AND table_name = 'billing' OR true"],
    ['unterminated comment', "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' /*"],
    ['second statement', "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing'; SELECT 1"],
    ['binding expression with a cast', "SELECT data FROM external_rows WHERE connector_id = 'source-1'::text AND table_name = 'billing'"],
    ['OR branch omits the table binding', "SELECT data FROM external_rows WHERE connector_id = 'source-1' OR table_name = 'billing'"],
    ['OR true bypasses the declared pair', "SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing' OR true"],
    ['bindings belong to an unrelated subquery', "SELECT data FROM external_rows WHERE EXISTS (SELECT 1 FROM events WHERE connector_id = 'source-1' AND table_name = 'billing')"],
    ['bindings appear only in a comment', "SELECT data FROM external_rows /* connector_id = 'source-1' AND table_name = 'billing' */"],
  ])('does not verify source bindings when %s', (_, sql) => {
    const result = sourceEvidence(sql, {
      query_ref: 'query/1', query_digest: 'digest', executed_at: '2026-10-05T00:00:00Z',
      result_completeness: 'complete', truncated: false, availability_reason: null,
      serving_data_watermark: {
        event_landed_at: null,
        total_sources: 1,
        sources_truncated: false,
        sources: [{ connector_id: 'source-1', table: 'billing', generation: 'v1', capture_started_at: '2026-09-13T00:00:00Z', capture_finished_at: '2026-10-04T00:00:00Z', landed_at: '2026-10-05T00:00:00Z' }],
      },
    });

    expect(result).toEqual({ coverage: 'Coverage not verified', bindings: 'Bindings not verified' });
  });

  it('does not promote project watermarks into coverage without matching exact declarations', () => {
    const meta = {
      query_ref: 'query/1', query_digest: 'digest', executed_at: '2026-10-05T00:00:00Z',
      result_completeness: 'complete' as const, truncated: false, availability_reason: null,
      serving_data_watermark: {
        event_landed_at: null, total_sources: 1, sources_truncated: false,
        sources: [{ connector_id: 'other-source', table: 'billing', generation: null, capture_started_at: '2026-09-13T00:00:00Z', capture_finished_at: '2026-10-04T00:00:00Z', landed_at: '2026-10-05T00:00:00Z' }],
      },
    };
    const missing = sourceEvidence("SELECT data FROM external_rows WHERE connector_id = 'source-1' AND table_name = 'billing'", meta);
    expect(missing.coverage).toMatch(/^Coverage not verified/);
    expect(missing.bindings).toMatch(/^Bindings not verified/);

    const ambiguous = sourceEvidence("SELECT data FROM external_rows WHERE table_name = 'billing'", meta);
    expect(ambiguous).toEqual({ coverage: 'Coverage not verified', bindings: 'Bindings not verified' });

    const inert = sourceEvidence("SELECT 1 FROM events -- FROM external_rows WHERE connector_id = 'other-source' AND table_name = 'billing'", meta);
    expect(inert).toEqual({ coverage: 'Coverage not verified', bindings: 'No external source bindings declared' });

    const invalidInterval = sourceEvidence("SELECT data FROM external_rows WHERE connector_id = 'other-source' AND table_name = 'billing'", {
      ...meta,
      serving_data_watermark: {
        ...meta.serving_data_watermark,
        sources: [{ ...meta.serving_data_watermark.sources[0], capture_started_at: 'not-a-time' }],
      },
    });
    expect(invalidInterval.coverage).toMatch(/^Coverage not verified/);
  });
});
