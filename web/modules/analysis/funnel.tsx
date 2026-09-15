'use client';

import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Filter } from 'lucide-react';
import { AgentRayAPI, defaultFilters, type Filters, type InsightResult, type OverviewResult } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { formatFractionAsPercent } from '@/lib/format';
import { funnelStepNames } from '@/lib/ia';
import { platformLabel } from '@/lib/platform';
import { useEventNames } from '@/modules/app/hooks';
import { Chart } from '@/modules/shared/components/charts';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { EmptyState, Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { Text } from '@astryxdesign/core/Text';
import { headlineStats } from './headline';

type FunnelStep = InsightResult['funnel'][number];

// The board's own period/platform controls scope this panel: the insight call
// takes the overview's served range verbatim, so the funnel and the tiles
// above it always describe the same window.
function boardFilters(res: OverviewResult, platform: string): Filters {
  return {
    ...defaultFilters,
    from: res.context.range.from,
    to: res.context.range.to,
    platform,
  };
}

// useFunnelByPlatform runs the same funnel once per platform, so a product that
// ships more than one app can see which one leaks and where. It is the smallest
// honest way to answer that: the blended funnel is the *average* of two curves
// and describes neither, and until this existed the only route to the split was
// asking the agent to write the SQL.
//
// Platforms come from the overview's traffic_by_platform rows — the apps that
// actually sent events in this window — so the list does not collapse when a
// platform filter is applied. Runs only when there really is more than one.
function useFunnelByPlatform(steps: string[], platforms: string[], filters: Filters) {
  const projectID = useAuthStore((s) => s.project?.id);
  const enabled = !!projectID && platforms.length > 1 && steps.length > 0;

  const query = useQuery({
    queryKey: ['funnel-by-platform', projectID, filters, steps, platforms],
    queryFn: async () => {
      const client = new AgentRayAPI(projectID!);
      // One failing platform must not blank the comparison — it drops out of the
      // table instead, the same degrade-per-slice rule the console fan-out uses.
      const results = await Promise.all(
        platforms.map((platform) =>
          client
            .insight('funnel', { ...filters, platform }, 'events', steps)
            .then((data) => ({ platform, funnel: data.insight?.funnel ?? [] }))
            .catch(() => null),
        ),
      );
      return results.filter((r): r is { platform: string; funnel: NonNullable<typeof r>['funnel'] } => r !== null);
    },
    enabled,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  return { splits: query.data ?? [], loading: query.isFetching && enabled };
}

// UsageFunnelPanel is the retired Product page's drop-off question, pinned to
// the Usage board: the catalog-derived activation funnel, its per-step table,
// and the per-platform split. The trend/retention/table questions it used to
// sit beside are covered by the board's own tiles — this panel is the one
// question no catalog metric answers.
export function UsageFunnelPanel({ res, platform }: { res: OverviewResult; platform: string }) {
  const projectID = useAuthStore((s) => s.project?.id);
  const { names: eventNames, loading: namesLoading } = useEventNames();
  const emptyCatalog = !namesLoading && eventNames.length === 0;
  const steps = useMemo(() => (emptyCatalog ? [] : funnelStepNames(eventNames)), [emptyCatalog, eventNames]);
  const filters = useMemo(() => boardFilters(res, platform), [res, platform]);

  const funnelQuery = useQuery({
    queryKey: ['usage-funnel', projectID, filters, steps],
    queryFn: () => new AgentRayAPI(projectID!).insight('funnel', filters, 'events', steps),
    enabled: !!projectID && steps.length > 0,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });
  const insight = funnelQuery.data?.insight ?? null;

  // Under a platform filter there is only one app to ask about, so the split
  // drops to a single series and PlatformFunnels' own two-or-more guard hides
  // it — the funnel above is already that app's.
  const platforms = useMemo(
    () => (platform ? [platform] : res.content.traffic_by_platform.rows.map((r) => r.value)),
    [platform, res.content.traffic_by_platform.rows],
  );
  const { splits, loading: splitsLoading } = useFunnelByPlatform(steps, platforms, filters);

  if (namesLoading) {
    return (
      <Panel title="Where do new users drop off?">
        <Loading label="Loading event catalog…" />
      </Panel>
    );
  }
  if (emptyCatalog) {
    return (
      <Panel title="Where do new users drop off?">
        <EmptyState
          icon={<Filter size={22} style={{ color: 'var(--agent)' }} />}
          title="No events yet"
          detail="The funnel is derived from this project’s event catalog. Send product events and the activation steps appear here."
        />
      </Panel>
    );
  }

  const stats = insight ? headlineStats(insight) : [];
  return (
    <Panel title="Where do new users drop off?">
      <div className="flex flex-col gap-4">
        <p className="text-xs text-[var(--color-text-secondary)]">
          Step-by-step conversion over the board’s selected range, derived from the event catalog.
        </p>
        {funnelQuery.isLoading ? <Loading label="Running funnel…" /> : null}
        {funnelQuery.isError ? (
          <Text type="supporting">The funnel read failed. Retry by changing the range or reloading the page.</Text>
        ) : null}
        {stats.length > 0 ? <StatsStrip stats={stats} /> : null}
        {insight?.funnel?.length ? (
          <>
            <Chart spec={{
              type: 'bar',
              x: insight.funnel.map((f) => f.event_name),
              series: [{ name: 'Users', data: insight.funnel.map((f) => f.users) }],
              height: 240,
            }} />
            <FunnelTable funnel={insight.funnel} />
          </>
        ) : null}
        {insight && !insight.funnel?.length && !funnelQuery.isLoading ? (
          <Text type="supporting">No one entered this funnel in the selected range.</Text>
        ) : null}
        <PlatformFunnels splits={splits} loading={splitsLoading} />
      </div>
    </Panel>
  );
}

// PlatformFunnels puts each app's funnel beside the others. The blended funnel
// above it is the average of these curves and describes none of them — a site
// that converts at 25% and an app that converts at 50% report "one third" and
// send you to fix the wrong one.
//
// Rows are the steps of the funnel that ran; each platform contributes a people
// count and its conversion from the first step. A platform whose insight failed
// is absent rather than shown as zero.
function PlatformFunnels({
  splits,
  loading,
}: {
  splits: Array<{ platform: string; funnel: FunnelStep[] }>;
  loading: boolean;
}) {
  const usable = splits.filter((s) => s.funnel.length > 0);
  const columns = useMemo<DataColumn<PlatformFunnelRow>[]>(() => {
    if (usable.length === 0) return [];
    return [
      { key: 'event_name', header: 'Step', renderCell: (r) => <span className="font-mono">{r.event_name}</span> },
      ...usable.map((split) => ({
        key: split.platform,
        header: platformLabel(split.platform),
        renderCell: (r: PlatformFunnelRow) => {
          const cell = r.byPlatform[split.platform];
          if (!cell) return <Text type="supporting">—</Text>;
          return (
            <span className="font-mono tabular-nums">
              {cell.users}
              <span className="ms-2 text-[var(--color-text-secondary)]">{formatFractionAsPercent(cell.conversion)}</span>
            </span>
          );
        },
      })),
    ];
  }, [usable]);

  if (loading) return <Loading label="Comparing platforms…" />;
  if (usable.length < 2) return null;

  // Steps come from the longest funnel returned, so a platform missing a step
  // shows an em dash there instead of shortening the table for everyone.
  const longest = usable.reduce((best, s) => (s.funnel.length > best.funnel.length ? s : best), usable[0]);
  const rows: PlatformFunnelRow[] = longest.funnel.map((step, index) => ({
    step: step.step,
    event_name: step.event_name,
    byPlatform: Object.fromEntries(
      usable
        .map((s) => [s.platform, s.funnel[index]] as const)
        .filter(([, cell]) => !!cell && cell.event_name === step.event_name),
    ),
  }));

  return <DataTable title="Same funnel, per platform" columns={columns} data={rows} idKey="step" pageSize={10} />;
}

type PlatformFunnelRow = {
  step: number;
  event_name: string;
  byPlatform: Record<string, FunnelStep | undefined>;
};

// FunnelTable renders the per-step funnel breakdown with sortable columns.
function FunnelTable({ funnel }: { funnel: FunnelStep[] }) {
  const columns = useMemo<DataColumn<FunnelStep>[]>(() => [
    { key: 'step', header: 'Step', sortValue: (f) => f.step, renderCell: (f) => <span className="font-mono tabular-nums">{f.step}</span> },
    { key: 'event_name', header: 'Event', renderCell: (f) => <span className="font-mono">{f.event_name}</span> },
    { key: 'users', header: 'Users', renderCell: (f) => <span className="font-mono tabular-nums">{f.users}</span> },
    { key: 'conversion', header: 'Conv.', renderCell: (f) => <span className="font-mono tabular-nums">{formatFractionAsPercent(f.conversion)}</span> },
  ], []);
  return <DataTable title="Funnel steps" columns={columns} data={funnel} idKey="step" pageSize={10} />;
}
