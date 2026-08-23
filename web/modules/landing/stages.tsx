'use client';

import type { ReactNode } from 'react';
import { CalendarClock, MessageSquare, Webhook } from 'lucide-react';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Badge } from '@astryxdesign/core/Badge';
import { ChatMessage, ChatMessageBubble, ChatToolCalls } from '@astryxdesign/core/Chat';
import { Markdown } from '@astryxdesign/core/Markdown';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { Text } from '@astryxdesign/core/Text';
import {
  CHANNELS,
  EXAMPLE_ANSWER,
  EXAMPLE_BADGE,
  EXAMPLE_BRIEF,
  EXAMPLE_BRIEF_NOTE,
  EXAMPLE_EVENTS,
  EXAMPLE_FUNNEL,
  EXAMPLE_GAP,
  EXAMPLE_MEMORY,
} from '@/modules/landing/content';

// The product surfaces the landing page shows instead of screenshots.
//
// This is the technique the old AuthValue already used and the reason the page
// needs no image assets: every stage below is built from the SAME Astryx
// components the shipped screens use, so it carries the product's real surface,
// spacing and tokens rather than being a drawing of one — and it follows the
// theme for free. `web/public/` stays empty.
//
// Every stage is fully aria-hidden. It is an illustration of an answer, not an
// answer, and a screen reader reading a fabricated funnel result as if it were
// this workspace's data would be worse than silence. Nothing inside is
// focusable — ChatToolCalls only renders its expand control for a group of two
// or more, so the single call below is an inert row.

type StageTone = 'primary' | 'data' | 'agent' | 'danger';

// NEW: LandingStage — the framed holder every product surface sits in. Astryx
// Card cannot bleed a surface to its own edge (the header strip has to touch
// the frame), and six sections need the identical frame, so it is one component
// rather than a per-section style.
export function LandingStage({
  tone,
  title,
  children,
}: {
  tone: StageTone;
  title: string;
  children: ReactNode;
}) {
  return (
    <div className="lp-stage" aria-hidden>
      <HStack gap={2} align="center" className="lp-stage-bar">
        <span className="lp-dot" style={{ background: `var(--${tone})` }} />
        <Text type="supporting">{title}</Text>
        <span className="ms-auto">
          <Badge variant="neutral" label={EXAMPLE_BADGE} />
        </span>
      </HStack>
      <div className="lp-stage-body">{children}</div>
    </div>
  );
}

// 01 · measure — the event catalog the agent reads. Mono type and tabular
// numbers because this is the one place on the page where data benefits.
export function EventCatalogStage() {
  return (
    <LandingStage tone="data" title="Event catalog · last 7 days">
      <VStack gap={0.5} align="stretch" className="lp-zebra">
        {EXAMPLE_EVENTS.map((event) => (
          <HStack key={event.name} gap={3} align="center" justify="between">
            <Text type="code">{event.name}</Text>
            <Text type="code" color="secondary" hasTabularNumbers>
              {event.count}
            </Text>
          </HStack>
        ))}
      </VStack>
    </LandingStage>
  );
}

// 02 · diagnose — the same funnel, with the widest gap called out. The gap note
// is the point: a bar chart alone would be the commodity half.
export function FunnelStage() {
  return (
    <LandingStage tone="danger" title="Funnel · last 7 days">
      <VStack gap={3} align="stretch">
        {EXAMPLE_FUNNEL.map((step) => (
          <VStack key={step.name} gap={1.5} align="stretch">
            <HStack gap={3} align="center" justify="between">
              <Text type="code">{step.name}</Text>
              <Text type="code" color="secondary" hasTabularNumbers>
                {step.people.toLocaleString('en-US')} people
              </Text>
            </HStack>
            <div className="lp-bar-track">
              <div
                className="lp-bar-fill"
                data-weakest={step.weakest}
                style={{ width: `${step.width}%` }}
              />
            </div>
          </VStack>
        ))}
        <VStack gap={1} align="start" className="lp-gap-note">
          <Text type="supporting" weight="semibold" color="primary">
            {EXAMPLE_GAP.title}
          </Text>
          <Text type="supporting">{EXAMPLE_GAP.detail}</Text>
        </VStack>
      </VStack>
    </LandingStage>
  );
}

// 03 · test — the brief, not the chat turn. The hero already shows the answer
// bubble; showing it again here would make the page repeat its own picture.
export function TestBriefStage() {
  return (
    <LandingStage tone="agent" title="Growth Lead · this week’s test">
      <VStack gap={0} align="stretch">
        {EXAMPLE_BRIEF.map((row) => (
          <VStack key={row.id} gap={0.5} align="start" className="lp-brief-row">
            <Text type="supporting" color="secondary">
              {row.label}
            </Text>
            <Text type="body">{row.detail}</Text>
          </VStack>
        ))}
        <VStack gap={0} align="start" className="lp-brief-row">
          <Text type="supporting">{EXAMPLE_BRIEF_NOTE}</Text>
        </VStack>
      </VStack>
    </LandingStage>
  );
}

// 04 · learn — the claim a saved view structurally cannot make. Last cycle's
// result sitting inside next cycle's reasoning is the whole differentiator, so
// it gets a surface of its own rather than a sentence.
export function MemoryStage() {
  return (
    <LandingStage tone="primary" title="Growth Lead · what it carries in">
      <VStack gap={2} align="stretch">
        {EXAMPLE_MEMORY.map((memo) => (
          <HStack key={memo.id} gap={3} align="start" className="lp-memo" data-current={memo.current}>
            <Text type="code" color="disabled" className="whitespace-nowrap">
              {memo.when}
            </Text>
            <VStack gap={2} align="start" className="min-w-0">
              <Text type="supporting" color="primary">
                {memo.detail}
              </Text>
              <Badge variant={memo.current ? 'purple' : 'success'} label={memo.state} />
            </VStack>
          </HStack>
        ))}
      </VStack>
    </LandingStage>
  );
}

// The hero surface: the shipped answer, rendered with the shipped chat
// components. It shows the query it ran and the number it found, which is what
// earns every claim on the page — and it carries the one sentence no claim
// states, "don't add a new dashboard — change the product", demonstrated
// instead of boasted.
export function AnswerStage() {
  return (
    <LandingStage tone="primary" title="Growth Lead · working now">
      {/* The bubble's own `max(80%, 280px)` cap is right in a full-width
          transcript and wrong here, where the stage is already the measure. */}
      <div className="[&_.astryx-chat-message-bubble]:!max-w-full">
        <ChatMessage
          sender="assistant"
          avatar={<Avatar name="Growth Lead" size="small" status={<StatusDot variant="success" label="Online" />} />}
        >
          <ChatMessageBubble variant="ghost">
            <VStack gap={1} align="stretch">
              <Text type="supporting" weight="semibold" color="secondary">
                Growth Lead
              </Text>
              <VStack gap={3} align="stretch">
                <ChatToolCalls
                  calls={[
                    {
                      key: 'run_sql',
                      name: 'Queried data',
                      node: 'run_sql',
                      target: 'people per step',
                      duration: '1.2s',
                      status: 'complete',
                    },
                  ]}
                />
                <Markdown headingLevelStart={3}>{EXAMPLE_ANSWER}</Markdown>
              </VStack>
            </VStack>
          </ChatMessageBubble>
        </ChatMessage>
      </div>
    </LandingStage>
  );
}

const CHANNEL_ICONS = {
  schedule: CalendarClock,
  webhook: Webhook,
  chat: MessageSquare,
} as const;

// Section 4 · the channels that start a run without a person in the loop. This
// is the only proof on the page that the headline is literal rather than a
// figure of speech, which is why it earns a section of its own.
export function ChannelsStage() {
  return (
    <LandingStage tone="primary" title="Channels · what starts a run">
      <VStack gap={0.5} align="stretch" className="lp-zebra">
        {CHANNELS.map((channel) => {
          const Icon = CHANNEL_ICONS[channel.icon];
          return (
            <HStack key={channel.id} gap={3} align="center">
              <span className="lp-tile">
                <Icon size={14} />
              </span>
              <VStack gap={0} align="start" className="min-w-0">
                <Text type="supporting" color="primary" weight="medium">
                  {channel.title}
                </Text>
                <Text type="supporting">{channel.detail}</Text>
              </VStack>
              <span className="ms-auto">
                <Badge
                  variant={channel.tone === 'success' ? 'success' : 'neutral'}
                  label={channel.status}
                />
              </span>
            </HStack>
          );
        })}
      </VStack>
    </LandingStage>
  );
}

// Keyed by LoopStepId so scroll-steps.tsx renders the same stage inline on a
// phone and inside the sticky panel on a desktop — one source, two renderings,
// which is what stops the two from drifting.
export const LOOP_STAGES = {
  measure: EventCatalogStage,
  diagnose: FunnelStage,
  design: TestBriefStage,
  learn: MemoryStage,
} as const;
