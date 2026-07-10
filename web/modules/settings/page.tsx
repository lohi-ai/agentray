'use client';

import { useState } from 'react';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Intro } from '@/modules/shared/components/signal-primitives';
import { ActivityTab, ApiKeysTab, MembersTab, ProjectsTab, WorkspaceTab } from './settings-tabs';
import { ModelsTab } from './models-tab';

const TABS = ['Workspace', 'Projects', 'Members', 'AI Provider', 'API keys', 'Activity'] as const;
type Tab = (typeof TABS)[number];

export function SettingsPage() {
  const [tab, setTab] = useState<Tab>('Workspace');

  return (
    <AppShell active="settings">
      <Intro title="Settings" sub="Workspace, access, and API safety." />
      <div className="mb-[18px] flex gap-1 border-b border-[var(--color-border)]">
        {TABS.map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`relative h-9 px-3 text-[13px] ${t === tab ? "text-[var(--color-text-primary)] after:absolute after:inset-x-2 after:-bottom-px after:h-0.5 after:rounded-full after:bg-primary after:content-['']" : 'text-[var(--color-text-secondary)]'}`}
          >
            {t}
          </button>
        ))}
      </div>
      {tab === 'Workspace' ? <WorkspaceTab /> : null}
      {tab === 'Projects' ? <ProjectsTab /> : null}
      {tab === 'Members' ? <MembersTab /> : null}
      {tab === 'AI Provider' ? <ModelsTab /> : null}
      {tab === 'API keys' ? <ApiKeysTab /> : null}
      {tab === 'Activity' ? <ActivityTab /> : null}
    </AppShell>
  );
}
