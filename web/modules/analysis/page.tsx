'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertTriangle, Info } from 'lucide-react';
import { AgentRayAPI, APIError, type BoardContent, type OverviewResult } from '@/lib/api';
import type { AnalysisBoardKey } from '@/lib/analysis';
import { ANALYSIS_BOARDS } from '@/lib/analysis';
import { useAuthStore } from '@/lib/app-state';
import { platformLabel } from '@/lib/platform';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, Callout, Loading, Segment } from '@/modules/shared/components/signal-primitives';
import { useAnnotations, type AnnotationsController } from '@/modules/annotations';
import { AnalysisBoard } from './board';

const PERIODS = [
  { value: 'today', label: 'Today' },
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: '90d', label: '90 days' },
];
const PLATFORM_OPTIONS = ['web', 'ios', 'android', 'server', 'unknown'].map((value) => ({
  value,
  label: platformLabel(value),
}));
const TARGET_44 = '[&_button]:min-h-[44px] [&_[role=radio]]:min-h-[44px]';

export function AnalysisPage({ boardKey }: { boardKey: AnalysisBoardKey }) {
  const projectID = useAuthStore((s) => s.project?.id);
  const [period, setPeriod] = useState('7d');
  const [platform, setPlatform] = useState('');
  const meta = ANALYSIS_BOARDS.find((b) => b.key === boardKey)!;

  const overviewQuery = useQuery({
    queryKey: ['overview', projectID, period, platform],
    queryFn: () => new AgentRayAPI(projectID!).overview(period, platform),
    enabled: !!projectID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
  const boardQuery = useQuery({
    queryKey: ['board', projectID, boardKey],
    queryFn: () => new AgentRayAPI(projectID!).getBoard({ board_key: boardKey }),
    enabled: !!projectID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const res = overviewQuery.data ?? null;
  const board = boardQuery.data ?? null;
  const loading = overviewQuery.isLoading || boardQuery.isLoading;
  const error = overviewQuery.error || boardQuery.error;

  // The board's annotation window is the served overview range — the same
  // window every tile's series covers.
  const annotationRange = res?.context.range ?? null;
  const annotations = useAnnotations(
    annotationRange ? { from: annotationRange.from, to: annotationRange.to } : null,
  );

  return (
    <AppShell
      title={meta.label}
      sub={board?.board.description || 'AgentRay values under App Store Connect labels. Missing metrics stay Not available.'}
      actions={
        <div className={`flex flex-wrap items-center gap-2 ${TARGET_44}`}>
          <Segment
            options={[{ value: 'all', label: 'All platforms' }, ...PLATFORM_OPTIONS]}
            value={platform || 'all'}
            onChange={(v) => setPlatform(v === 'all' ? '' : v)}
            label="Platform"
          />
          <Segment options={PERIODS} value={period} onChange={setPeriod} label="Time range" />
        </div>
      }
    >
      <Callout
        tone="agentic"
        icon={<Info size={18} />}
        label="Provenance"
        title="Labels follow App Store Connect. Values are AgentRay."
        detail="These tiles are not store reports. Each number is an AgentRay catalog metric (or Not available when nothing computes it). Definitions stay on the tile provenance line."
      />
      {loading ? <Loading label="Loading analysis…" /> : null}
      {error ? (
        <Callout
          tone="warn"
          icon={<AlertTriangle size={18} />}
          label="Unavailable"
          title={error instanceof APIError && error.status === 403 ? 'You cannot read this board' : `${meta.label} is unavailable`}
          detail={error instanceof Error ? error.message : 'The analysis read failed. Retry; your data is unchanged.'}
          action={<Button variant="outline" size="sm" className="min-h-[44px]" onClick={() => { void overviewQuery.refetch(); void boardQuery.refetch(); }}>Retry</Button>}
        />
      ) : null}
      {!loading && !error && res && board ? <BoardOrEmpty board={board} res={res} boardKey={boardKey} annotations={annotations} /> : null}
    </AppShell>
  );
}

function BoardOrEmpty({ board, res, boardKey, annotations }: { board: BoardContent; res: OverviewResult; boardKey: AnalysisBoardKey; annotations: AnnotationsController }) {
  if (!board.has_definition) {
    return (
      <Callout
        tone="warn"
        icon={<AlertTriangle size={18} />}
        label="Undeclared"
        title="This board has no declared content"
        detail="The analysis destination exists, but no 007 declaration is stored on it yet. Re-open the project after the API has seeded default boards."
      />
    );
  }
  return <AnalysisBoard board={board} res={res} boardKey={boardKey} annotations={annotations} />;
}
