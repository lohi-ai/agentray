'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import {
  Bot,
  Gauge,
  Languages,
  LayoutDashboard,
  List,
  LogOut,
  Menu,
  MessageSquare,
  Settings,
  Users,
  X,
  Zap,
} from 'lucide-react';
import { Eye, FlaskConical } from 'lucide-react';
import { useState, type ComponentType, type ReactNode, type SVGProps } from 'react';
import { SideNav, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { Avatar } from '@astryxdesign/core/Avatar';
import { Button } from '@astryxdesign/core/Button';
import { IconButton } from '@astryxdesign/core/IconButton';
import { CHILD_SURFACES, childSurfacesFor, matchActiveHref, navGroups, navItemsFor } from '@/lib/ia';
import { useAuth, useProjectAccess, useUser } from '@/modules/app/hooks';
import { useAuthStore } from '@/lib/app-state';
import { ProjectSwitcher } from '@/modules/shared/components/project-menu';
import { AsideSection, PageShell } from '@/modules/shared/components/page-shell';
import { RelatedSurfacesNav } from '@/modules/shared/components/related-surfaces';

export type AppSection = 'agents' | 'chat' | 'traffic' | 'product' | 'monitor' | 'dashboards' | 'settings' | 'prototypes' | 'operations';

const NAV_ICONS: Record<string, ComponentType<SVGProps<SVGSVGElement>>> = {
  '/overview': Gauge,
  '/agents': Bot,
  '/dashboard': LayoutDashboard,
  '/settings': Settings,
  '/persons': Users,
  '/events': List,
  '/prototypes': FlaskConical,
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
      <div className="flex items-center gap-2 px-1 py-0.5 text-[var(--color-text-secondary)] text-xs">
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
          <div className="truncate text-xs font-medium">{accountName}</div>
          <div className="truncate text-2xs text-[var(--color-text-secondary)]">{workspace?.name || 'workspace'}</div>
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
function DemoBar() {
  const access = useProjectAccess();
  const projectName = useAuthStore((s) => s.project?.name);
  if (!access.isDemo) return null;
  return (
    <div
      role="status"
      className="flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--agent)_12%,transparent)] px-3 py-2 text-sm text-[var(--color-text-secondary)]"
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

export type AppShellProps = {
  active?: AppSection;
  /** Page title — row one of the page grid. */
  title?: ReactNode;
  /** One line under the title saying what this screen answers. */
  sub?: ReactNode;
  /** Action buttons, right-aligned on the title row. */
  actions?: ReactNode;
  /** Tab strip — row two. Use <PageTabs>. */
  tabs?: ReactNode;
  /** Extra content for the right-hand context column. */
  aside?: ReactNode;
  /**
   * Suppress the automatic "Related" links in the aside. Set on screens where
   * the child surface IS the page's own subject.
   */
  hideRelated?: boolean;
  /** Hand the content row to the child untouched — it owns its own scrolling. */
  bleed?: boolean;
  children: ReactNode;
};

// AppShell is the app frame: a two-track grid of [sidebar][page].
//
//   grid-template-columns: auto minmax(0,1fr)
//
// The sidebar track is `auto`, so hiding the column below the nav breakpoint
// collapses it to zero without a second layout — the page simply becomes the
// only track. Everything inside the page track is PageShell's row grid.
export function AppShell({
  children,
  bleed = false,
  title,
  sub,
  actions,
  tabs,
  aside,
  hideRelated = false,
}: AppShellProps) {
  const pathname = usePathname() ?? '';
  // Self-host never renders Plans: a `docker compose up` operator has no plan
  // to be on and nothing to buy, so the item is dropped rather than disabled.
  const hosted = useAuthStore((s) => s.auth?.hosted ?? false);
  const items = navItemsFor({ hosted });
  const current = matchActiveHref(pathname, items);
  const groups = navGroups(items);
  const [navOpen, setNavOpen] = useState(false);

  // A tap on a nav item navigates; leaving the drawer open over the screen it
  // just took you to is the classic mobile-nav bug. Adjusted during render
  // rather than in an effect so the drawer never paints over the new page for
  // a frame — and so browser Back closes it too.
  const [navPath, setNavPath] = useState(pathname);
  if (navPath !== pathname) {
    setNavPath(pathname);
    if (navOpen) setNavOpen(false);
  }

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

  // Emptiness is decided here, not inside RelatedSurfacesNav: a component that
  // returns null is still a truthy element, so asking `related ? …` there would
  // hand PageShell an aside on every screen and leave a blank 240px column on
  // the ones with no child surfaces.
  const hasRelated = !hideRelated && childSurfacesFor(current, CHILD_SURFACES, { hosted }).some((s) => s.href !== pathname);
  const asideContent = aside || hasRelated
    ? (
      <>
        {aside}
        {hasRelated ? (
          <AsideSection title="Related">
            <RelatedSurfacesNav parentHref={current} currentHref={pathname} hosted={hosted} />
          </AsideSection>
        ) : null}
      </>
    )
    : null;

  return (
    <>
      {/* First tab stop on every page. Without it a keyboard user walks all
          eleven nav items before reaching the content, on every navigation. */}
      <a href="#main-content" className="skip-to-content">Skip to content</a>

      <div
        className="grid h-dvh min-h-0 overflow-hidden bg-[var(--background)]"
        style={{ gridTemplateColumns: 'auto minmax(0,1fr)' }}
      >
        {/* SideNav is `height: 100%` with its own internal scroll region, so the
            track only has to give it a height and a width — adding overflow here
            would make a second, competing scroller. */}
        <div
          className="hidden h-full min-h-0 border-e border-[var(--color-border)] lg:block"
          style={{ width: 'var(--sidebar-w)' }}
        >
          {sideNav}
        </div>

        <div className="grid min-h-0 min-w-0" style={{ gridTemplateRows: 'auto minmax(0,1fr)' }}>
          {/* Mobile top bar. Both rows are placed explicitly: `display:none`
              takes an element out of the grid entirely, so on desktop auto
              placement would slide the page up into the `auto` track and it
              would size to its content instead of filling the viewport. */}
          <div
            className="flex items-center gap-2 border-b border-[var(--color-border)] p-[var(--pad)] lg:hidden"
            style={{ gridRow: 1 }}
          >
            <IconButton
              label="Open navigation"
              icon={<Menu size={18} />}
              variant="ghost"
              size="sm"
              onClick={() => setNavOpen(true)}
            />
            <span className="truncate text-sm font-medium">{title ?? 'AgentRay'}</span>
          </div>

          <div className="min-h-0 min-w-0" style={{ gridRow: 2 }}>
            <PageShell
              banner={<DemoBar />}
              title={title}
              sub={sub}
              actions={actions}
              tabs={tabs}
              aside={asideContent}
              bleed={bleed}
            >
              {children}
            </PageShell>
          </div>
        </div>
      </div>

      {navOpen ? (
        <div className="fixed inset-0 z-50 lg:hidden">
          <button
            aria-label="Close navigation"
            className="absolute inset-0 bg-[color-mix(in_srgb,var(--background)_70%,transparent)]"
            onClick={() => setNavOpen(false)}
          />
          {/* A column, not a scroller: the close row is flex-none and SideNav
              takes the rest with min-h-0, so its own footer stays pinned inside
              the remaining height instead of running off the bottom. */}
          <div
            className="absolute inset-y-0 start-0 flex flex-col border-e border-[var(--color-border)] bg-[var(--color-background-body)] [animation:pop_var(--fast)_var(--ease)]"
            style={{ width: 'var(--sidebar-w)' }}
          >
            <div className="flex flex-none justify-end p-[var(--pad)]">
              <IconButton label="Close navigation" icon={<X size={18} />} variant="ghost" size="sm" onClick={() => setNavOpen(false)} />
            </div>
            <div className="min-h-0 flex-1">{sideNav}</div>
          </div>
        </div>
      ) : null}
    </>
  );
}
