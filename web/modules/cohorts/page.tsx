'use client';

import { useMemo, useState } from 'react';
import { SlidersHorizontal, Users } from 'lucide-react';
import type { CohortRow } from '@/lib/api';
import { formatCompact, formatDate, formatPercent } from '@/lib/format';
import { useCohorts } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { FilterBar } from '@/modules/shared/components/filter-bar';
import { Button, EmptyState, Intro, Loading, Panel, Segment, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { AudienceManager } from './audience-manager';

const WEEK_MS = 7 * 24 * 3600 * 1000;

// Shown until the first response arrives; the real catalog is server-driven
// (CohortAnalysis.audiences) so a new audience added in the backend appears here
// with no client change. Kept in sync with audienceSegments in store.go.
const DEFAULT_AUDIENCES = [
  { key: 'all', label: 'Everyone' },
  { key: 'user', label: 'Users' },
  { key: 'guest', label: 'Guests' },
  { key: 'paid', label: 'Paid' },
  { key: 'premium', label: 'Premium' },
];

// retentionTone shades a heatmap cell by retention rate: a transparent-to-primary
// mix so a strong cohort reads as a saturated green and a churned one fades out.
// Week 0 (always 100%) and empty cells are handled by the caller.
function retentionTone(rate: number): string {
  const pct = Math.round(Math.min(1, Math.max(0, rate)) * 100);
  return `color-mix(in srgb, var(--color-primary) ${pct}%, transparent)`;
}

// avgRate averages a single period column, but only over cohorts whose week-N
// window has actually elapsed (mature), and counts a mature cohort with no
// week-N row as 0% — not as missing. The SQL emits no (cohort, period) row when
// uniqExact is 0, so without the mature/zero handling a fully-churned cohort
// would silently drop out of the denominator and inflate the average upward.
function avgRate(rows: CohortRow[], period: number): number | null {
  const cutoff = Date.now() - (period + 1) * WEEK_MS;
  const mature = rows.filter((r) => r.size > 0 && new Date(r.cohort_start).getTime() <= cutoff);
  if (mature.length === 0) return null;
  const sum = mature.reduce((acc, r) => acc + (r.cells.find((c) => c.period === period)?.rate ?? 0), 0);
  return sum / mature.length;
}

export function CohortsPage() {
  const [segment, setSegment] = useState('all');
  const [managing, setManaging] = useState(false);
  const { cohorts, loading } = useCohorts(segment);

  const periods = cohorts?.periods ?? 8;
  const columns = useMemo(() => Array.from({ length: periods + 1 }, (_, i) => i), [periods]);
  const audiences = cohorts?.audiences?.length ? cohorts.audiences : DEFAULT_AUDIENCES;

  const totalPeople = useMemo(
    () => (cohorts?.rows ?? []).reduce((sum, r) => sum + r.size, 0),
    [cohorts?.rows],
  );
  const week1 = cohorts ? avgRate(cohorts.rows, 1) : null;
  const week4 = cohorts ? avgRate(cohorts.rows, 4) : null;

  return (
    <AppShell active="traffic">
      <Intro
        title="Cohort analysis"
        sub="How weekly acquisition cohorts retain — split by users and guests."
        action={(
          <div className="flex items-center gap-2">
            <Segment options={audiences.map((a) => ({ value: a.key, label: a.label }))} value={segment} onChange={setSegment} />
            <Button variant="outline" size="sm" icon={<SlidersHorizontal size={14} />} onClick={() => setManaging(true)}>
              Manage
            </Button>
          </div>
        )}
      />
      {managing ? <AudienceManager onClose={() => setManaging(false)} /> : null}
      <FilterBar showErrors={false} />
      <StatsStrip stats={[
        { label: 'Cohorts', value: formatCompact(cohorts?.rows.length ?? 0) },
        { label: 'People', value: formatCompact(totalPeople) },
        { label: 'Avg week-1 retention', value: week1 == null ? '—' : formatPercent(week1 * 100) },
        { label: 'Avg week-4 retention', value: week4 == null ? '—' : formatPercent(week4 * 100) },
      ]} />
      <Panel title="Weekly retention by cohort">
        {!cohorts && loading ? (
          <Loading label="Loading cohorts…" />
        ) : !cohorts || cohorts.rows.length === 0 ? (
          <EmptyState
            icon={<Users size={22} />}
            title="Not enough history yet"
            detail="Cohort retention needs at least a couple of weeks of events. Check back as your audience grows."
          />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full border-separate border-spacing-1 text-[12.5px]">
              <thead>
                <tr>
                  <th className="sticky left-0 z-10 bg-[var(--color-background-card)] px-2 py-1.5 text-start font-medium text-[var(--color-text-secondary)]">Cohort</th>
                  <th className="px-2 py-1.5 text-end font-medium text-[var(--color-text-secondary)]">Size</th>
                  {columns.map((p) => (
                    <th key={p} className="px-2 py-1.5 text-center font-medium text-[var(--color-text-secondary)]">{`W${p}`}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {cohorts.rows.map((row) => {
                  const byPeriod = new Map(row.cells.map((c) => [c.period, c]));
                  return (
                    <tr key={row.cohort}>
                      <td className="sticky left-0 z-10 whitespace-nowrap bg-[var(--color-background-card)] px-2 py-1.5 font-medium">
                        {formatDate(row.cohort_start, { month: 'short', year: undefined })}
                      </td>
                      <td className="px-2 py-1.5 text-end font-mono tabular-nums text-[var(--color-text-secondary)]">{formatCompact(row.size)}</td>
                      {columns.map((p) => {
                        const cell = byPeriod.get(p);
                        if (!cell) return <td key={p} className="px-2 py-1.5 text-center text-[var(--color-text-disabled)]">·</td>;
                        const strong = cell.rate >= 0.5;
                        return (
                          <td
                            key={p}
                            className="rounded-md px-2 py-1.5 text-center font-mono tabular-nums"
                            style={{
                              backgroundColor: retentionTone(cell.rate),
                              color: strong ? 'var(--color-primary-foreground)' : 'var(--color-text-primary)',
                            }}
                            title={`${formatCompact(cell.users)} of ${formatCompact(row.size)} active in week ${p}`}
                          >
                            {formatPercent(cell.rate * 100, 0)}
                          </td>
                        );
                      })}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </AppShell>
  );
}
