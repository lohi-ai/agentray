'use client';

import { useState } from 'react';
import { Check, FolderPlus, Settings, Waypoints } from 'lucide-react';
import { Divider } from '@astryxdesign/core/Divider';
import { NavHeadingMenu, NavHeadingMenuItem } from '@astryxdesign/core/NavMenu';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { SideNavHeading } from '@astryxdesign/core/SideNav';
import { settingsPath } from '@/lib/ia';
import { useAuthStore } from '@/lib/app-state';
import { useCurrentProject } from '@/modules/app/hooks';
import { PromptDialog } from '@/modules/shared/components/modal';

function IconSlot({ filled }: { filled: boolean }) {
  return filled ? <Check size={16} aria-hidden /> : <span className="inline-block size-4" aria-hidden />;
}

function ProjectMenuList({ onCreate }: { onCreate: () => void }) {
  const projects = useAuthStore((s) => s.projects);
  const project = useAuthStore((s) => s.project);
  const { selectProject } = useCurrentProject();

  return (
    <NavHeadingMenu>
      {projects.length === 0 ? (
        <NavHeadingMenuItem label="No projects yet" isDisabled />
      ) : (
        projects.map((p) => (
          <NavHeadingMenuItem
            key={p.id}
            label={p.name}
            description={p.id === project?.id ? 'Current' : undefined}
            icon={<IconSlot filled={p.id === project?.id} />}
            onClick={() => void selectProject(p.id)}
          />
        ))
      )}
      <Divider variant="subtle" />
      <NavHeadingMenuItem label="New project" icon={FolderPlus} onClick={onCreate} />
      <NavHeadingMenuItem label="Manage projects" icon={Settings} href={settingsPath('projects')} />
    </NavHeadingMenu>
  );
}

// Dialog lives next to the heading, not inside the hover popover.
export function ProjectSwitcher() {
  const project = useAuthStore((s) => s.project);
  const workspaces = useAuthStore((s) => s.workspaces);
  const selectedWorkspaceID = useAuthStore((s) => s.selectedWorkspaceID);
  const workspace = workspaces.find((w) => w.id === selectedWorkspaceID) ?? workspaces[0];
  const { createProject } = useCurrentProject();
  const [creating, setCreating] = useState(false);

  return (
    <>
      {creating ? (
        <PromptDialog
          title="New project"
          label="Project name"
          placeholder="e.g. Production"
          submitLabel="Create project"
          onSubmit={(n) => void createProject(n)}
          onClose={() => setCreating(false)}
        />
      ) : null}
      <SideNavHeading
        heading={project?.name ?? 'No project'}
        subheading={workspace?.name || 'workspace'}
        icon={<NavIcon icon={<Waypoints size={16} />} />}
        menu={<ProjectMenuList onCreate={() => setCreating(true)} />}
      />
    </>
  );
}
