import { describe, expect, it } from 'vitest';
import { annotationMarks, type ChartAnnotation } from './charts';

// annotationMarks is the renderer's own guarantee that a mark lands on the
// right bucket: instants resolve to the bucket containing them, ranges to the
// first/last buckets they overlap, and a non-temporal axis gets nothing.

const day = (n: number) => `2026-09-${String(n).padStart(2, '0')}`;
const days = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, i) => day(from + i));

function anno(partial: Partial<ChartAnnotation>): ChartAnnotation {
  return { id: 'a1', label: 'v2.4 deploy', kind: 'deploy', starts_at: '2026-09-10T12:00:00Z', ...partial };
}

describe('annotationMarks', () => {
  it('resolves an instant to the day bucket containing it', () => {
    const marks = annotationMarks(days(8, 14), [anno({})]);
    expect(marks).toEqual([{ kind: 'point', x: day(10), annotation: expect.objectContaining({ label: 'v2.4 deploy' }) }]);
  });

  it('resolves a range to its first and last overlapping buckets', () => {
    const marks = annotationMarks(days(8, 14), [anno({ starts_at: '2026-09-12T00:00:00Z', ends_at: '2026-09-13T12:00:00Z' })]);
    expect(marks).toEqual([{ kind: 'range', xFrom: day(12), xTo: day(13), annotation: expect.objectContaining({ label: 'v2.4 deploy' }) }]);
  });

  it('drops annotations outside the window — the chart never draws off-axis', () => {
    const marks = annotationMarks(days(8, 14), [
      anno({ id: 'before', starts_at: '2026-09-01T00:00:00Z' }),
      anno({ id: 'after', starts_at: '2026-09-20T00:00:00Z' }),
      anno({ id: 'edge', starts_at: '2026-09-15T00:00:00Z' }), // exactly at the half-open end
    ]);
    expect(marks).toEqual([]);
  });

  it('keeps a range that only clips the window edge', () => {
    const marks = annotationMarks(days(8, 14), [anno({ starts_at: '2026-09-13T00:00:00Z', ends_at: '2026-09-20T00:00:00Z' })]);
    expect(marks).toEqual([{ kind: 'range', xFrom: day(13), xTo: day(14), annotation: expect.anything() }]);
  });

  it('renders nothing on a categorical axis — a mark on the wrong bucket is worse than none', () => {
    expect(annotationMarks(['Top pages', 'Pricing', 'Docs'], [anno({})])).toEqual([]);
    expect(annotationMarks(undefined, [anno({})])).toEqual([]);
    expect(annotationMarks(days(8, 14), undefined)).toEqual([]);
    expect(annotationMarks(days(8, 14), [])).toEqual([]);
  });

  it('resolves against hour buckets, not only days', () => {
    const hours = ['2026-09-10T08:00:00Z', '2026-09-10T09:00:00Z', '2026-09-10T10:00:00Z'];
    const marks = annotationMarks(hours, [anno({ starts_at: '2026-09-10T09:30:00Z' })]);
    expect(marks).toEqual([{ kind: 'point', x: '2026-09-10T09:00:00Z', annotation: expect.anything() }]);
  });
});
