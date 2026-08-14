'use client';

import { useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { settingsTabFromQuery } from '@/lib/ia';
import { AppShell } from '@/modules/shared/components/app-shell';
import { RelatedSurfacesLabel } from '@/modules/shared/components/related-surfaces';
import { Intro } from '@/modules/shared/components/signal-primitives';
import { ActivityTab, ApiKeysTab, MembersTab, ProjectsTab, WorkspaceTab } from './settings-tabs';
import { ModelsTab } from './models-tab';
import { ConnectorsTab } from './connectors-tab';

const TABS = ['Workspace', 'Projects', 'Members', 'AI Provider', 'Data connectors', 'API keys', 'Activity'] as const;
type Tab = (typeof TABS)[number];

export function SettingsPage() {
  const search = useSearchParams();
  const query = search.toString();
  const fromQuery = settingsTabFromQuery(query);
  const requested = (TABS as readonly string[]).includes(fromQuery) ? (fromQuery as Tab) : 'Workspace';
  const [tab, setTab] = useState<Tab>(requested);
  // ?tab= is a deep link, and `router.push('/settings?tab=ai')` from a surface
  // already on /settings only changes the query — no remount, so useState's
  // initial value would never be re-read and the deep link would do nothing.
  const [syncedQuery, setSyncedQuery] = useState(query);
  if (query !== syncedQuery) {
    setSyncedQuery(query);
    setTab(requested);
  }

  return (
    <AppShell active="settings">
      <Intro title="Settings" sub="Workspace, people, AI key, and how events get in." />
      <div className="mb-3"><RelatedSurfacesLabel parentHref="/settings" /></div>
      <div className="mb-[18px] flex gap-1 border-b border-[var(--color-border)]">
        {TABS.map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`relative min-h-11 px-3 text-[13px] ${t === tab ? "text-[var(--color-text-primary)] after:absolute after:inset-x-2 after:-bottom-px after:h-0.5 after:rounded-full after:bg-primary after:content-['']" : 'text-[var(--color-text-secondary)]'}`}
          >
            {t}
          </button>
        ))}
      </div>
      {tab === 'Workspace' ? <WorkspaceTab /> : null}
      {tab === 'Projects' ? <ProjectsTab /> : null}
      {tab === 'Members' ? <MembersTab /> : null}
      {tab === 'AI Provider' ? <ModelsTab /> : null}
      {tab === 'Data connectors' ? <ConnectorsTab /> : null}
      {tab === 'API keys' ? <ApiKeysTab /> : null}
      {tab === 'Activity' ? <ActivityTab /> : null}
    </AppShell>
  );
}
