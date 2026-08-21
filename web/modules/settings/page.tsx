'use client';

import { useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { settingsTabFromQuery } from '@/lib/ia';
import { AppShell } from '@/modules/shared/components/app-shell';
import { PageTabs } from '@/modules/shared/components/page-shell';

import { ActivityTab, ApiKeysTab, MembersTab, ProjectsTab, WorkspaceTab } from './settings-tabs';
import { ModelsTab } from './models-tab';
import { ConnectorsTab } from './connectors-tab';
import { PlanTab } from './plan-tab';

const TABS = ['Workspace', 'Plan & usage', 'Projects', 'Members', 'AI Provider', 'Data connectors', 'API keys', 'Activity'] as const;
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
    <AppShell
      active="settings"
      title="Settings"
      sub="Workspace, people, AI key, and how events get in."
      tabs={<PageTabs tabs={TABS.map((t) => ({ id: t, label: t }))} value={tab} onChange={setTab} />}
    >
      {tab === 'Workspace' ? <WorkspaceTab /> : null}
      {tab === 'Plan & usage' ? <PlanTab /> : null}
      {tab === 'Projects' ? <ProjectsTab /> : null}
      {tab === 'Members' ? <MembersTab /> : null}
      {tab === 'AI Provider' ? <ModelsTab /> : null}
      {tab === 'Data connectors' ? <ConnectorsTab /> : null}
      {tab === 'API keys' ? <ApiKeysTab /> : null}
      {tab === 'Activity' ? <ActivityTab /> : null}
    </AppShell>
  );
}
