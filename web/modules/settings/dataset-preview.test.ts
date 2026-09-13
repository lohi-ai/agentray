import { describe, expect, it } from 'vitest';
import { previewProjection, type PreviewRow } from './dataset-preview';

// The preview table is rendered from these column keys. A duplicate key is a
// React key collision and two columns fighting over one value; a source column
// that lands on a landing-metadata key would silently replace what the sync
// recorded with whatever the source table happens to contain.
describe('previewProjection', () => {
  const row = (data: Record<string, unknown>, meta: Partial<PreviewRow> = {}): PreviewRow => ({
    row_key: meta.row_key ?? 'rk-1',
    cursor: meta.cursor ?? '2026-09-12T00:00:00Z',
    synced_at: meta.synced_at ?? '2026-09-12T01:00:00Z',
    data: JSON.stringify(data),
  });

  it('keeps a source column named row_key from overwriting the landing key', () => {
    const { rows } = previewProjection([row({ row_key: 'source-value', email: 'a@b.c' })]);

    expect(rows[0].row_key).toBe('rk-1');
    expect(rows[0]['source.row_key']).toBe('source-value');
  });

  it('keeps source synced_at and cursor columns out of the landing metadata', () => {
    const { rows, sourceKeys } = previewProjection([
      row({ synced_at: 'not-a-time', cursor: 42 }),
    ]);

    expect(sourceKeys).toEqual(['synced_at', 'cursor']);
    expect(rows[0].synced_at).toBe('2026-09-12T01:00:00Z');
    expect(rows[0].cursor).toBe('2026-09-12T00:00:00Z');
    expect(rows[0]['source.synced_at']).toBe('not-a-time');
    expect(rows[0]['source.cursor']).toBe(42);
  });

  it('never emits two columns under one key', () => {
    const { sourceKeys, sourceKeyMap, rows } = previewProjection([
      row({ row_key: 'a', 'source.row_key': 'b', email: 'c' }),
    ]);

    const keys = ['row_key', ...sourceKeys.map((k) => sourceKeyMap[k]), 'synced_at'];
    expect(new Set(keys).size).toBe(keys.length);
    expect(rows[0]['source.row_key']).toBe('a');
    expect(rows[0]['source.source.row_key']).toBe('b');
  });

  it('passes ordinary source columns through unchanged', () => {
    const { sourceKeys, rows } = previewProjection([row({ email: 'a@b.c', plan: 'pro' })]);

    expect(sourceKeys).toEqual(['email', 'plan']);
    expect(rows[0].email).toBe('a@b.c');
    expect(rows[0].plan).toBe('pro');
  });

  it('renders an unparseable payload verbatim instead of inventing columns', () => {
    const { rows } = previewProjection([{ row_key: 'rk-2', cursor: '', synced_at: '', data: 'not json' }]);

    expect(rows[0]).toEqual({ data: 'not json', row_key: 'rk-2', cursor: '', synced_at: '' });
  });
});
