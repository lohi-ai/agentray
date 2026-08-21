'use client';

import { ListChecks, MessageSquareText, Target, Waypoints } from 'lucide-react';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Card } from '@astryxdesign/core/Card';
import { ChatMessage, ChatMessageBubble, ChatToolCalls } from '@astryxdesign/core/Chat';
import { Markdown } from '@astryxdesign/core/Markdown';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { Text } from '@astryxdesign/core/Text';

// AuthValue is the companion column on the signed-out door. The auth Card is a
// form and stays one; this is the only thing on the page that tells a stranger
// what they are signing up for. Every claim here is something the shipped
// product does — the wording of the answer mock is lifted from
// `writtenOpinion` in web/lib/ia.ts, not written for the page. Do not add a
// price, a trial, a logo, or a testimonial: this instance may be self-hosted,
// where all four would be a lie. For the same reason nothing here promises a
// demo: the shared demo project is opt-in per instance
// (AGENTRAY_DEMO_PROJECT_ID, internal/dataplane/store/demo.go) and is absent by
// default, so "you'll land in a live funnel" is not a promise this page can keep.

// No Heading in this column, deliberately. The page's h1 lives inside the auth
// card ("Create your workspace" / "Welcome back") and this column renders
// *before* it in the DOM on both layouts, so a Heading here would either
// compete for h1 or put an h2 above the h1. `display-3` is Astryx's marketing
// headline type (astryx docs typography) and carries the size without the
// document-outline claim.

const CLAIMS = [
  {
    icon: MessageSquareText,
    title: 'Ask in plain language.',
    detail: 'Ask “which feature drives retention?” — it writes the SQL and runs it.',
  },
  {
    icon: ListChecks,
    title: 'It shows its work.',
    detail: 'Every query streams as it runs, so you see what it looked at.',
  },
  {
    icon: Target,
    title: 'It argues against busywork.',
    detail: 'A real answer ends: “Don’t add a new dashboard — change the product.”',
  },
] as const;

export function AuthValue() {
  return (
    // Tighter vertical rhythm below `lg`, where this column sits ABOVE the form
    // and every pixel it spends is a pixel between a stranger and the email
    // field. The row layout at `lg` is the auth screen's; `flex-1 min-w-0` is
    // what makes this the growing column there.
    <VStack gap={4} align="stretch" className="w-full lg:flex-1 lg:min-w-0 lg:gap-6">
      <HStack gap={2} align="center">
        <span
          className="grid size-8 flex-none place-items-center rounded-[var(--radius-lg)]"
          style={{ background: 'color-mix(in srgb, var(--primary) 16%, transparent)', color: 'var(--primary)' }}
          aria-hidden
        >
          <Waypoints size={18} />
        </span>
        <Text type="body" weight="medium">AgentRay</Text>
      </HStack>

      <VStack gap={3} align="start">
        <Text type="display-3" as="p" textWrap="balance">
          Ask which step is losing people.
        </Text>
        <Text type="supporting">
          A product analyst that reads your event data. Ask in the chat and it answers with the
          query it ran, the number it found, and what it would change.
        </Text>
      </VStack>

      <VStack gap={3} align="stretch" className="lg:gap-4">
        {CLAIMS.map(({ icon: Icon, title, detail }) => (
          <HStack key={title} gap={3} align="start">
            <span
              className="grid size-6 flex-none place-items-center rounded-[var(--radius-md)]"
              style={{ background: 'color-mix(in srgb, var(--primary) 12%, transparent)', color: 'var(--primary)' }}
              aria-hidden
            >
              <Icon size={14} />
            </span>
            <VStack gap={0.5} align="start">
              <Text type="body" weight="medium">{title}</Text>
              <Text type="supporting">{detail}</Text>
            </VStack>
          </HStack>
        ))}
      </VStack>

      {/* The mock is the fourth claim — that the agent names what it cannot see —
          shown rather than stated, so no bullet above repeats it. Hidden below
          `lg`: on a phone this column sits ABOVE the form, and the whole point of
          putting it there is defeated if the email field lands a screen and a
          half down. */}
      <AnswerPreview />
    </VStack>
  );
}

// The shipped answer, rendered with the shipped components. This is the same
// Astryx Chat bubble the transcript uses (modules/chat/chat-parts.tsx), so it
// carries the product's real surface, spacing and tokens instead of being a
// drawing of one — and it changes with the theme for free.
//
// Fully aria-hidden: it is an illustration of an answer, not an answer, and a
// screen reader reading a fabricated funnel result as if it were this
// workspace's data would be worse than silence. Nothing inside is focusable —
// ChatToolCalls only renders its expand control for a group of two or more, so
// the single call below is an inert row.
function AnswerPreview() {
  return (
    <VStack gap={2} align="stretch" className="hidden lg:flex" aria-hidden>
      <Text type="supporting">Example — a signup-funnel answer</Text>
      {/* A muted Card frames the bubble as a quoted example rather than a second
          voice on the page. The bubble's own `max(80%, 280px)` cap is right in a
          full-width transcript and wrong in a 400px column — it would set the
          answer in a 300px measure — so it is released to the card width here. */}
      <Card variant="muted" padding={4}>
        <div className="[&_.astryx-chat-message-bubble]:!max-w-full">
          <ChatMessage
            sender="assistant"
            avatar={<Avatar name="Growth Lead" size="small" status={<StatusDot variant="success" label="Online" />} />}
          >
            <ChatMessageBubble variant="ghost">
              <VStack gap={1} align="stretch">
                <Text type="supporting" weight="semibold" color="secondary">Growth Lead</Text>
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
                  <Markdown headingLevelStart={3}>{ANSWER}</Markdown>
                </VStack>
              </VStack>
            </ChatMessageBubble>
          </ChatMessage>
        </div>
      </Card>
    </VStack>
  );
}

// Shape and wording follow `writtenOpinion` (web/lib/ia.ts): the widest gap as a
// percentage, the two people counts it compared, then the caveat that names what
// the comparison does NOT prove. The event names are the product's own funnel
// vocabulary — `DEFAULT_FUNNEL_STEPS` and the `FUNNEL_STAGES` matchers in the
// same file. Labelled "Example" on the page because it is one: it is what an
// answer looks like, not a number from this instance.
const ANSWER = [
  '**activation → user.conversion is the widest gap (25%).**',
  '',
  '28 people fired `activation`; 7 people fired `user.conversion`. That’s the widest gap in your catalog.',
  '',
  'I’m comparing two people counts, not tracing one cohort — open the funnel to confirm the same people did both steps in that order.',
].join('\n');
