'use client';

// PROTOTYPE — bs-e9dnyopv. Throwaway. implement rebuilds from ticket design.md.
// Not linked from production navigation.
// Open: http://localhost:3200/prototype/project-switcher

import { useCallback, useState, type ComponentType, type SVGProps } from 'react';
import {
  Bot,
  Check,
  CreditCard,
  FolderPlus,
  Globe,
  LayoutDashboard,
  List,
  MessageSquare,
  Package,
  Plus,
  Settings,
  TriangleAlert,
  Users,
  Waypoints,
  Zap,
} from 'lucide-react';
import { toast } from 'sonner';
import { AppShell as AstryxAppShell } from '@astryxdesign/core/AppShell';
import { Divider } from '@astryxdesign/core/Divider';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Badge } from '@astryxdesign/core/Badge';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { NavHeadingMenu, NavHeadingMenuItem } from '@astryxdesign/core/NavMenu';
import { SideNav, SideNavHeading, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { Text } from '@astryxdesign/core/Text';
import { HStack } from '@astryxdesign/core/HStack';
import { VStack } from '@astryxdesign/core/VStack';
import { PromptDialog } from '@/modules/shared/components/modal';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import {
  Button,
  Callout,
  EmptyState,
  Intro,
  Segment,
} from '@/modules/shared/components/signal-primitives';

type Screen = 'chat' | 'settings' | 'sql' | 'agent-detail' | 'agents';
type Fixture = 'several' | 'one' | 'none';

type Project = { id: string; name: string; created: string };

const SEED: Project[] = [
  { id: 'prod', name: 'Production', created: '3 months ago' },
  { id: 'staging', name: 'Staging', created: '6 weeks ago' },
  { id: 'demo', name: 'Demo', created: '2 days ago' },
];

const NAV: Array<{ id: string; items: Array<{ label: string; icon: ComponentType<SVGProps<SVGSVGElement>>; live?: boolean; screen?: Screen }> }> = [
  { id: 'Runtime', items: [{ label: 'Chat', icon: MessageSquare, live: true, screen: 'chat' }] },
  { id: 'Channels', items: [{ label: 'Operations', icon: Zap }] },
  { id: 'Workloads', items: [{ label: 'Agents', icon: Bot, screen: 'agent-detail' }] },
  {
    id: 'Data',
    items: [
      { label: 'Dashboards', icon: LayoutDashboard },
      { label: 'Traffic', icon: Globe },
      { label: 'Product', icon: Package },
      { label: 'People', icon: Users },
      { label: 'Events', icon: List },
    ],
  },
  {
    id: 'Workspace',
    items: [
      { label: 'Settings', icon: Settings, screen: 'settings' },
      { label: 'Plans', icon: CreditCard },
    ],
  },
];

function IconSlot({ filled }: { filled: boolean }) {
  return filled ? <Check size={16} aria-hidden /> : <span className="inline-block size-4" aria-hidden />;
}

function ProjectMenu({
  projects,
  activeID,
  onSwitch,
  onCreate,
  onManage,
}: {
  projects: Project[];
  activeID: string;
  onSwitch: (id: string) => void;
  onCreate: () => void;
  onManage: () => void;
}) {
  return (
    <NavHeadingMenu>
      {projects.length === 0 ? (
        <NavHeadingMenuItem label="No projects yet" isDisabled />
      ) : (
        projects.map((p) => (
          <NavHeadingMenuItem
            key={p.id}
            label={p.name}
            description={p.id === activeID ? 'Current' : undefined}
            icon={<IconSlot filled={p.id === activeID} />}
            onClick={() => onSwitch(p.id)}
          />
        ))
      )}
      <Divider variant="subtle" />
      <NavHeadingMenuItem label="New project" icon={FolderPlus} onClick={onCreate} />
      <NavHeadingMenuItem label="Manage projects" icon={Settings} onClick={onManage} />
    </NavHeadingMenu>
  );
}

export default function ProjectSwitcherPrototype() {
  const [screen, setScreen] = useState<Screen>('chat');
  const [fixture, setFixture] = useState<Fixture>('several');
  const [projects, setProjects] = useState<Project[]>(SEED);
  const [activeID, setActiveID] = useState('prod');
  const [creating, setCreating] = useState(false);
  const [sqlRows, setSqlRows] = useState<string[]>(['signup 12', 'pageview 48']);

  const list = fixture === 'none' ? [] : fixture === 'one' ? projects.slice(0, 1) : projects;
  const active = list.find((p) => p.id === activeID) ?? list[0] ?? null;

  function applyFixture(next: Fixture) {
    setFixture(next);
    if (next === 'none') setActiveID('');
    else setActiveID('prod');
  }

  const switchTo = useCallback((id: string) => {
    if (id === activeID) return;
    const next = (fixture === 'none' ? [] : fixture === 'one' ? projects.slice(0, 1) : projects).find((p) => p.id === id);
    if (!next) return;
    setActiveID(next.id);
    setSqlRows([]);
    if (screen === 'agent-detail') setScreen('agents');
    toast.success(`Switched to ${next.name}.`);
  }, [activeID, fixture, projects, screen]);

  function createProject(name: string) {
    const id = name.toLowerCase().replace(/\s+/g, '-');
    const next = { id, name, created: 'just now' };
    setProjects((rows) => [...rows, next]);
    setFixture('several');
    setActiveID(id);
    setSqlRows([]);
    if (screen === 'agent-detail') setScreen('agents');
    toast.success('Project created and connected.');
  }

  const sideNav = (
    <SideNav
      header={
        <SideNavHeading
          heading={active?.name ?? 'No project'}
          subheading="Acme workspace"
          icon={<NavIcon icon={<Waypoints size={16} />} />}
          menu={
            <ProjectMenu
              projects={list}
              activeID={active?.id ?? ''}
              onSwitch={switchTo}
              onCreate={() => setCreating(true)}
              onManage={() => setScreen('settings')}
            />
          }
        />
      }
      footer={
        <div className="flex items-center gap-[9px] p-2 rounded-md bg-[var(--color-background-muted)]">
          <Avatar name="Ada Lovelace" size={24} />
          <div className="min-w-0 flex-1">
            <div className="text-[12.5px] font-medium truncate">Ada Lovelace</div>
            <div className="text-[var(--color-text-secondary)] text-[11px] truncate">Acme workspace</div>
          </div>
        </div>
      }
    >
      {NAV.map((group) => (
        <SideNavSection key={group.id} title={group.id}>
          {group.items.map((item) => {
            const Icon = item.icon;
            const selected =
              (screen === 'chat' && item.label === 'Chat') ||
              (screen === 'settings' && item.label === 'Settings') ||
              ((screen === 'agent-detail' || screen === 'agents') && item.label === 'Agents');
            return (
              <SideNavItem
                key={item.label}
                href="#proto"
                label={item.label}
                icon={Icon}
                isSelected={selected}
                endContent={item.live ? <span className="relative inline-block size-2 flex-none rounded-full bg-agent after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-['']" /> : undefined}
                onClick={(e: { preventDefault: () => void }) => {
                  e.preventDefault();
                  if (item.screen) setScreen(item.screen);
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
      {creating ? (
        <PromptDialog
          title="New project"
          label="Project name"
          placeholder="e.g. Production"
          submitLabel="Create project"
          onSubmit={(n) => createProject(n)}
          onClose={() => setCreating(false)}
        />
      ) : null}
      <AstryxAppShell height="fill" contentPadding={6} sideNav={sideNav}>
        <VStack id="main-content" gap={4} className="max-w-[1320px] mx-auto">
          <Callout
            tone="agentic"
            icon={<TriangleAlert size={18} aria-hidden />}
            label="Prototype"
            title="Project switcher — not wired to production"
            detail="Click the sidebar heading to switch or create. Chat Project badge stays display-only. Switching on Agent setup lands on the Agents list. implement rebuilds from design.md."
          />
          <HStack gap={3} align="center" className="flex-wrap">
            <Segment
              value={screen}
              onChange={(v) => setScreen(v as Screen)}
              options={[
                { value: 'chat', label: 'Chat' },
                { value: 'settings', label: 'Settings → Projects' },
                { value: 'sql', label: 'SQL (stale rows)' },
                { value: 'agent-detail', label: 'Agent setup' },
              ]}
            />
            <Segment
              value={fixture}
              onChange={(v) => applyFixture(v as Fixture)}
              options={[
                { value: 'several', label: 'Several' },
                { value: 'one', label: 'One' },
                { value: 'none', label: 'None' },
              ]}
            />
          </HStack>
          {screen === 'chat' ? <ChatFrame projectName={active?.name ?? '—'} /> : null}
          {screen === 'settings' ? (
            <SettingsFrame
              projects={list}
              activeID={active?.id ?? ''}
              onSwitch={switchTo}
              onCreate={() => setCreating(true)}
            />
          ) : null}
          {screen === 'sql' ? <SqlFrame rows={sqlRows} projectName={active?.name ?? '—'} /> : null}
          {screen === 'agent-detail' ? <AgentDetailFrame projectName={active?.name ?? '—'} /> : null}
          {screen === 'agents' ? <AgentsListFrame projectName={active?.name ?? '—'} /> : null}
        </VStack>
      </AstryxAppShell>
    </>
  );
}

function ChatFrame({ projectName }: { projectName: string }) {
  return (
    <>
      <HStack justify="between" align="center" className="h-12 flex-none rounded-md border border-[var(--color-border)] bg-[var(--color-background-card)] px-4">
        <HStack align="center" gap={2}>
          <Text weight="semibold">Chat</Text>
          <Badge variant="neutral" label={<>Project <b className="font-medium text-[var(--color-text-primary)]">{projectName}</b></>} />
        </HStack>
        <HStack align="center" gap={2}>
          <Button variant="ghost" size="sm">Set up</Button>
          <Button variant="outline" size="sm" icon={<Plus size={14} />}>New chat</Button>
        </HStack>
      </HStack>
      <EmptyState
        title="Ask here. The thread is where the teammate works."
        detail="The Project badge is display-only. Switch projects from the sidebar heading."
      />
    </>
  );
}

function SqlFrame({ rows, projectName }: { rows: string[]; projectName: string }) {
  return (
    <>
      <Intro title="SQL" sub={`Results for ${projectName}. Switching clears this table.`} />
      {rows.length === 0 ? (
        <EmptyState title="No results" detail="The previous project's rows were cleared on switch." />
      ) : (
        <VStack gap={2}>
          {rows.map((row) => (
            <Text key={row}>{row}</Text>
          ))}
        </VStack>
      )}
    </>
  );
}

function AgentDetailFrame({ projectName }: { projectName: string }) {
  return (
    <>
      <Intro title="Growth Lead · setup" sub={`This agent belongs to ${projectName}. Switching leaves this page.`} />
      <EmptyState
        title="On /agents/growth-lead/setup"
        detail="Switching a project from a detail route lands on /agents — the group's list. Use the heading switcher to see it."
      />
    </>
  );
}

function AgentsListFrame({ projectName }: { projectName: string }) {
  return (
    <>
      <Intro title="Agents" sub={`Teammates in ${projectName}.`} />
      <EmptyState title="Landed on /agents" detail="The previous agent's setup page is gone. This is the list root for the new project." />
    </>
  );
}

function SettingsFrame({
  projects,
  activeID,
  onSwitch,
  onCreate,
}: {
  projects: Project[];
  activeID: string;
  onSwitch: (id: string) => void;
  onCreate: () => void;
}) {
  const columns: DataColumn<Project>[] = [
    {
      key: 'name',
      header: 'Project',
      renderCell: (p) => {
        const active = p.id === activeID;
        return <span>{active ? <b>{p.name}</b> : p.name}{active ? <span className="text-[var(--color-text-secondary)]"> · active</span> : null}</span>;
      },
    },
    {
      key: 'created',
      header: 'Created',
      renderCell: (p) => <span className="text-[var(--color-text-secondary)]">{p.created}</span>,
    },
    {
      key: 'id',
      header: '',
      hideable: false,
      sortable: false,
      align: 'end',
      renderCell: (p) => (
        <span onClick={(e) => e.stopPropagation()}>
          {p.id === activeID
            ? <Button variant="ghost" size="sm">Rename</Button>
            : <Button variant="ghost" size="sm" onClick={() => onSwitch(p.id)}>Switch</Button>}
        </span>
      ),
    },
  ];

  if (projects.length === 0) {
    return (
      <>
        <Intro title="Settings" sub="Workspace, people, AI key, and how events get in." />
        <EmptyState title="No projects" detail="Create a project to start ingesting events." action={<Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={onCreate}>New project</Button>} />
      </>
    );
  }

  return (
    <>
      <Intro title="Settings" sub="Workspace, people, AI key, and how events get in." />
      <DataTable
        title="Projects"
        columns={columns}
        data={projects}
        action={<Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={onCreate}>New project</Button>}
        onRowClick={(p) => onSwitch(p.id)}
      />
    </>
  );
}
