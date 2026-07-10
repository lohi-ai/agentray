'use client';

import { useRouter } from 'next/navigation';
import { Sparkles, TrendingUp } from 'lucide-react';
import { formatCompact, formatDuration, formatNumber, formatPercent } from '@/lib/format';
import { useWebAnalytics } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { FilterBar } from '@/modules/shared/components/filter-bar';
import { BarRows, Button, Callout, Intro, Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';

export function WebAnalyticsPage() {
  const router = useRouter();
  const web = useWebAnalytics();

  if (!web) {
    return (
      <AppShell active="traffic">
        <Intro title="Traffic" sub="Where visitors come from and which sources are worth more." />
        <Loading label="Loading traffic…" />
      </AppShell>
    );
  }

  const totalClass = web.traffic_by_class.reduce((sum, c) => sum + c.count, 0) || 1;
  const aiCount = web.traffic_by_class.filter((c) => /ai|crawler|assistant|bot/i.test(c.class)).reduce((sum, c) => sum + c.count, 0);
  const aiShare = (aiCount / totalClass) * 100;
  const topSource = web.referrers_by_channel[0] || web.referrers[0];

  return (
    <AppShell active="traffic">
      <Intro title="Traffic" sub="Where visitors come from and which sources are worth more." action={<Button variant="agent" icon={<Sparkles size={15} />} onClick={() => router.push('/chat')}>Ask about traffic</Button>} />
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
      <div className="flex flex-col gap-[14px]">
        <div className="grid grid-cols-2 gap-[14px] max-[980px]:grid-cols-1">
          <Panel title="Top sources"><BarRows rows={(web.referrers_by_channel.length ? web.referrers_by_channel : web.referrers).slice(0, 6)} valueHead="Source" countHead="Visits" /></Panel>
          <Panel title="Top pages"><BarRows rows={web.top_paths.slice(0, 6)} valueHead="Path" countHead="Views" mono /></Panel>
        </div>
        <div className="grid grid-cols-2 gap-[14px] max-[980px]:grid-cols-1">
          <Panel title="Traffic by type"><BarRows rows={web.traffic_by_class.map((c) => ({ value: c.class, count: c.count })).slice(0, 6)} valueHead="Type" countHead="Visitors" /></Panel>
          <Panel title="AI-cited pages"><BarRows rows={web.ai_top_paths.slice(0, 6)} valueHead="Path" countHead="AI views" mono empty="No AI crawler traffic yet" /></Panel>
        </div>
      </div>
    </AppShell>
  );
}
