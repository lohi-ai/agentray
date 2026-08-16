'use client';

import { Badge } from '@astryxdesign/core/Badge';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import type { MeasuredTest } from '@/lib/api';
import { Button, StatusPill } from '@/modules/shared/components/signal-primitives';
import { countsLine, primaryAction, progressPct, stateOf, STATE_LABEL, STATE_PILL, STATE_TONE } from '../lib/prototype';

// PrototypeCard is one bet. The left rule carries the verdict as color and the
// pill carries it as words — color never carries it alone.

export function PrototypeCard({
  test,
  waitlistCount,
  busy,
  onOpen,
  onAct,
}: {
  test: MeasuredTest;
  waitlistCount: number;
  busy: boolean;
  onOpen: () => void;
  onAct: () => void;
}) {
  const state = stateOf(test);
  const pct = progressPct(test);
  const tone = STATE_TONE[state];
  const action = primaryAction(test);

  return (
    <div className="border-s-2 py-1 ps-3" style={{ borderInlineStartColor: tone }}>
      {/* Below md the action drops under the hypothesis — side by side, the
          sentence gets ~120px and wraps to one word per line. */}
      <HStack align="start" justify="between" gap={3} className="!flex-col md:!flex-row">
        <VStack gap={1} className="min-w-0">
          <HStack gap={2} align="center" className="flex-wrap">
            <StatusPill status={STATE_PILL[state]} label={STATE_LABEL[state]} grow={false} />
            <button type="button" onClick={onOpen} className="min-w-0 text-start">
              <Text weight="medium">{test.hypothesis}</Text>
            </button>
          </HStack>
          <HStack gap={2} align="center" className="flex-wrap">
            <Badge variant="neutral" label={<code>{test.metric_event}</code>} />
            <Text type="supporting">{countsLine(test, waitlistCount)}</Text>
          </HStack>
        </VStack>
        <Button
          variant={state === 'proposed' ? 'agent' : state === 'passed' ? 'primary' : state === 'failed' ? 'outline' : 'ghost'}
          size="sm"
          disabled={busy}
          onClick={onAct}
        >
          {busy ? 'Committing…' : action}
        </Button>
      </HStack>
      {pct !== null ? (
        <div
          className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-[var(--color-background-muted)]"
          role="progressbar"
          aria-valuenow={test.metric_count}
          aria-valuemin={0}
          aria-valuemax={test.target_count}
          aria-label={`${test.metric_count} of ${test.target_count} toward the committed threshold for ${test.metric_event}`}
        >
          <div className="h-full rounded-full" style={{ width: `${pct}%`, background: tone }} />
        </div>
      ) : null}
    </div>
  );
}
