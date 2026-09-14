'use client';

import { useRouter } from 'next/navigation';
import { Sparkles, TrendingUp } from 'lucide-react';
import { Table } from '@astryxdesign/core/Table';
import { formatCompact, formatDuration, formatNumber, formatPercent } from '@/lib/format';
import { useFilters, useWebAnalytics } from '@/modules/app/hooks';
import { useFiltersStore } from '@/lib/app-state';
import { platformLabel } from '@/lib/platform';
import { AppShell } from '@/modules/shared/components/app-shell';
import { FilterBar } from '@/modules/shared/components/filter-bar';
import { BarRows, Button, Callout, Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';

export function WebAnalyticsPage() {
  const router = useRouter();
  const web = useWebAnalytics();
  const applied = useFiltersStore((s) => s.appliedFilters);
  const { refresh } = useFilters();

  if (!web) {
    return (
      <AppShell title="Traffic" sub="Where visitors come from and which sources are worth more.">
        <Loading label="Loading traffic…" />
      </AppShell>
    );
  }

  const totalClass = web.traffic_by_class.reduce((sum, c) => sum + c.count, 0) || 1;
  const aiCount = web.traffic_by_class.filter((c) => /ai|crawler|assistant|bot/i.test(c.class)).reduce((sum, c) => sum + c.count, 0);
  const aiShare = (aiCount / totalClass) * 100;
  const topSource = web.referrers_by_channel[0] || web.referrers[0];

  return (
    <AppShell
      title="Traffic"
      sub="Where visitors come from and which sources are worth more."
      actions={<Button variant="agent" icon={<Sparkles size={15} />} onClick={() => router.push('/chat')}>Ask about traffic</Button>}
    >
      <FilterBar showEventType={false} showErrors={false} />
      <StatsStrip
        stats={[
          { label: 'Visitors', value: formatNumber(web.visitors) },
          { label: 'Pageviews', value: formatNumber(web.pageviews) },
          { label: 'Sessions', value: formatNumber(web.sessions) },
          { label: 'Conversions', value: formatNumber(web.conversions) },
          { label: 'Avg session', value: formatDuration(web.avg_session_duration_seconds) },
          { label: 'AI traffic', value: formatPercent(aiShare), tone: aiShare > 0 ? 'agent' : undefined },
        ]}
      />
      {topSource ? (
        <Callout
          tone="growth"
          icon={<TrendingUp size={18} />}
          label="What moved"
          title={`${topSource.value || 'Direct'} is your top source (${formatCompact(topSource.count)} visits)`}
          detail={`AI and answer-engine traffic is ${formatPercent(aiShare)} of classified visits. Bounce rate is ${formatPercent(web.bounce_rate * 100)} across ${formatNumber(web.sessions)} sessions.`}
        />
      ) : null}
      <div className="flex flex-col gap-4">
        <div className="grid grid-cols-2 gap-4 max-[980px]:grid-cols-1">
          <Panel title="Top sources"><BarRows rows={(web.referrers_by_channel.length ? web.referrers_by_channel : web.referrers).slice(0, 6)} valueHead="Source" countHead="Visits" /></Panel>
          <Panel title="Top pages"><BarRows rows={web.top_paths.slice(0, 6)} valueHead="Path" countHead="Views" mono /></Panel>
        </div>
        <div className="grid grid-cols-2 gap-4 max-[980px]:grid-cols-1">
          {/* countHead is "Pageviews": traffic_by_class counts user.pageview rows,
              not people. It used to say "Visitors" while the stat strip above it
              said something different for the same window — two numbers, one
              label, on one screen. */}
          <Panel title="Traffic by type"><BarRows rows={web.traffic_by_class.map((c) => ({ value: c.class, count: c.count })).slice(0, 6)} valueHead="Type" countHead="Pageviews" /></Panel>
          <Panel title="AI-cited pages"><BarRows rows={web.ai_top_paths.slice(0, 6)} valueHead="Path" countHead="AI views" mono empty="No AI crawler traffic yet" /></Panel>
        </div>
        {/* Shown while a platform is selected as well, even though the split then
            has one row: the row is the selected state and clicking it clears the
            filter, so the panel stays the way back out. */}
        {web.traffic_by_platform.length > 1 || applied.platform ? (
          <Panel title="By platform">
            <PlatformRows
              rows={web.traffic_by_platform}
              selected={applied.platform}
              onSelect={(platform) => void refresh({ ...applied, platform: applied.platform === platform ? '' : platform })}
            />
          </Panel>
        ) : null}
      </div>
    </AppShell>
  );
}

// PlatformRows splits the audience by the app it came from. It shows people and
// pageviews side by side, under their own headings, because they are different
// questions and a single "count" behind one header is what made this panel's
// sibling untrustworthy. Clicking a row scopes the whole page to that platform;
// clicking it again clears the filter, so the panel is also the way back out.
function PlatformRows({
  rows,
  selected,
  onSelect,
}: {
  rows: Array<{ platform: string; visitors: number; pageviews: number }>;
  selected: string;
  onSelect: (platform: string) => void;
}) {
  const max = rows.reduce((m, r) => Math.max(m, r.visitors), 0) || 1;
  const columns = [
    {
      key: 'platform',
      header: 'Platform',
      align: 'start' as const,
      renderCell: (row: { platform: string; visitors: number; pageviews: number }) => (
        <button
          className={`flex w-full items-center gap-2 text-start ${row.platform === selected ? 'text-[var(--color-text-primary)]' : ''}`}
          aria-pressed={row.platform === selected}
          onClick={() => onSelect(row.platform)}
        >
          <span
            className="h-1.5 rounded-[3px] bg-[color-mix(in_srgb,var(--data)_55%,transparent)]"
            style={{ width: Math.max(6, Math.round((row.visitors / max) * 88)) }}
          />
          <span>{platformLabel(row.platform)}</span>
        </button>
      ),
    },
    {
      key: 'visitors',
      header: 'People',
      align: 'end' as const,
      renderCell: (row: { visitors: number }) => <span className="font-mono tabular-nums">{formatNumber(row.visitors)}</span>,
    },
    {
      key: 'pageviews',
      header: 'Pageviews',
      align: 'end' as const,
      renderCell: (row: { pageviews: number }) => <span className="font-mono tabular-nums">{formatNumber(row.pageviews)}</span>,
    },
  ];
  return <Table data={rows} columns={columns} density="compact" />;
}
