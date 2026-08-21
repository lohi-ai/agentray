'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import {
  Bot,
  ClipboardList,
  CreditCard,
  Globe,
  Languages,
  LayoutDashboard,
  List,
  LogOut,
  MessageSquare,
  Package,
  Settings,
  Users,
  Zap,
} from 'lucide-react';
import { Eye } from 'lucide-react';
import type { ComponentType, ReactNode, SVGProps } from 'react';
import { AppShell as AstryxAppShell } from '@astryxdesign/core/AppShell';
import { SideNav, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Button } from '@astryxdesign/core/Button';
import { IconButton } from '@astryxdesign/core/IconButton';
import { matchActiveHref, navGroups, navItemsFor } from '@/lib/ia';
import { useAuth, useProjectAccess, useUser } from '@/modules/app/hooks';
import { useAuthStore } from '@/lib/app-state';
import { ProjectSwitcher } from '@/modules/shared/components/project-menu';

export type AppSection = 'agents' | 'chat' | 'traffic' | 'product' | 'monitor' | 'dashboards' | 'settings' | 'prototypes' | 'operations';

const NAV_ICONS: Record<string, ComponentType<SVGProps<SVGSVGElement>>> = {
  '/chat': MessageSquare,
  '/start': ClipboardList,
  '/operations': Zap,
  '/agents': Bot,
  '/dashboard': LayoutDashboard,
  '/web-analytics': Globe,
  '/product': Package,
  '/settings': Settings,
  '/persons': Users,
  '/events': List,
  '/pricing': CreditCard,
};

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
      <div className="flex items-center gap-2 p-2 rounded-md bg-[var(--color-background-muted)]">
        <div className="flex-none">
          <Avatar name={accountName} size={24} />
        </div>
        <div className="min-w-0 flex-1 overflow-hidden">
          <div className="truncate text-[12.5px] font-medium">{accountName}</div>
          <div className="truncate text-[11px] text-[var(--color-text-secondary)]">{workspace?.name || 'workspace'}</div>
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

// DemoBar is the label that never goes away.
//
// A toast would be wrong here twice over: it is dismissible, so the answer to
// "whose numbers am I looking at?" would depend on whether the reader happened
// to be watching four seconds ago; and it is transient, so it would be gone by
// the time they reach the dashboard the number is on. This sits above every
// screen for as long as the demo project is the active one, and it says both
// halves — this is somebody else's site, and you are here to read it.
function DemoBar({ inset }: { inset: boolean }) {
  const access = useProjectAccess();
  const projectName = useAuthStore((s) => s.project?.name);
  if (!access.isDemo) return null;
  return (
    <div
      role="status"
      className={`flex flex-none items-center gap-2 bg-[color-mix(in_srgb,var(--agent)_12%,transparent)] px-4 py-1.5 text-[12.5px] text-[var(--color-text-secondary)] ${
        inset
          ? 'mb-4 rounded-[var(--radius-md)] border border-[var(--color-border)]'
          : 'border-b border-[var(--color-border)]'
      }`}
    >
      <Eye size={14} aria-hidden className="flex-none text-agent" />
      <span className="min-w-0">
        <b className="font-medium text-[var(--color-text-primary)]">{projectName || 'This project'}</b>
        {' is a live site someone else runs. '}
        {access.canWrite ? 'You can read and change it.' : 'You’re reading it as a viewer — nothing here can be changed.'}
      </span>
    </div>
  );
}

export function AppShell({ children, bleed = false }: { active?: AppSection; children: ReactNode; bleed?: boolean }) {
  const pathname = usePathname() ?? '';
  // Self-host never renders Plans: a `docker compose up` operator has no plan
  // to be on and nothing to buy, so the item is dropped rather than disabled.
  const hosted = useAuthStore((s) => s.auth?.hosted ?? false);
  const items = navItemsFor({ hosted });
  const current = matchActiveHref(pathname, items);
  const groups = navGroups(items);

  const sideNav = (
    <SideNav
      header={<ProjectSwitcher />}
      footer={<SidebarFooter />}
    >
      {groups.map((group) => (
        <SideNavSection key={group.id} title={group.label}>
          {group.items.map((item) => {
            const Icon = NAV_ICONS[item.href];
            return (
              <SideNavItem
                key={item.href}
                as={Link}
                href={item.href}
                label={item.label}
                icon={Icon}
                isSelected={item.href === current}
                endContent={item.href === '/chat' ? <LiveDot /> : undefined}
              />
            );
          })}
        </SideNavSection>
      ))}
    </SideNav>
  );

  return (
    <>
      {/* First tab stop on every page. Without it a keyboard user walks all
          eleven nav items before reaching the content, on every navigation. */}
      <a href="#main-content" className="skip-to-content">Skip to content</a>
      <AstryxAppShell height="fill" contentPadding={bleed ? 0 : 6} sideNav={sideNav}>
        {bleed ? (
          <div className="flex h-full min-h-0 flex-col">
            <DemoBar inset={false} />
            <div id="main-content" className="min-h-0 flex-1">{children}</div>
          </div>
        ) : (
          <div id="main-content" className="mx-auto w-full max-w-[1320px]">
            <DemoBar inset />
            {children}
          </div>
        )}
      </AstryxAppShell>
    </>
  );
}
