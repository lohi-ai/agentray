'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { Check, ShieldCheck } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useUpgradeRequest, useWorkspacePlan } from '@/modules/app/hooks';
import { MeterUnavailable, PlanMeter } from '@/modules/shared/components/plan-meter';
import { Button, Callout, Panel } from '@/modules/shared/components/signal-primitives';
import { nextPlan, planByID, usageMeter } from '@/lib/plans';
import { UpgradeSheet } from './upgrade-sheet';

// PlanTab answers two questions and no more: what am I on, and how much of it
// have I used this month. There is no persistent upgrade bar anywhere in the
// product — the one nag fires here, once, at 80% of the ceiling.
export function PlanTab() {
  const router = useRouter();
  const { hosted, plan: planID, usage, loading, failed } = useWorkspacePlan();
  const { request } = useUpgradeRequest();
  const [sheetOpen, setSheetOpen] = useState(false);
  const plan = planByID(planID);
  const upgrade = nextPlan(planID);
  const meter = usageMeter(usage?.event_count ?? 0, plan);

  // Self-host has no plan to be on and no ceiling to hit. Showing it a meter
  // against a number it cannot buy past would be a lie about its own limits.
  if (!hosted) {
    return (
      <Grid columns={{ minWidth: 440, max: 2 }} gap={4}>
        <Panel title="Plan">
          <VStack gap={3} align="start">
            <Badge variant="green" label="Self-hosted · unlimited" />
            <Text type="supporting">
              You are running AgentRay on your own infrastructure under the MIT licence. There is no
              usage ceiling, no plan, and nothing to buy — your limits are your own machines.
            </Text>
            <HStack gap={1.5} align="center">
              <ShieldCheck size={15} className="text-success" aria-hidden />
              <Text type="supporting">Your events never leave your infrastructure.</Text>
            </HStack>
          </VStack>
        </Panel>
      </Grid>
    );
  }

  return (
    <>
      {sheetOpen && upgrade ? <UpgradeSheet plan={upgrade.id} onClose={() => setSheetOpen(false)} /> : null}

      {/* The whole nag budget: one card, past 80%, stating the projected date and
          promising ingestion will not stop. With a 1M free ceiling this is
          genuinely rare — it only reaches someone whose product is working. */}
      {meter.nearCeiling && upgrade ? (
        <Callout
          tone="warn"
          icon={<Check size={16} />}
          label="Heads up"
          title={meter.overCeiling
            ? `You are past the ${plan.name} plan's monthly events`
            : `You are at ${Math.round(meter.ratio * 100)}% of this month's events`}
          detail={`${meter.projectedFullOn
            ? `At this rate you reach the ceiling around ${meter.projectedFullOn.toLocaleDateString('en-US', { month: 'short', day: 'numeric' })}. `
            : ''}Nothing stops: we keep ingesting your events either way. ${upgrade.name} raises the ceiling when you want it raised.`}
          action={<Button variant="primary" size="sm" onClick={() => setSheetOpen(true)}>See {upgrade.name}</Button>}
        />
      ) : null}

      <Grid columns={{ minWidth: 440, max: 2 }} gap={4}>
        <Panel
          title="Plan"
          action={<Button variant="outline" size="sm" onClick={() => router.push('/pricing')}>Compare plans</Button>}
        >
          <VStack gap={3} align="start">
            <HStack gap={2} align="center">
              <Badge variant={plan.id === 'free' ? 'neutral' : 'green'} label={plan.name} />
              <Text type="supporting">{plan.tagline}</Text>
            </HStack>
            <VStack gap={1} align="start">
              {plan.features.map((feature) => (
                <HStack key={feature} gap={1.5} align="center">
                  <Check size={14} className="text-success" aria-hidden />
                  <Text type="supporting">{feature}</Text>
                </HStack>
              ))}
            </VStack>
            {request ? (
              // Say "we heard you" rather than inviting the same request twice.
              <Text type="supporting">
                {`You asked about ${planByID(request.plan).name} on ${new Date(request.created_at).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })}. We will come back to you at ${request.email}.`}
              </Text>
            ) : upgrade ? (
              <Button variant="primary" size="sm" onClick={() => setSheetOpen(true)}>Move to {upgrade.name}</Button>
            ) : null}
          </VStack>
        </Panel>

        <Panel title="This month">
          {failed ? (
            <MeterUnavailable label="Events ingested" />
          ) : loading ? (
            // A skeleton dash, not a spinner: the layout must not jump when the
            // number lands, and a spinner where a number belongs reads as broken.
            <MeterUnavailable label="Events ingested" />
          ) : (
            <VStack gap={4} align="stretch">
              <PlanMeter meter={meter} label="Events ingested" />
              <Text type="supporting">
                {`Counted from the 1st of this month across ${usage?.project_count ?? 0} project${(usage?.project_count ?? 0) === 1 ? '' : 's'}. Agent runs are not metered — you bring your own AI key, so you already pay your model provider directly.`}
              </Text>
            </VStack>
          )}
        </Panel>
      </Grid>
    </>
  );
}
