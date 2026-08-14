'use client';

import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { HStack } from '@astryxdesign/core/HStack';
import { formatEvents, type UsageMeter } from '@/lib/plans';

// PlanMeter is a bar *and* a number, never a bar alone: color is never the only
// signal (DESIGN.md §Accessibility), and a 1M ceiling means the interesting
// values are seven figures — so the label is compact with the exact count kept
// in the title, and set in tabular-nums so it doesn't jitter as it climbs.
//
// Astryx has no progress primitive in the inventory and this meter appears in
// three places (settings, pricing, the upgrade sheet), so it is a composition of
// existing primitives rather than a one-off in each.
export function PlanMeter({ meter, label }: { meter: UsageMeter; label: string }) {
  const pct = Math.round(meter.ratio * 100);
  const tone = meter.overCeiling ? 'var(--danger)' : meter.nearCeiling ? 'var(--warning)' : 'var(--primary)';
  return (
    <VStack gap={1.5} align="stretch">
      <HStack justify="between" align="center" gap={3}>
        <Text type="supporting">{label}</Text>
        {/* The compact label is the readable one; the exact count rides the
            title so a user checking their own bill can get the real number. */}
        <span title={`${meter.used.toLocaleString('en-US')} of ${meter.ceiling.toLocaleString('en-US')}`}>
          <Text weight="medium" hasTabularNumbers>
            {formatEvents(meter.used)} / {formatEvents(meter.ceiling)}
          </Text>
        </span>
      </HStack>
      {/* role="meter" carries the same reading to a screen reader that the bar
          gives visually, so the number is never the sighted-only channel. */}
      <div
        role="meter"
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={`${label}: ${pct}% of the monthly ceiling used`}
        className="h-1.5 w-full overflow-hidden rounded-full bg-[var(--color-background-muted)]"
      >
        <div className="h-full rounded-full transition-[width] duration-[var(--slow)] ease-[var(--ease)]" style={{ width: `${pct}%`, background: tone }} />
      </div>
    </VStack>
  );
}

// MeterUnavailable is the degraded state: when the usage query fails the meter
// becomes a sentence, not a spinner and not a zero. Showing "0 / 1M" for a
// failed fetch would be a lie about the user's own account.
export function MeterUnavailable({ label }: { label: string }) {
  return (
    <VStack gap={1.5} align="stretch">
      <Text type="supporting">{label}</Text>
      <Text type="supporting">Usage unavailable — retrying.</Text>
    </VStack>
  );
}
