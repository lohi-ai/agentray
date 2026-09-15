'use client';

import type { BoardTile, OverviewResult } from '@/lib/api';
import type { AnalysisBoardKey } from '@/lib/analysis';
import { Chart } from '@/modules/shared/components/charts';
import { BarRows, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { UsageFunnelPanel } from './funnel';
import { analysisBars, analysisSeries, analysisStat } from './tiles';

// System sections are the reads the retired /traffic, /web-analytics and
// /product surfaces served, rendered on the board that now answers them. They
// are page-level, not declaration edits: EnsureDefaultBoards never updates an
// existing key, so a declaration change would reach only new projects — these
// sections render for every project, seeded or not. Each row reuses the same
// composers a declared tile would, so a system section and a declared tile can
// never disagree about the same metric.
const SYSTEM_TILES: Record<AnalysisBoardKey, { stats: string[]; bars: string[]; series: string[] }> = {
  acquisition: {
    stats: ['pageviews', 'conversions', 'ai_share', 'bounce_rate', 'avg_session_duration'],
    bars: ['traffic_by_class', 'ai_top_paths', 'traffic_by_platform'],
    series: [],
  },
  monetization: { stats: [], bars: [], series: [] },
  usage: {
    stats: [],
    bars: ['top_events'],
    series: ['event_volume_daily'],
  },
};

function systemTile(metric: string): BoardTile {
  return { key: `system-${metric}`, metric, kind: 'metric' };
}

export function AnalysisSystemSections({
  boardKey,
  res,
  platform,
}: {
  boardKey: AnalysisBoardKey;
  res: OverviewResult;
  platform: string;
}) {
  const spec = SYSTEM_TILES[boardKey];
  const stats = spec.stats
    .map((metric) => analysisStat(res, systemTile(metric)))
    .filter((s): s is NonNullable<typeof s> => s !== null);
  const bars = spec.bars
    .map((metric) => analysisBars(res, systemTile(metric)))
    .filter((s): s is NonNullable<typeof s> => s !== null);
  const series = spec.series
    .map((metric) => analysisSeries(res, systemTile(metric)))
    .filter((s): s is NonNullable<typeof s> => s !== null);

  return (
    <>
      {stats.length > 0 ? (
        <Panel title="Traffic">
          <StatsStrip stats={stats} />
        </Panel>
      ) : null}
      {bars.length > 0 ? (
        <Panel title={boardKey === 'usage' ? 'Events' : 'Traffic breakdowns'}>
          <div className="grid grid-cols-2 gap-4 [@media(max-width:700px)]:grid-cols-1">
            {bars.map((item) => (
              <div key={item.label}>
                <h3 className="mb-2 text-sm font-medium">{item.label}</h3>
                <BarRows rows={item.rows} valueHead={item.label} countHead={item.unit} mono empty={item.empty} />
                <p className="mt-2 font-mono text-xs text-[var(--color-text-secondary)]">{item.provenance}</p>
              </div>
            ))}
          </div>
        </Panel>
      ) : null}
      {series.map((item) => (
        <Panel key={item.label} title={item.label}>
          {item.points.length > 0 ? (
            <Chart
              spec={{
                type: 'area',
                x: item.points.map((p) => p.label),
                series: [{ name: item.label, data: item.points.map((p) => p.value) }],
                smooth: false,
                integerY: true,
              }}
            />
          ) : (
            <p className="text-sm text-[var(--color-text-secondary)]">No daily series in this range.</p>
          )}
          <p className="mt-2 font-mono text-xs text-[var(--color-text-secondary)]">{item.provenance}</p>
        </Panel>
      ))}
      {boardKey === 'usage' ? <UsageFunnelPanel res={res} platform={platform} /> : null}
    </>
  );
}
