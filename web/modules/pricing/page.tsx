'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { useQuery } from '@tanstack/react-query';
import { Check, ShieldCheck } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { Card } from '@astryxdesign/core/Card';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { AgentRayAPI, type OverviewResult } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { formatCompact } from '@/lib/format';
import { PLANS, formatEvents, formatPrice, planByID, usageMeter, type Plan } from '@/lib/plans';
import { useUpgradeRequest, useWorkspacePlan } from '@/modules/app/hooks';
import { formatMoney } from '@/modules/overview/page';
import { AppShell } from '@/modules/shared/components/app-shell';
import { PlanMeter } from '@/modules/shared/components/plan-meter';
import { Button } from '@/modules/shared/components/signal-primitives';
import { UpgradeSheet } from '@/modules/settings/upgrade-sheet';
import { AutoGrid } from '@/modules/shared/components/page-shell';

// Solo is pre-selected — not because it is the most expensive thing we can
// sell, but because it is the one a single founder actually needs. A pricing
// page with no recommendation makes the reader do work they came here to avoid.
const RECOMMENDED: Plan['id'] = 'solo';

function TierCard({
  plan,
  current,
  recommended,
  onPick,
}: {
  plan: Plan;
  current: boolean;
  recommended: boolean;
  onPick: () => void;
}) {
  return (
    <Card
      padding={4}
      className={`h-full ${recommended ? 'ring-1 ring-[var(--primary)]' : ''}`}
    >
      <VStack gap={4} align="stretch" className="h-full">
        <VStack gap={2} align="start">
          <HStack gap={2} align="center" wrap="wrap">
            <Heading level={4}>{plan.name}</Heading>
            {recommended ? <Badge variant="green" label="Most founders" /> : null}
            {current ? <Badge variant="neutral" label="Your plan" /> : null}
          </HStack>
          <HStack gap={2} align="center">
            <Text weight="semibold" hasTabularNumbers className="text-[length:var(--font-size-2xl)] leading-none tracking-[-0.02em]">
              {formatPrice(plan, 'en')}
            </Text>
            {plan.usdPerMonth > 0 ? <Text type="supporting">/ month</Text> : null}
          </HStack>
          {/* VND is shown as the localized equivalent, never as the billed
              amount — USD is the billing currency, so a rate move can never
              become a pricing bug. */}
          {plan.usdPerMonth > 0 ? (
            <Text type="supporting">≈ {formatPrice(plan, 'vi')} / tháng</Text>
          ) : null}
          <Text type="supporting">{plan.tagline}</Text>
        </VStack>

        <VStack gap={2} align="start" className="flex-1">
          <HStack gap={2} align="center">
            <Check size={14} className="text-success" aria-hidden />
            <Text type="supporting" weight="medium">{formatEvents(plan.eventsPerMonth)} events / month</Text>
          </HStack>
          {plan.features.map((feature) => (
            <HStack key={feature} gap={2} align="center">
              <Check size={14} className="text-success" aria-hidden />
              <Text type="supporting">{feature}</Text>
            </HStack>
          ))}
          <HStack gap={2} align="center">
            <Check size={14} className="text-success" aria-hidden />
            <Text type="supporting">{plan.support} support</Text>
          </HStack>
        </VStack>

        {current ? (
          <Button variant="outline" disabled>You are on this plan</Button>
        ) : plan.usdPerMonth === 0 ? (
          <Button variant="ghost" disabled>Included</Button>
        ) : (
          <Button variant={recommended ? 'primary' : 'outline'} onClick={onPick}>Move to {plan.name}</Button>
        )}
      </VStack>
    </Card>
  );
}

/**
 * What the project already measured — the reason to pay, stated in the
 * customer's own numbers rather than adjectives. Renders nothing when the
 * overview has no usable reading (a project with no events yet has no value
 * to show, and a card of empty states is worse than no card).
 */
function ValueDeliveredCard() {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['overview', projectID, '30d', ''],
    queryFn: () => new AgentRayAPI(projectID!).overview('30d', ''),
    enabled: !!projectID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
  const res: OverviewResult | undefined = query.data;
  if (!res) return null;

  const rows: { label: string; value: string }[] = [];
  const detail = res.metrics.revenue_detail;
  if (res.metrics.revenue.state === 'ok' && detail?.currency) {
    rows.push({ label: 'Net revenue', value: `${formatMoney(detail.net)} ${detail.currency}` });
  }
  if (res.metrics.paying_users.state === 'ok' && res.metrics.paying_users.value !== undefined) {
    rows.push({ label: 'Paying people', value: formatCompact(res.metrics.paying_users.value) });
  }
  if (res.metrics.active_users.state === 'ok' && res.metrics.active_users.value !== undefined) {
    rows.push({ label: 'Active people', value: formatCompact(res.metrics.active_users.value) });
  }
  const d7 = res.paid_conversion?.d7;
  if (d7?.state === 'ok' && d7.eligible > 0) {
    rows.push({
      label: 'Download→paid D7',
      value: `${(d7.rate * 100).toFixed(1)}%`,
    });
  }
  if (rows.length === 0) return null;

  return (
    <Card padding={4}>
      <VStack gap={3} align="stretch">
        <HStack gap={2} align="center" wrap="wrap">
          <Text weight="medium">Your numbers, last 30 days</Text>
          <Badge variant="neutral" label="measured" />
        </HStack>
        <HStack gap={4} wrap="wrap">
          {rows.map((row) => (
            <VStack key={row.label} gap={0.5} align="start">
              <Text type="supporting">{row.label}</Text>
              <Text weight="semibold" hasTabularNumbers className="text-[length:var(--font-size-xl)] leading-tight tracking-[-0.02em]">{row.value}</Text>
            </VStack>
          ))}
        </HStack>
        <Text type="supporting">
          These are your events, already counted. A plan only changes how much history you keep —
          the numbers above are already yours. <Link href="/overview" className="underline">Open the overview</Link>
        </Text>
      </VStack>
    </Card>
  );
}

export function PricingPage() {
  const router = useRouter();
  const { hosted, plan: planID, usage, failed, loading } = useWorkspacePlan();
  const { request } = useUpgradeRequest();
  const [picked, setPicked] = useState<string | null>(null);
  const current = planByID(planID);
  const meter = usageMeter(usage?.event_count ?? 0, current);

  // A self-hosted instance has no plan to buy. Rather than 404, say the honest
  // thing and send them back — this route is only ever reached by a stale link
  // or a typed URL, since navItemsFor already drops the nav item.
  if (!hosted) {
    return (
      <AppShell title="Plans" sub="This instance is self-hosted.">
        <Card padding={4}>
          <VStack gap={3} align="start">
            <Badge variant="green" label="Self-hosted · unlimited · MIT" />
            <Text type="supporting">
              You are running AgentRay on your own machines. There is no plan, no ceiling and nothing
              to buy — and your events never leave your infrastructure.
            </Text>
            <Button variant="outline" onClick={() => router.push('/settings')}>Back to settings</Button>
          </VStack>
        </Card>
      </AppShell>
    );
  }

  return (
    <AppShell
      title="Plans"
      sub="Meter the events. Never the questions — you bring your own AI key, so asking is always unlimited."
    >
      {picked ? <UpgradeSheet plan={picked} onClose={() => setPicked(null)} /> : null}

      {/* Where you actually are, before the ladder. A pricing page that opens
          with tiers makes the reader guess which one is theirs. */}
      {!failed && !loading ? (
        <Card padding={4}>
          <VStack gap={3} align="stretch">
            <HStack gap={2} align="center" wrap="wrap">
              <Text weight="medium">You are on {current.name}</Text>
              <Badge variant="neutral" label="this month" />
            </HStack>
            <PlanMeter meter={meter} label="Events ingested" />
          </VStack>
        </Card>
      ) : null}

      <ValueDeliveredCard />

      <AutoGrid min={280} max={3} gap={4}>
        {PLANS.map((plan) => (
          <TierCard
            key={plan.id}
            plan={plan}
            current={plan.id === current.id}
            recommended={plan.id === RECOMMENDED && plan.id !== current.id}
            onPick={() => setPicked(plan.id)}
          />
        ))}
      </AutoGrid>

      {request ? (
        <Text type="supporting" className="block">
          {`You already asked about ${planByID(request.plan).name}. We will come back to you at ${request.email}.`}
        </Text>
      ) : null}

      {/* The honest answer to "why trust a solo builder with my data" — and the
          reason the hosted plans can afford to be this cheap. It stays on the
          hosted page, not only the self-host one. */}
      <Card padding={4} className="mt-2">
        <HStack gap={3} align="center" wrap="wrap">
          <ShieldCheck size={18} className="text-success" aria-hidden />
          <VStack gap={0.5} align="start" className="flex-1 min-w-[240px]">
            <Text weight="medium">You can always leave with your data.</Text>
            <Text type="supporting">
              AgentRay is MIT-licensed and runs with <code className="font-mono">docker compose up</code>.
              Self-hosting has no ceiling and no plan — the hosted tiers pay for us running it, not for
              access to the product.
            </Text>
          </VStack>
        </HStack>
      </Card>

      <Text type="supporting" className="block">
        Prices in USD. We are not taking cards yet — picking a plan puts you on the list and changes
        nothing about your account today.
      </Text>
    </AppShell>
  );
}
