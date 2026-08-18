'use client';

// PROTOTYPE — bs-8dg2uvct. Throwaway. implement rebuilds from ticket design.md.
// Not linked from production navigation.

import { useState, type ComponentType, type SVGProps } from 'react';
import {
  Beaker,
  Bot,
  Clock,
  CreditCard,
  Globe,
  LayoutDashboard,
  List,
  MessageSquare,
  Package,
  Settings,
  Sparkles,
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

type Screen =
  | 'chat-empty'
  | 'chat-live'
  | 'operations'
  | 'operations-empty'
  | 'operations-error'
  | 'product'
  | 'prototypes'
  | 'prototype-detail';

type NavId = 'Chat' | 'Operations' | 'Agents' | 'Dashboards' | 'Traffic' | 'Product' | 'People' | 'Events' | 'Settings' | 'Plans';

const GROUPS: Array<{
  id: string;
  items: Array<{ nav: NavId; label: string; icon: ComponentType<SVGProps<SVGSVGElement>>; live?: boolean; hostedOnly?: boolean }>;
}> = [
  { id: 'Runtime', items: [{ nav: 'Chat', label: 'Chat', icon: MessageSquare, live: true }] },
  { id: 'Channels', items: [{ nav: 'Operations', label: 'Operations', icon: Zap }] },
  { id: 'Workloads', items: [{ nav: 'Agents', label: 'Agents', icon: Bot }] },
  {
    id: 'Data',
    items: [
      { nav: 'Dashboards', label: 'Dashboards', icon: LayoutDashboard },
      { nav: 'Traffic', label: 'Traffic', icon: Globe },
      { nav: 'Product', label: 'Product', icon: Package },
      { nav: 'People', label: 'People', icon: Users },
      { nav: 'Events', label: 'Events', icon: List },
    ],
  },
  {
    id: 'Workspace',
    items: [
      { nav: 'Settings', label: 'Settings', icon: Settings },
      { nav: 'Plans', label: 'Plans', icon: CreditCard, hostedOnly: true },
    ],
  },
];

const COMING = [
  { name: 'Slack', detail: 'Talk to the same teammate from Slack.' },
  { name: 'Discord', detail: 'Same runtime, from a Discord channel.' },
  { name: 'Telegram', detail: 'Ask on the go. The thread still lands here.' },
];

function selectedNav(screen: Screen): NavId {
  if (screen.startsWith('chat')) return 'Chat';
  if (screen.startsWith('operations')) return 'Operations';
  if (screen === 'product' || screen.startsWith('prototype')) return 'Product';
  return 'Chat';
}

export default function LayerShellPrototype() {
  const [screen, setScreen] = useState<Screen>('chat-live');
  const [hosted, setHosted] = useState(true);
  const [navPick, setNavPick] = useState<NavId | null>(null);
  const selected = navPick ?? selectedNav(screen);

  const go = (nav: NavId) => {
    setNavPick(nav);
    if (nav === 'Chat') setScreen('chat-live');
    else if (nav === 'Operations') setScreen('operations');
    else if (nav === 'Product') setScreen('product');
  };

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
        <VStack gap={2}>
          <HStack gap={2} align="center">
            <Button variant={hosted ? 'primary' : 'outline'} size="sm" onClick={() => setHosted(true)}>Hosted</Button>
            <Button variant={!hosted ? 'primary' : 'outline'} size="sm" onClick={() => setHosted(false)}>Self-host</Button>
          </HStack>
          <div className="flex items-center gap-[9px] p-2 rounded-md bg-[var(--color-background-muted)]">
            <Avatar name="Reviewer" size={24} />
            <div className="min-w-0 flex-1">
              <div className="text-[12.5px] font-medium truncate">Reviewer</div>
              <div className="text-[var(--color-text-secondary)] text-[11px] truncate">prototype</div>
            </div>
          </div>
        </VStack>
      }
    >
      {GROUPS.map((group) => (
        <SideNavSection key={group.id} title={group.id}>
          {group.items.filter((item) => !item.hostedOnly || hosted).map((item) => {
            const Icon = item.icon;
            return (
              <SideNavItem
                key={item.label}
                href="#proto"
                label={item.label}
                icon={Icon}
                isSelected={item.nav === selected}
                endContent={item.live ? <span className="relative inline-block size-2 flex-none rounded-full bg-agent after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-['']" /> : undefined}
                onClick={(e: { preventDefault: () => void }) => {
                  e.preventDefault();
                  go(item.nav);
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
            detail="Switch frames below. Hosted/self-host toggles Plans. implement rebuilds from design.md."
          />
          <Segment
            value={screen}
            onChange={(v) => {
              const next = v as Screen;
              setScreen(next);
              setNavPick(null);
            }}
            options={[
              { value: 'chat-empty', label: 'Chat empty' },
              { value: 'chat-live', label: 'Chat live' },
              { value: 'operations', label: 'Operations' },
              { value: 'operations-empty', label: 'Ops empty' },
              { value: 'operations-error', label: 'Ops error' },
              { value: 'product', label: 'Product' },
              { value: 'prototypes', label: 'Prototypes list' },
              { value: 'prototype-detail', label: 'Prototype detail' },
            ]}
          />
          {screen === 'chat-empty' ? <ChatFrame live={false} /> : null}
          {screen === 'chat-live' ? <ChatFrame live /> : null}
          {screen.startsWith('operations') ? <OperationsFrame mode={screen === 'operations' ? 'ok' : screen === 'operations-empty' ? 'empty' : 'error'} /> : null}
          {screen === 'product' ? <ProductFrame onOpenPrototypes={() => setScreen('prototypes')} /> : null}
          {screen === 'prototypes' ? <PrototypesFrame onOpen={(id) => { void id; setScreen('prototype-detail'); }} /> : null}
          {screen === 'prototype-detail' ? <PrototypeDetailFrame /> : null}
        </VStack>
      </AstryxAppShell>
    </>
  );
}

function ChatHeader({ live }: { live: boolean }) {
  return (
    <HStack justify="between" align="center">
      <HStack align="center" gap={2}>
        <Text weight="semibold">Chat</Text>
        <Badge variant="neutral" label={<>Project <b className="font-medium text-[var(--color-text-primary)]">Demo</b></>} />
        {live ? <Badge variant="neutral" label="Watching the run" /> : null}
      </HStack>
      <HStack align="center" gap={2}>
        <Button variant="ghost" size="sm">Set up</Button>
        <Button variant="outline" size="sm">New chat</Button>
      </HStack>
    </HStack>
  );
}

function ChatFrame({ live }: { live: boolean }) {
  return (
    <VStack gap={3} className="rounded-xl border border-[var(--color-border)] bg-[var(--color-background-card)] p-4">
      <ChatHeader live={live} />
      {live ? (
        <VStack gap={3} className="py-2">
          <Text>What is the single weakest step in my activation funnel?</Text>
          <VStack gap={1}>
            <Text type="supporting">Growth Lead · ran explore_events, run_insight</Text>
            <Text>
              pageview → signup is the weakest step (8%). 80 of 1,000 people make it. This week: one test on that step — don’t add a dashboard.
            </Text>
          </VStack>
        </VStack>
      ) : (
        <VStack gap={2} className="py-10 items-start">
          <Heading level={2}>Ask Growth Lead</Heading>
          <Text type="supporting">Ask here. The thread is where the teammate works.</Text>
          <HStack gap={2} className="flex-wrap pt-2">
            <Button variant="outline" size="sm">What is the weakest step in my funnel?</Button>
            <Button variant="outline" size="sm">Design the cheapest test that could prove this wrong.</Button>
          </HStack>
        </VStack>
      )}
      <div className="rounded-lg border border-[var(--color-border)] px-3 py-2 text-[13px] text-[var(--color-text-secondary)]">
        {live ? 'Redirect the agent…' : 'Ask anything…'}
      </div>
    </VStack>
  );
}

function OperationsFrame({ mode }: { mode: 'ok' | 'empty' | 'error' }) {
  return (
    <VStack gap={0}>
      <Intro
        title="Operations"
        sub="Chat is the front door. These channels start a run without a conversation — a schedule or a webhook. Slack, Discord, and Telegram are next."
        action={<Button variant="primary" icon={<Zap size={15} aria-hidden />}>Give a teammate a schedule</Button>}
      />
      <StatsStrip
        stats={[
          { label: 'Armed', value: mode === 'ok' ? '3' : '0', tone: mode === 'ok' ? 'success' : undefined },
          { label: 'Working now', value: mode === 'ok' ? '1' : '0', tone: mode === 'ok' ? 'agent' : undefined },
          { label: 'Needs attention', value: mode === 'ok' ? '1' : '0', tone: mode === 'ok' ? 'danger' : undefined },
          { label: 'Paused', value: mode === 'ok' ? '1' : '0' },
          { label: 'Runs (24h)', value: mode === 'ok' ? '12' : '0' },
          { label: 'Spend (24h)', value: mode === 'ok' ? '$1.40' : '$0.00' },
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
            detail={mode === 'error' ? 'This list is empty because the request failed — not because you have no channels.' : 'Give a teammate a schedule, or wait for Slack.'}
            action={mode === 'empty' ? <Button variant="primary" size="sm">Give a teammate a schedule</Button> : undefined}
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
        action={<Button variant="agent" icon={<Sparkles size={15} aria-hidden />}>Ask Growth Lead</Button>}
      />
      <HStack gap={2} align="center" className="mb-4 flex-wrap">
        <Text type="supporting">Also</Text>
        <Button variant="outline" size="sm" icon={<Beaker size={13} aria-hidden />} onClick={onOpenPrototypes}>
          Prototypes
        </Button>
      </HStack>
      <Callout
        tone="agentic"
        icon={<Beaker size={18} aria-hidden />}
        label="No events yet"
        title="No product yet? Prove the idea first"
        detail="Market a landing page, paste the snippet, collect the waitlist — then this page has something to chart."
        action={<Button variant="outline" size="sm" onClick={onOpenPrototypes}>Prototypes</Button>}
      />
      <Card padding={4} className="mb-4">
        <Text type="supporting" className="mb-3 block font-medium uppercase tracking-[0.08em]">Ask about your product</Text>
        <Grid columns={{ minWidth: 280, max: 2 }} gap={3}>
          {['How is activity trending?', 'Where do new users drop off?', 'How well do users retain?', 'What are the top events?'].map((label) => (
            <Card key={label} padding={3}>
              <Text weight="semibold">{label}</Text>
            </Card>
          ))}
        </Grid>
      </Card>
      <EmptyState
        icon={<Sparkles size={22} aria-hidden />}
        title="Pick a question to begin"
        detail="Each question runs against this project's events and returns a chart plus the underlying numbers — no SQL required."
      />
    </VStack>
  );
}

function PrototypesFrame({ onOpen }: { onOpen: (id: string) => void }) {
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
          { label: 'On the waitlist', value: '27' },
        ]}
      />
      <Panel title="Running">
        <button type="button" className="text-left w-full" onClick={() => onOpen('pr-2')}>
          <VStack gap={1}>
            <Text>Solo translators will give an email for a weekly glossary-drift report.</Text>
            <Text type="supporting">27 of 40 · 6 days left · waitlist.joined · Marketing Lead</Text>
          </VStack>
        </button>
      </Panel>
    </VStack>
  );
}

function PrototypeDetailFrame() {
  return (
    <VStack gap={0}>
      <Intro
        title="Prototype"
        sub="Committed 6 days ago · 14-day window"
        action={<Button variant="ghost" size="sm">Prototypes</Button>}
      />
      <Panel title="The bet">
        <VStack gap={1}>
          <Text weight="medium">Solo translators will give an email for a weekly glossary-drift report.</Text>
          <Text type="supporting">waitlist.joined · 40 within 14 days</Text>
        </VStack>
      </Panel>
      <div className="h-4" />
      <StatsStrip
        stats={[
          { label: 'waitlist.joined', value: '27 / 40' },
          { label: 'Days left', value: '6' },
          { label: 'On the waitlist', value: '27' },
        ]}
      />
      <Panel title="Put it on the page">
        <VStack gap={2}>
          <HStack gap={2}>
            <Button variant="outline" size="sm">1 · Track the page</Button>
            <Button variant="ghost" size="sm">2 · Collect emails</Button>
          </HStack>
          <Text type="supporting">
            Paste this before &lt;/body&gt; on your landing page. Fed from project.api_key + apiBase() — same as /start. Empty key keeps the YOUR_PROJECT_API_KEY placeholder.
          </Text>
          <div className="rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-2 font-mono text-[12px] text-[var(--color-text-secondary)]">
            {'<script>… AgentRay snippet using project.api_key …</script>'}
          </div>
        </VStack>
      </Panel>
    </VStack>
  );
}
