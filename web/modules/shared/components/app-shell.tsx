'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import {
  Activity,
  Bell,
  Bot,
  Database,
  Globe,
  Languages,
  Layers,
  LayoutDashboard,
  List,
  LogOut,
  MessageSquare,
  Package,
  Settings,
  Store,
  Users,
  UsersRound,
  Waypoints,
} from 'lucide-react';
import type { ComponentType, ReactNode, SVGProps } from 'react';
import { AppShell as AstryxAppShell } from '@astryxdesign/core/AppShell';
import { SideNav, SideNavHeading, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Button } from '@astryxdesign/core/Button';
import { IconButton } from '@astryxdesign/core/IconButton';
import { useAuth, useUser } from '@/modules/app/hooks';
import { useAuthStore } from '@/lib/app-state';

export type AppSection = 'agents' | 'chat' | 'traffic' | 'product' | 'monitor' | 'dashboards' | 'settings';

type NavItem = {
  href: string;
  section: AppSection;
  // Matches Astryx's IconType so the component can be handed straight to SideNavItem.
  icon: ComponentType<SVGProps<SVGSVGElement>>;
  label: string;
  live?: boolean;
};

const navGroups: Array<{ label: string; items: NavItem[] }> = [
  {
    label: 'Main',
    items: [
      { href: '/dashboard', section: 'dashboards', icon: LayoutDashboard, label: 'Dashboards' },
      { href: '/chat', section: 'chat', icon: MessageSquare, label: 'Chat', live: true },
      { href: '/agents', section: 'agents', icon: Bot, label: 'Agents' },
      { href: '/teams', section: 'agents', icon: UsersRound, label: 'Teams' },
      { href: '/marketplace', section: 'agents', icon: Store, label: 'Marketplace' },
      { href: '/web-analytics', section: 'traffic', icon: Globe, label: 'Traffic' },
      { href: '/product', section: 'product', icon: Package, label: 'Product' },
      { href: '/settings', section: 'settings', icon: Settings, label: 'Settings' },
    ],
  },
  {
    label: 'Explore',
    items: [
      { href: '/agents/monitor', section: 'monitor', icon: Activity, label: 'Agent monitor' },
      { href: '/events', section: 'traffic', icon: List, label: 'Events' },
      { href: '/persons', section: 'traffic', icon: Users, label: 'People' },
      { href: '/cohorts', section: 'traffic', icon: Layers, label: 'Cohorts' },
      { href: '/alerts', section: 'monitor', icon: Bell, label: 'Alerts' },
      { href: '/sql', section: 'dashboards', icon: Database, label: 'SQL' },
    ],
  },
];

// The active nav item is the one whose href is the longest prefix of the current
// path. Matching on the shared `section` lit several links at once (Agents +
// Marketplace, Dashboards + SQL); longest-prefix keeps exactly one lit and still
// highlights the parent on nested routes (/agents/123 → Agents, but
// /agents/monitor → Agent monitor since its href is more specific).
function activeHref(pathname: string): string {
  const all = navGroups.flatMap((g) => g.items.map((i) => i.href));
  const matches = all.filter((href) => pathname === href || pathname.startsWith(href + '/'));
  return matches.sort((a, b) => b.length - a.length)[0] ?? '';
}

// Small pulsing "live" indicator shown on the Chat item. Uses the --agent token
// (via the bg-agent/text-agent utilities) and the shared `pulse` keyframes; no
// hardcoded colors.
function LiveDot() {
  return (
    <span className="relative inline-block size-2 flex-none rounded-full bg-agent text-agent after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-['']" />
  );
}

// Account + language + logout footer, pinned to the bottom of the SideNav.
function SidebarFooter() {
  const user = useUser();
  const { logout } = useAuth();
  const workspaces = useAuthStore((s) => s.workspaces);
  const selectedWorkspaceID = useAuthStore((s) => s.selectedWorkspaceID);
  const workspace = workspaces.find((w) => w.id === selectedWorkspaceID) ?? workspaces[0];
  const accountName = user?.name || user?.email || 'Account';

  return (
    <div className="flex flex-col gap-2 px-1 pb-1">
      <div className="flex items-center gap-1.5 px-1 py-0.5 text-[var(--color-text-secondary)] text-[12.5px]">
        <Languages size={15} />
        <span>Language</span>
        <span className="flex-1" />
        <Button label="EN" size="sm" variant="secondary" />
        <Button label="VI" size="sm" variant="ghost" />
      </div>
      <div className="flex items-center gap-[9px] p-2 rounded-md bg-[var(--color-background-muted)]">
        <Avatar name={accountName} size={24} />
        <div className="min-w-0 flex-1">
          <div className="text-[12.5px] font-medium truncate">{accountName}</div>
          <div className="text-[var(--color-text-secondary)] text-[11px] truncate">{workspace?.name || 'workspace'}</div>
        </div>
        <IconButton
          label="Log out"
          icon={<LogOut size={15} />}
          variant="ghost"
          size="sm"
          tooltip="Log out"
          onClick={() => void logout()}
        />
      </div>
    </div>
  );
}

export function AppShell({ children, bleed = false }: { active?: AppSection; children: ReactNode; bleed?: boolean }) {
  const pathname = usePathname() ?? '';
  const current = activeHref(pathname);

  const sideNav = (
    <SideNav
      header={
        <SideNavHeading
          heading="AgentRay"
          subheading="Growth · data · agents"
          headingHref="/dashboard"
          icon={<NavIcon icon={<Waypoints size={16} />} />}
        />
      }
      footer={<SidebarFooter />}
    >
      {navGroups.map((group) => (
        <SideNavSection key={group.label} title={group.label}>
          {group.items.map((item) => (
            <SideNavItem
              key={item.href}
              as={Link}
              href={item.href}
              label={item.label}
              icon={item.icon}
              isSelected={item.href === current}
              endContent={item.live ? <LiveDot /> : undefined}
            />
          ))}
        </SideNavSection>
      ))}
    </SideNav>
  );

  return (
    <AstryxAppShell height="fill" contentPadding={bleed ? 0 : 6} sideNav={sideNav}>
      {bleed ? children : <div className="max-w-[1320px] mx-auto">{children}</div>}
    </AstryxAppShell>
  );
}
