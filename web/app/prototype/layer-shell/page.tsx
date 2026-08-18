'use client';

// PROTOTYPE — bs-8dg2uvct. Throwaway design surface for the layer-aligned
// shell: Runtime / Channels / Workloads / Data / Workspace. Not linked from
// production navigation, not wired to the API. `implement` rebuilds the nav
// in web/lib/ia.ts + app-shell.tsx and the framing on /chat, /operations,
// /prototypes. Spec: ticket design.md

import { useState, type ComponentType, type SVGProps } from 'react';
import {
  Beaker,
  Bot,
  Clock,
  Globe,
  LayoutDashboard,
  List,
  MessageSquare,
  Package,
  Settings,
  TriangleAlert,
  Users,
  Waypoints,
  Webhook,
  Zap,
} from 'lucide-react';
import { AppShell as AstryxAppShell } from '@astryxdesign/core/AppShell';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Badge } from '@astryxdesign/core/Badge';
import { Card } from '@astryxdesign/core/Card';
import { Grid } from '@astryxdesign/core/Grid';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { SideNav, SideNavHeading, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import {
  Button,
  Callout,
  EmptyState,
  Intro,
  Panel,
  Segment,
  StatsStrip,
  StatusPill,
} from '@/modules/shared/components/signal-primitives';

type Screen = 'chat-empty' | 'chat-live' | 'operations' | 'operations-empty' | 'operations-error' | 'product' | 'prototypes';

const GROUPS: Array<{
  id: string;
  items: Array<{ href: Screen; label: string; icon: ComponentType<SVGProps<SVGSVGElement>>; live?: boolean }>;
}> = [
  { id: 'Runtime', items: [{ href: 'chat-live', label: 'Chat', icon: MessageSquare, live: true }] },
  { id: 'Channels', items: [{ href: 'operations', label: 'Operations', icon: Zap }] },
  { id: 'Workloads', items: [{ href: 'product', label: 'Agents', icon: Bot }] },
  {
    id: 'Data',
    items: [
      { href: 'product', label: 'Dashboards', icon: LayoutDashboard },
      { href: 'product', label: 'Traffic', icon: Globe },
      { href: 'product', label: 'Product', icon: Package },
      { href: 'product', label: 'People', icon: Users },
      { href: 'product', label: 'Events', icon: List },
    ],
  },
  { id: 'Workspace', items: [{ href: 'prototypes', label: 'Settings', icon: Settings }] },
];

const COMING = [
  { name: 'Slack', detail: 'Talk to the same teammate from Slack.' },
  { name: 'Discord', detail: 'Same runtime, from a Discord channel.' },
  { name: 'Telegram', detail: 'Ask on the go. The thread still lands here.' },
];

export default function LayerShellPrototype() {
  const [screen, setScreen] = useState<Screen>('chat-live');
  const selected = screen.startsWith('chat')
    ? 'Chat'
    : screen.startsWith('operations')
      ? 'Operations'
      : screen === 'product' || screen === 'prototypes'
        ? 'Product'
        : 'Product';

  const sideNav = (
    <SideNav
      header={
        <SideNavHeading
          heading="AgentRay"
          subheading="Growth · data · agents"
          headingHref="#proto"
          icon={<NavIcon icon={<Waypoints size={16} />} />}
        />
      }
      footer={
        <div className="flex items-center gap-[9px] p-2 rounded-md bg-[var(--color-background-muted)]">
          <Avatar name="Reviewer" size={24} />
          <div className="min-w-0 flex-1">
            <div className="text-[12.5px] font-medium truncate">Reviewer</div>
            <div className="text-[var(--color-text-secondary)] text-[11px] truncate">prototype</div>
          </div>
        </div>
      }
    >
      {GROUPS.map((group) => (
        <SideNavSection key={group.id} title={group.id}>
          {group.items.map((item) => {
            const Icon = item.icon;
            const isSelected = item.label === selected;
            return (
              <SideNavItem
                key={item.label}
                href="#proto"
                label={item.label}
                icon={Icon}
                isSelected={isSelected}
                endContent={item.live ? <span className="relative inline-block size-2 flex-none rounded-full bg-agent after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-['']" /> : undefined}
                onClick={(e: { preventDefault: () => void }) => {
                  e.preventDefault();
                  if (item.label === 'Chat') setScreen('chat-live');
                  else if (item.label === 'Operations') setScreen('operations');
                  else if (item.label === 'Product') setScreen('product');
                }}
              />
            );
          })}
        </SideNavSection>
      ))}
    </SideNav>
  );

  return (
    <>
      <a href="#main-content" className="skip-to-content">Skip to content</a>
      <AstryxAppShell height="fill" contentPadding={6} sideNav={sideNav}>
        <VStack id="main-content" gap={4} className="max-w-[1320px] mx-auto">
          <Callout
            tone="agentic"
            icon={<TriangleAlert size={18} aria-hidden />}
            label="Prototype"
            title="Layer-aligned shell — not wired to production"
            detail="Switch the frames below. Production nav still shows Ask / Work / Team. implement rebuilds from this."
          />
          <Segment
            value={screen}
            onChange={(v) => setScreen(v as Screen)}
            options={[
              { value: 'chat-empty', label: 'Chat empty' },
              { value: 'chat-live', label: 'Chat live' },
              { value: 'operations', label: 'Operations' },
              { value: 'operations-empty', label: 'Ops empty' },
              { value: 'operations-error', label: 'Ops error' },
              { value: 'product', label: 'Product' },
              { value: 'prototypes', label: 'Prototypes (nested)' },
            ]}
          />
          {screen === 'chat-empty' ? <ChatEmpty /> : null}
          {screen === 'chat-live' ? <ChatLive /> : null}
          {screen.startsWith('operations') ? <OperationsFrame mode={screen === 'operations' ? 'ok' : screen === 'operations-empty' ? 'empty' : 'error'} /> : null}
          {screen === 'product' ? <ProductFrame onOpenPrototypes={() => setScreen('prototypes')} /> : null}
          {screen === 'prototypes' ? <PrototypesFrame /> : null}
        </VStack>
      </AstryxAppShell>
    </>
  );
}

function ChatEmpty() {
  return (
    <VStack gap={3} className="rounded-xl border border-[var(--color-border)] bg-[var(--color-background-card)] p-4">
      <HStack justify="between" align="center">
        <HStack align="center" gap={2}>
          <Text weight="semibold">Chat</Text>
        </HStack>
        <Button variant="outline" size="sm">New chat</Button>
      </HStack>
      <VStack gap={2} className="py-10 items-start">
        <Heading level={2}>Ask Growth Lead</Heading>
        <Text type="supporting">Ask here. The thread is where the teammate works.</Text>
        <HStack gap={2} className="flex-wrap pt-2">
          <Button variant="outline" size="sm">What is the weakest step in my funnel?</Button>
          <Button variant="outline" size="sm">Design the cheapest test that could prove this wrong.</Button>
          <Button variant="ghost" size="sm">Set up</Button>
        </HStack>
      </VStack>
      <div className="rounded-lg border border-[var(--color-border)] px-3 py-2 text-[13px] text-[var(--color-text-secondary)]">
        Ask anything…
      </div>
    </VStack>
  );
}

function ChatLive() {
  return (
    <VStack gap={3} className="rounded-xl border border-[var(--color-border)] bg-[var(--color-background-card)] p-4">
      <HStack justify="between" align="center">
        <HStack align="center" gap={2}>
          <Text weight="semibold">Chat</Text>
          <Badge variant="neutral" label="Watching the run" />
        </HStack>
        <Button variant="outline" size="sm">New chat</Button>
      </HStack>
      <VStack gap={3} className="py-2">
        <Text>What is the single weakest step in my activation funnel?</Text>
        <VStack gap={1}>
          <Text type="supporting">Growth Lead · ran explore_events, run_insight</Text>
          <Text>
            pageview → signup is the weakest step (8%). 80 of 1,000 people make it. This week: one test on that step — don’t add a dashboard.
          </Text>
        </VStack>
      </VStack>
      <div className="rounded-lg border border-[var(--color-border)] px-3 py-2 text-[13px] text-[var(--color-text-secondary)]">
        Redirect the agent…
      </div>
    </VStack>
  );
}

function OperationsFrame({ mode }: { mode: 'ok' | 'empty' | 'error' }) {
  return (
    <VStack gap={0}>
      <Intro
        title="Operations"
        sub="Channels that start a run without a conversation — a schedule or a webhook. Slack, Discord, and Telegram are next."
        action={<Button variant="primary" icon={<Zap size={15} aria-hidden />}>Add a schedule</Button>}
      />
      <StatsStrip
        stats={[
          { label: 'Armed', value: mode === 'ok' ? '3' : '0', tone: mode === 'ok' ? 'success' : undefined },
          { label: 'Working now', value: mode === 'ok' ? '1' : '0', tone: mode === 'ok' ? 'agent' : undefined },
          { label: 'Needs attention', value: mode === 'ok' ? '1' : '0', tone: mode === 'ok' ? 'danger' : undefined },
          { label: 'Paused', value: '1' },
        ]}
      />
      {mode === 'error' ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Can’t read"
          title="We couldn’t load your channels"
          detail="The API didn’t answer. Nothing has been paused — armed schedules keep firing while this page is blind."
          action={<Button variant="outline" size="sm">Try again</Button>}
        />
      ) : null}
      <Panel title="Armed channels">
        {mode === 'empty' || mode === 'error' ? (
          <EmptyState
            icon={<Zap size={18} aria-hidden />}
            title={mode === 'error' ? 'Nothing to show while we’re disconnected' : 'Nothing starts a run without you yet'}
            detail={mode === 'error' ? 'This list is empty because the request failed — not because you have no channels.' : 'Add a schedule, or wait for Slack. Chat is still the front door.'}
            action={mode === 'empty' ? <Button variant="primary" size="sm">Add a schedule</Button> : undefined}
          />
        ) : (
          <VStack gap={2}>
            <ChannelRow icon={<Clock size={13} aria-hidden />} title="Morning health sweep" detail="Every day 07:00 · Ops Watch" status="attention" label="Attention" />
            <ChannelRow icon={<Clock size={13} aria-hidden />} title="Weekly marketing plan" detail="Mondays 09:00 · Growth pod" status="working" label="Working" />
            <ChannelRow icon={<Webhook size={13} aria-hidden />} title="Deploy → tracking check" detail="POST /hook/…a41f · Tracking Steward" status="healthy" label="Armed" />
          </VStack>
        )}
      </Panel>
      <div className="h-4" />
      <Panel title="Coming">
        <Grid columns={{ minWidth: 180, max: 3 }} gap={3}>
          {COMING.map((ch) => (
            <Card key={ch.name} padding={3}>
              <VStack gap={1}>
                <HStack align="center" justify="between">
                  <Text weight="semibold">{ch.name}</Text>
                  <Badge variant="neutral" label="Not yet" />
                </HStack>
                <Text type="supporting">{ch.detail}</Text>
              </VStack>
            </Card>
          ))}
        </Grid>
      </Panel>
    </VStack>
  );
}

function ChannelRow({ icon, title, detail, status, label }: { icon: React.ReactNode; title: string; detail: string; status: string; label: string }) {
  return (
    <HStack align="center" justify="between" gap={3} className="py-1">
      <HStack align="center" gap={2} className="min-w-0">
        <span className="text-[var(--color-text-secondary)]">{icon}</span>
        <VStack gap={0} className="min-w-0">
          <Text>{title}</Text>
          <Text type="supporting" maxLines={1}>{detail}</Text>
        </VStack>
      </HStack>
      <StatusPill status={status} label={label} grow={false} />
    </HStack>
  );
}

function ProductFrame({ onOpenPrototypes }: { onOpenPrototypes: () => void }) {
  return (
    <VStack gap={0}>
      <Intro
        title="Product"
        sub="Answer behavior questions without writing SQL first."
        action={<Button variant="agent">Ask Growth Lead</Button>}
      />
      <HStack gap={2} align="center" className="mb-4 flex-wrap">
        <Text type="supporting">Also</Text>
        <Button variant="outline" size="sm" icon={<Beaker size={13} aria-hidden />} onClick={onOpenPrototypes}>
          Prototypes
        </Button>
      </HStack>
      <Text type="supporting">Prototypes are a child of Product — marketing-first tests, not a sidebar item.</Text>
    </VStack>
  );
}

function PrototypesFrame() {
  return (
    <VStack gap={0}>
      <Intro
        title="Prototypes"
        sub="Market the idea before you build it. Paste the snippet on the page, collect the waitlist, and keep the number you agreed to."
        action={<Button variant="agent" icon={<Beaker size={15} aria-hidden />}>Talk to Marketing Lead</Button>}
      />
      <StatsStrip
        stats={[
          { label: 'Waiting on you', value: '1', tone: 'agent' },
          { label: 'Running', value: '1' },
          { label: 'Passed', value: '1', tone: 'success' },
          { label: 'Failed', value: '1' },
        ]}
      />
      <Panel title="Running">
        <VStack gap={1}>
          <Text>Solo translators will give an email for a weekly glossary-drift report.</Text>
          <Text type="supporting">27 of 40 · 6 days left · waitlist.joined · Marketing Lead</Text>
        </VStack>
      </Panel>
      <div className="h-4" />
      <Panel title="Put it on the page">
        <Text type="supporting">
          Paste the tracking snippet on the landing page, then collect emails with the waitlist form. No npm — Framer, Carrd, or HTML.
        </Text>
      </Panel>
    </VStack>
  );
}
