'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import {
  Bot,
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
  Waypoints,
  Zap,
} from 'lucide-react';
import type { ComponentType, ReactNode, SVGProps } from 'react';
import { AppShell as AstryxAppShell } from '@astryxdesign/core/AppShell';
import { SideNav, SideNavHeading, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Button } from '@astryxdesign/core/Button';
import { IconButton } from '@astryxdesign/core/IconButton';
import { matchActiveHref, navGroups, navItemsFor, signedInLandingTarget } from '@/lib/ia';
import { useAuth, useUser } from '@/modules/app/hooks';
import { useAuthStore } from '@/lib/app-state';

export type AppSection = 'agents' | 'chat' | 'traffic' | 'product' | 'monitor' | 'dashboards' | 'settings' | 'prototypes' | 'operations';

const NAV_ICONS: Record<string, ComponentType<SVGProps<SVGSVGElement>>> = {
  '/chat': MessageSquare,
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
  // Self-host never renders Plans: a `docker compose up` operator has no plan
  // to be on and nothing to buy, so the item is dropped rather than disabled.
  const hosted = useAuthStore((s) => s.auth?.hosted ?? false);
  const items = navItemsFor({ hosted });
  const current = matchActiveHref(pathname, items);
  const groups = navGroups(items);

  const sideNav = (
    <SideNav
      header={
        <SideNavHeading
          heading="AgentRay"
          subheading="Growth · data · agents"
          headingHref={signedInLandingTarget()}
          icon={<NavIcon icon={<Waypoints size={16} />} />}
        />
      }
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
          <div id="main-content" className="h-full">{children}</div>
        ) : (
          <div id="main-content" className="max-w-[1320px] mx-auto">{children}</div>
        )}
      </AstryxAppShell>
    </>
  );
}
