'use client';

import type { BoardContent, OverviewResult } from '@/lib/api';
import type { AnalysisBoardKey } from '@/lib/analysis';
import { UNSERVED_TILES } from '@/lib/analysis';
import { AddAnnotationButton, type AnnotationsController } from '@/modules/annotations';
import { Chart } from '@/modules/shared/components/charts';
import { BarRows, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { FunnelTile } from './funnel';
import { analysisBars, analysisSeries, analysisStat, unservedStat } from './tiles';

export function AnalysisBoard({
  board,
  res,
  boardKey,
  platform,
  annotations,
}: {
  board: BoardContent;
  res: OverviewResult;
  boardKey: AnalysisBoardKey;
  platform: string;
  annotations: AnnotationsController;
}) {
  const unserved = UNSERVED_TILES[boardKey].map((tile) => unservedStat(res, tile.label));
  return (
    <>
      {board.definition.sections.map((section) => {
        const stats = section.tiles.map((tile) => analysisStat(res, tile)).filter((s): s is NonNullable<typeof s> => s !== null);
        const bars = section.tiles.map((tile) => analysisBars(res, tile)).filter((s): s is NonNullable<typeof s> => s !== null);
        const series = section.tiles.map((tile) => analysisSeries(res, tile)).filter((s): s is NonNullable<typeof s> => s !== null);
        const funnels = section.tiles.filter((tile) => tile.kind === 'funnel');
        return (
          <Panel key={section.key} title={section.title} action={series.length > 0 ? <AddAnnotationButton annotations={annotations} /> : undefined}>
            <div className="flex flex-col gap-4">
              {section.description ? (
                <p className="text-xs text-[var(--color-text-secondary)]">{section.description}</p>
              ) : null}
              {stats.length > 0 ? <StatsStrip stats={stats} /> : null}
              {series.map((item) => (
                <div key={item.label}>
                  {item.points.length > 0 ? (
                    <>
                      <Chart
                        spec={{
                          type: 'area',
                          x: item.points.map((p) => p.label),
                          series: [{ name: item.label, data: item.points.map((p) => p.value) }],
                          smooth: false,
                          integerY: true,
                          annotations: annotations.annotations,
                        }}
                      />
                      <p className="mt-2 text-xs text-[var(--color-text-secondary)]">
                        {item.points.map((p) => `${p.label.slice(5)}: ${p.value}`).join(' · ')}
                      </p>
                    </>
                  ) : (
                    <p className="text-sm text-[var(--color-text-secondary)]">No daily series in this range.</p>
                  )}
                  <p className="mt-2 font-mono text-xs text-[var(--color-text-secondary)]">{item.provenance}</p>
                </div>
              ))}
              {bars.length > 0 ? (
                <div className="grid grid-cols-2 gap-4 [@media(max-width:700px)]:grid-cols-1">
                  {bars.map((item) => (
                    <div key={item.label}>
                      <h3 className="mb-2 text-sm font-medium">{item.label}</h3>
                      <BarRows rows={item.rows} valueHead={item.label} countHead={item.unit} mono={item.label === 'Top pages'} empty={item.empty} />
                      <p className="mt-2 font-mono text-xs text-[var(--color-text-secondary)]">{item.provenance}</p>
                    </div>
                  ))}
                </div>
              ) : null}
              {funnels.map((tile) => (
                <div key={tile.key}>
                  {tile.title ? <h3 className="mb-2 text-sm font-medium">{tile.title}</h3> : null}
                  <FunnelTile steps={tile.steps} res={res} platform={platform} />
                </div>
              ))}
            </div>
          </Panel>
        );
      })}
      {unserved.length > 0 ? (
        <Panel title="Not available from AgentRay data">
          <div className="flex flex-col gap-3">
            <p className="text-xs text-[var(--color-text-secondary)]">
              App Store Connect labels with no AgentRay catalog metric. Each tile is Not available — never zero, never a sample, never inferred from unrelated events.
            </p>
            <StatsStrip stats={unserved} />
            <ul className="text-xs text-[var(--color-text-secondary)]">
              {UNSERVED_TILES[boardKey].map((tile) => (
                <li key={tile.label}><span className="font-medium text-[var(--color-text-primary)]">{tile.label}.</span> {tile.reason}</li>
              ))}
            </ul>
          </div>
        </Panel>
      ) : null}
    </>
  );
}
