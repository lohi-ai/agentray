'use client';

// PROTOTYPE — bs-g1cigg86. Throwaway design surface for the five new chat
// affordances. Not linked from navigation, not wired to the API, no shared
// state. `implement` rebuilds these properly inside web/modules/chat/.
// Spec: .babysit tickets/bs-g1cigg86/design.md

import { useState } from 'react';
import { Copy, Paperclip, Pencil, RotateCcw, Square, CornerDownLeft } from 'lucide-react';
import {
  ChatComposer,
  ChatMessage,
  ChatMessageBubble,
  ChatMessageList,
  ChatMessageMetadata,
  ChatSystemMessage,
  ChatToolCalls,
  type ChatToolCallItem,
} from '@astryxdesign/core/Chat';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Markdown } from '@astryxdesign/core/Markdown';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';

// The user bubble treatment copied verbatim from chat-parts.tsx:482 so the
// prototype reads as the existing screen.
const USER_BUBBLE =
  '!bg-[color-mix(in_srgb,var(--primary)_16%,var(--color-background-surface))] !text-[var(--color-text-primary)] !border !border-[color-mix(in_srgb,var(--primary)_24%,transparent)]';

// ---------------------------------------------------------------------------
// NEW: ContextMeter — §4. Astryx has no compact inline capacity gauge and
// StatsStrip is a page-level metric strip.
// ---------------------------------------------------------------------------

function ContextMeter({ percent }: { percent: number }) {
  const tone = percent >= 90 ? 'text-danger' : percent >= 75 ? 'text-warning' : 'text-muted-foreground';
  const fill = percent >= 90 ? 'bg-danger' : percent >= 75 ? 'bg-warning' : 'bg-primary';
  const label =
    percent >= 90
      ? `Context ${percent}% — older messages will be summarized soon`
      : `Context ${percent}%`;
  return (
    <div
      role="meter"
      aria-valuenow={percent}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-label={label}
      className="flex items-center gap-2"
    >
      <span className="h-[3px] w-16 overflow-hidden rounded-sm bg-surface-3">
        <span className={`block h-full ${fill}`} style={{ width: `${percent}%` }} />
      </span>
      <span className={`text-xs tabular-nums ${tone}`}>{label}</span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// §3 responsive. An Astryx ChatToolCalls row is one un-truncated flex line: at
// 390px the tool name and the target both overflow their box and push the page
// into horizontal scroll (measured — the `target` span wants 170px in a 99px
// slot). Flex children default to min-width:auto, so nothing can shrink until
// min-w-0 is forced down the subtree; then the row's own text spans truncate.
// ---------------------------------------------------------------------------

function ToolCalls(props: React.ComponentProps<typeof ChatToolCalls>) {
  return (
    <div data-proto="toolcalls" className="[&_*]:!min-w-0 [&_span]:!truncate">
      <ChatToolCalls {...props} />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Footer actions — §5. ChatMessageMetadata has no action API, only a free-form
// `footer` ReactNode slot.
// ---------------------------------------------------------------------------

function Actions({ kind }: { kind: 'user' | 'ok' | 'stopped' | 'failed' }) {
  const items =
    kind === 'user'
      ? [
          { label: 'Copy', icon: <Copy size={14} /> },
          { label: 'Edit', icon: <Pencil size={14} /> },
        ]
      : kind === 'ok'
        ? [
            { label: 'Copy', icon: <Copy size={14} /> },
            { label: 'Regenerate', icon: <RotateCcw size={14} /> },
          ]
        : kind === 'stopped'
          ? [{ label: 'Try again', icon: <RotateCcw size={14} /> }]
          : [
              { label: 'Try again', icon: <RotateCcw size={14} /> },
              { label: 'Copy error', icon: <Copy size={14} /> },
            ];
  // Always in the DOM (opacity, not conditional render) so keyboard and screen
  // readers reach them; the group-hover reveal is a pointer nicety only. On a
  // coarse pointer they are always visible and padded out to a 44px target.
  return (
    <span className="inline-flex items-center gap-1 opacity-60 transition-opacity focus-within:opacity-100 group-hover:opacity-100 [@media(pointer:coarse)]:opacity-100 [@media(pointer:coarse)]:[&_button]:min-h-11 [@media(pointer:coarse)]:[&_button]:min-w-11">
      {items.map((a) => (
        <Button key={a.label} isIconOnly size="sm" variant="ghost" label={a.label} icon={a.icon} />
      ))}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Sections
// ---------------------------------------------------------------------------

function Section({ n, title, note, children }: { n: number; title: string; note: string; children: React.ReactNode }) {
  return (
    <VStack gap={2} align="stretch">
      <VStack gap={0} align="stretch">
        <HStack gap={2} align="center">
          <Badge label={`§${n}`} variant="neutral" />
          <Heading level={2} className="!text-sm">{title}</Heading>
        </HStack>
        <Text type="supporting" color="secondary">{note}</Text>
      </VStack>
      <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-background-card)] p-4">
        {children}
      </div>
    </VStack>
  );
}

const AGENT = (
  <Avatar name="Growth Lead" size="small" status={<StatusDot variant="success" label="Online" />} />
);

// ---------------------------------------------------------------------------

export default function ChatAffordancesPrototype() {
  const [draft, setDraft] = useState('');
  const [streaming, setStreaming] = useState(true);
  const [mode, setMode] = useState<'steer' | 'followup'>('steer');
  const [pct, setPct] = useState(41);

  const runningCalls: ChatToolCallItem[] = [
    {
      key: 'call_01H8QY',            // the call id — never the array index
      name: 'Reading events',
      node: 'explore_events',
      status: 'running',
      target: 'signup_completed, last 30 days',
      resultDetail: (
        <Markdown isStreaming>{'scanning `events` …\n\n- 12,904 rows read\n- 3 cohorts matched'}</Markdown>
      ),
    },
    {
      key: 'call_01H8QZ',            // same tool, concurrent — index keying collides here
      name: 'Reading events',
      node: 'explore_events',
      status: 'running',
      target: 'checkout_started, last 30 days',
      resultDetail: <Markdown isStreaming>{'scanning `events` …'}</Markdown>,
    },
  ];

  const settledCalls: ChatToolCallItem[] = [
    {
      key: 'call_01H8QY',
      name: 'Reading events',
      node: 'explore_events',
      status: 'complete',
      target: 'signup_completed, last 30 days',
      duration: '1.2s',
      resultDetail: <Text type="supporting">12,904 events across 3 cohorts.</Text>,
    },
    {
      key: 'call_01H8R0',
      name: 'Running SQL',
      node: 'run_sql',
      status: 'error',
      target: 'billing.invoices',
      errorMessage: "Blocked — this agent isn't allowed to read billing.",
      resultDetail: (
        <Text type="supporting">
          Growth Lead&rsquo;s scopes cover product and traffic data. Ask an admin to widen them in Team → Set up.
        </Text>
      ),
    },
    {
      key: 'call_01H8R1',
      name: 'Reading events',
      node: 'explore_events',
      status: 'error',
      target: 'checkout_started, last 30 days',
      errorMessage: 'Stopped before this finished.',
    },
  ];

  return (
    <main className="mx-auto max-w-3xl px-4 py-6">
      <VStack gap={5} align="stretch">
        <VStack gap={1} align="stretch">
          <HStack gap={2} align="center">
            <Heading level={1} className="!text-lg">Chat affordances</Heading>
            <Badge label="Prototype" variant="neutral" />
          </HStack>
          <Text type="supporting" color="secondary">
            bs-g1cigg86 · five new affordances on the existing /chat screen. Design surface only —
            nothing here is wired to the API.
          </Text>
        </VStack>

        {/* ---------------------------------------------------------------- */}
        <Section n={1} title="Stopped turn" note="The streamed text stays. No assistant message is appended to history.">
          <ChatMessageList density="balanced">
            <ChatMessage sender="user">
              <ChatMessageBubble className={USER_BUBBLE}>
                Which signup source converts best this month?
              </ChatMessageBubble>
            </ChatMessage>
            <ChatMessage sender="assistant" avatar={AGENT}>
              <div className="group w-full min-w-0 [&_.astryx-chat-message-metadata]:flex-wrap">
                <ChatMessageBubble
                  variant="ghost"
                  metadata={<ChatMessageMetadata footer={<Actions kind="stopped" />} />}
                >
                  <VStack gap={3} align="stretch">
                    <Text type="supporting" weight="semibold" color="secondary">Growth Lead</Text>
                    <ToolCalls calls={settledCalls} defaultIsExpanded={false} />
                    <Markdown headingLevelStart={3}>
                      {'Organic search is your strongest signup source this month at **31%** of completed signups, up from 24% in July. Paid social is'}
                    </Markdown>
                    <ChatSystemMessage icon={<Square size={12} />}>
                      Stopped — the agent didn&rsquo;t finish this answer.
                    </ChatSystemMessage>
                  </VStack>
                </ChatMessageBubble>
              </div>
            </ChatMessage>
          </ChatMessageList>

          <div className="mt-4 border-t border-[var(--color-border)] pt-4">
            <Text type="supporting" color="secondary">Variant — stopped before any token arrived</Text>
            <ChatMessageList density="balanced">
              <ChatMessage sender="assistant" avatar={AGENT}>
                <div className="group w-full min-w-0 [&_.astryx-chat-message-metadata]:flex-wrap">
                  <ChatMessageBubble
                    variant="ghost"
                    metadata={<ChatMessageMetadata footer={<Actions kind="stopped" />} />}
                  >
                    <ChatSystemMessage icon={<Square size={12} />}>
                      Stopped before the agent replied.
                    </ChatSystemMessage>
                  </ChatMessageBubble>
                </div>
              </ChatMessage>
            </ChatMessageList>
          </div>
        </Section>

        {/* ---------------------------------------------------------------- */}
        <Section n={2} title="Steering" note="A message sent mid-answer belongs inside the turn, not after it.">
          <ChatMessageList density="balanced">
            <ChatMessage sender="user">
              <ChatMessageBubble className={USER_BUBBLE}>
                Which signup source converts best this month?
              </ChatMessageBubble>
            </ChatMessage>

            <ChatMessage sender="user">
              <VStack gap={0} align="stretch">
                <HStack gap={1} align="center" justify="end">
                  <CornerDownLeft size={12} className="text-[var(--color-text-secondary)]" />
                  <Text type="supporting" color="secondary">Steered</Text>
                </HStack>
                <ChatMessageBubble variant="ghost">
                  Only paid channels, please.
                </ChatMessageBubble>
              </VStack>
            </ChatMessage>

            <ChatMessage sender="user">
              <VStack gap={0} align="stretch">
                <HStack gap={1} align="center" justify="end">
                  <CornerDownLeft size={12} className="text-[var(--color-text-secondary)]" />
                  <Text type="supporting" color="secondary">Queued — will run after this answer</Text>
                </HStack>
                <ChatMessageBubble variant="ghost">And chart it by week.</ChatMessageBubble>
              </VStack>
            </ChatMessage>

            <ChatMessage sender="user">
              <VStack gap={0} align="stretch">
                <HStack gap={1} align="center" justify="end">
                  <CornerDownLeft size={12} className="text-[var(--color-text-secondary)]" />
                  <Text type="supporting" className="!text-[var(--danger)]">
                    Not delivered — the agent had already finished
                  </Text>
                </HStack>
                <ChatMessageBubble variant="ghost">
                  <VStack gap={2} align="stretch">
                    <span>Break it down by country too.</span>
                    <span>
                      <Button size="sm" variant="secondary" label="Send as new message" />
                    </span>
                  </VStack>
                </ChatMessageBubble>
              </VStack>
            </ChatMessage>

            <ChatMessage sender="assistant" avatar={AGENT}>
              <ChatMessageBubble variant="ghost">
                <VStack gap={3} align="stretch">
                  <Text type="supporting" weight="semibold" color="secondary">Growth Lead</Text>
                  <ToolCalls calls={runningCalls} isExpanded />
                  <Markdown headingLevelStart={3} isStreaming>
                    {'Narrowing to paid channels. Paid search is at **18%**'}
                  </Markdown>
                </VStack>
              </ChatMessageBubble>
            </ChatMessage>
          </ChatMessageList>
        </Section>

        {/* ---------------------------------------------------------------- */}
        <Section
          n={3}
          title="Tool calls, keyed by call id"
          note="Two concurrent calls to the same tool. Index keying collides here; call ids do not."
        >
          <VStack gap={4} align="stretch">
            <VStack gap={1} align="stretch">
              <Text type="supporting" color="secondary">Running — live output, group held open</Text>
              <ToolCalls calls={runningCalls} isExpanded />
            </VStack>
            <VStack gap={1} align="stretch">
              <Text type="supporting" color="secondary">Settled — ok, blocked by scope, stopped</Text>
              <ToolCalls calls={settledCalls} isExpanded />
            </VStack>
            <VStack gap={1} align="stretch">
              <Text type="supporting" color="secondary">Collapsed — the default once a turn settles</Text>
              <ToolCalls calls={settledCalls} defaultIsExpanded={false} />
            </VStack>
          </VStack>
        </Section>

        {/* ---------------------------------------------------------------- */}
        <Section n={4} title="Compaction + context" note="The server folds older turns away. Say so, quietly.">
          <ChatMessageList density="balanced">
            <ChatMessage sender="assistant" avatar={AGENT}>
              <ChatMessageBubble variant="ghost">
                Earlier: activation is up 6% week over week.
              </ChatMessageBubble>
            </ChatMessage>
            {/* Copy stays short: Astryx's Divider label does not wrap, and a
                longer string clips at 390px. */}
            <ChatSystemMessage variant="divider" className="w-full min-w-0 overflow-hidden">
              Older messages summarized
            </ChatSystemMessage>
            <ChatMessage sender="user">
              <ChatMessageBubble className={USER_BUBBLE}>What changed since then?</ChatMessageBubble>
            </ChatMessage>
          </ChatMessageList>

          <div className="mt-4 flex flex-wrap items-center gap-4 border-t border-[var(--color-border)] pt-4">
            <ContextMeter percent={pct} />
            <HStack gap={1} align="center">
              {[12, 41, 78, 92].map((v) => (
                <Button key={v} size="sm" variant={v === pct ? 'secondary' : 'ghost'} label={`${v}%`} onClick={() => setPct(v)} />
              ))}
            </HStack>
          </div>
        </Section>

        {/* ---------------------------------------------------------------- */}
        <Section n={5} title="Message actions" note="Astryx gives one free-form footer slot. These are the actions that go in it.">
          <ChatMessageList density="balanced">
            <ChatMessage sender="user">
              <div className="group w-full min-w-0 [&_.astryx-chat-message-metadata]:flex-wrap">
                <ChatMessageBubble
                  className={USER_BUBBLE}
                  metadata={<ChatMessageMetadata timestamp="09:41" footer={<Actions kind="user" />} />}
                >
                  Which signup source converts best this month?
                </ChatMessageBubble>
              </div>
            </ChatMessage>
            <ChatMessage sender="assistant" avatar={AGENT}>
              <div className="group w-full min-w-0 [&_.astryx-chat-message-metadata]:flex-wrap">
                <ChatMessageBubble
                  variant="ghost"
                  metadata={<ChatMessageMetadata timestamp="09:41" footer={<Actions kind="ok" />} />}
                >
                  <VStack gap={2} align="stretch">
                    <Text type="supporting" weight="semibold" color="secondary">Growth Lead</Text>
                    <Markdown headingLevelStart={3}>
                      {'Organic search leads at **31%** of completed signups, up from 24% in July.'}
                    </Markdown>
                  </VStack>
                </ChatMessageBubble>
              </div>
            </ChatMessage>
            <ChatMessage sender="assistant" avatar={AGENT}>
              <div className="group w-full min-w-0 [&_.astryx-chat-message-metadata]:flex-wrap">
                <ChatMessageBubble
                  variant="ghost"
                  metadata={
                    <ChatMessageMetadata status="error" timestamp="09:43" footer={<Actions kind="failed" />} />
                  }
                >
                  <Text type="supporting">
                    Add an AI key in Settings so I can answer. One key is enough.
                  </Text>
                </ChatMessageBubble>
              </div>
            </ChatMessage>
          </ChatMessageList>

          <div className="mt-4 border-t border-[var(--color-border)] pt-4">
            <Text type="supporting" color="secondary">Variant — editing a user message forks the thread</Text>
            <VStack gap={2} align="stretch" className="mt-2">
              <label htmlFor="proto-edit" className="text-xs text-[var(--color-text-secondary)]">
                Edit your message
              </label>
              <textarea
                id="proto-edit"
                defaultValue="Which signup source converts best this month?"
                rows={2}
                className="w-full rounded-md border border-[var(--color-border)] bg-[var(--color-background-surface)] p-2 text-sm text-[var(--color-text-primary)] focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring)]"
              />
              <HStack gap={2} align="center">
                <Button size="sm" variant="primary" label="Save" />
                <Button size="sm" variant="ghost" label="Cancel" />
                <Text type="supporting" color="secondary">
                  Saving will replace everything after this message.
                </Text>
              </HStack>
            </VStack>
          </div>
        </Section>

        {/* ---------------------------------------------------------------- */}
        <Section n={6} title="Composer" note="Live while streaming. Stop is primary; Steer sits beside it.">
          <VStack gap={3} align="stretch">
            <HStack gap={2} align="center">
              <Button
                size="sm"
                variant={streaming ? 'secondary' : 'ghost'}
                label={streaming ? 'Streaming' : 'Idle'}
                onClick={() => setStreaming((s) => !s)}
              />
              <Text type="supporting" color="secondary">toggle the composer state</Text>
            </HStack>

            <ChatComposer
              value={draft}
              onChange={setDraft}
              onSubmit={() => setDraft('')}
              onStop={() => setStreaming(false)}
              isStopShown={streaming}
              placeholder={streaming ? 'Redirect the agent…' : 'Ask what changed…'}
              headerContext={<ContextMeter percent={pct} />}
              footerActions={
                <>
                  <Button isIconOnly variant="ghost" size="sm" label="Attach files" icon={<Paperclip size={16} />} />
                  {streaming ? (
                    <HStack gap={0} align="center">
                      <Button
                        size="sm"
                        variant={mode === 'steer' ? 'secondary' : 'ghost'}
                        label="Now"
                        onClick={() => setMode('steer')}
                      />
                      <Button
                        size="sm"
                        variant={mode === 'followup' ? 'secondary' : 'ghost'}
                        label="After this answer"
                        onClick={() => setMode('followup')}
                      />
                    </HStack>
                  ) : null}
                </>
              }
              sendActions={
                streaming ? (
                  <Button size="sm" variant="secondary" label="Steer" icon={<CornerDownLeft size={14} />} />
                ) : null
              }
            />

            <Text type="supporting" color="secondary">
              isDisabled is never set while streaming — it would kill the Stop button&rsquo;s clicks too.
            </Text>
          </VStack>
        </Section>

        {/* ---------------------------------------------------------------- */}
        <Section n={7} title="Errors" note="An error is red and blames the system. A stop is neutral and blames nobody.">
          <VStack gap={3} align="stretch">
            <ChatComposer
              value=""
              onChange={() => {}}
              onSubmit={() => {}}
              placeholder="Ask what changed…"
              status={{ type: 'error', message: 'Growth Lead is paused. Open Team → Set up and turn the agent on.' }}
            />
            <ChatComposer
              value=""
              onChange={() => {}}
              onSubmit={() => {}}
              placeholder="Ask what changed…"
              status={{ type: 'warning', message: 'Skipped 2 files — text files only.' }}
            />
          </VStack>
        </Section>
      </VStack>
    </main>
  );
}
