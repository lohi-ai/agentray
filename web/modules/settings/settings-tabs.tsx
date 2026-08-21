'use client';

import { useMemo, useState } from 'react';
import { Plus } from 'lucide-react';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { apiBase, type Project, type WorkspaceAuditLog, type WorkspaceMember, type WorkspaceRole } from '@/lib/api';
import { InstrumentSnippet } from '@/modules/start/components/instrument-snippet';
import { formatCompact, formatNumber, formatRelative } from '@/lib/format';
import { projectAccess } from '@/lib/ia';
import { useCurrentProject, useProjectAccess, useWorkspaceAuditLogs, useWorkspaceMembers, useWorkspaceUsage } from '@/modules/app/hooks';
import { ConfirmDialog, PromptDialog } from '@/modules/shared/components/modal';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { Button, EmptyState, Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';

const ROLES: WorkspaceRole[] = ['owner', 'admin', 'member'];

export function WorkspaceTab() {
  const { workspaces, selectedWorkspaceID, updateWorkspace } = useCurrentProject();
  const { usage } = useWorkspaceUsage();
  const current = workspaces.find((w) => w.id === selectedWorkspaceID);
  // Renaming a workspace is a workspace write, so the role that decides is the
  // one held in THAT workspace — not in whichever project happens to be active.
  const access = projectAccess(current ? { role: current.role, is_demo: current.is_demo } : null);
  const [name, setName] = useState(current?.name ?? '');

  return (
    <Grid columns={{ minWidth: 440, max: 2 }} gap={4}>
      <Panel title="Workspace">
        <div className="mb-4 max-w-[440px]">
          <TextInput
            label="Workspace name"
            value={name}
            placeholder={current?.name}
            onChange={(v) => setName(v)}
            width="100%"
            isDisabled={!access.canWrite}
          />
        </div>
        {access.canWrite ? null : <Text type="supporting" className="mb-2 block">{access.reason}</Text>}
        <Button variant="primary" size="sm" disabled={!access.canWrite} tooltip={access.reason || undefined} onClick={() => name.trim() && void updateWorkspace(name.trim())}>Save changes</Button>
      </Panel>
      {/* This panel reads the shared date filter; the plan meter on the Plan tab
          reads the calendar month. Two numbers a tab apart, both once labelled
          "Events" — so this one names its window, and points at the billed one. */}
      <Panel title="Activity in the selected range">
        <VStack gap={4} align="stretch">
          <StatsStrip stats={[
            { label: 'Projects', value: formatNumber(usage?.project_count ?? 0) },
            { label: 'Events', value: formatCompact(usage?.event_count ?? 0) },
            { label: 'People', value: formatCompact(usage?.distinct_users ?? 0) },
          ]} />
          <Text type="supporting">
            Events counts everything ingested, crawlers included — that is what your plan meters.
            People counts identified humans, so one person who signs in mid-visit is one person.
            For the figure your plan ceiling is measured against, see the Plan tab.
          </Text>
        </VStack>
      </Panel>
    </Grid>
  );
}

export function ProjectsTab() {
  const { projects, project, selectProject, createProject, updateProject, workspaces, selectedWorkspaceID } = useCurrentProject();
  const workspace = workspaces.find((w) => w.id === selectedWorkspaceID);
  const access = projectAccess(workspace ? { role: workspace.role, is_demo: workspace.is_demo } : null);
  const [dialog, setDialog] = useState<'create' | 'rename' | null>(null);

  const columns = useMemo<DataColumn<Project>[]>(() => [
    {
      key: 'name',
      header: 'Project',
      renderCell: (p) => {
        const active = p.id === project?.id;
        return <span>{active ? <b>{p.name}</b> : p.name}{active ? <span className="text-[var(--color-text-secondary)]"> · active</span> : null}</span>;
      },
    },
    {
      key: 'created_at',
      header: 'Created',
      sortValue: (p) => p.created_at,
      renderCell: (p) => <span className="text-[var(--color-text-secondary)]">{formatRelative(p.created_at)}</span>,
    },
    {
      key: 'actions',
      header: '',
      hideable: false,
      sortable: false,
      align: 'end',
      renderCell: (p) => p.id === project?.id
        ? <Button variant="ghost" size="sm" disabled={!projectAccess(p).canWrite} tooltip={projectAccess(p).reason || undefined} onClick={() => setDialog('rename')}>Rename</Button>
        : <Button variant="ghost" size="sm" onClick={() => void selectProject(p.id)}>Switch</Button>,
    },
  ], [project?.id, selectProject]);

  return (
    <>
      {dialog === 'create' ? (
        <PromptDialog title="New project" label="Project name" placeholder="e.g. Production" submitLabel="Create project" onSubmit={(n) => void createProject(n)} onClose={() => setDialog(null)} />
      ) : null}
      {dialog === 'rename' && project ? (
        <PromptDialog title="Rename project" label="Project name" defaultValue={project.name} submitLabel="Rename" onSubmit={(n) => void updateProject(n)} onClose={() => setDialog(null)} />
      ) : null}
      {projects.length === 0 ? (
        <Panel title="Projects" action={<Button variant="outline" size="sm" icon={<Plus size={15} />} disabled={!access.canWrite} tooltip={access.reason || undefined} onClick={() => setDialog('create')}>New project</Button>}>
          <EmptyState title="No projects" detail="Create a project to start ingesting events." />
        </Panel>
      ) : (
        <DataTable
          title="Projects"
          columns={columns}
          data={projects}
          action={<Button variant="outline" size="sm" icon={<Plus size={15} />} disabled={!access.canWrite} tooltip={access.reason || undefined} onClick={() => setDialog('create')}>New project</Button>}
          onRowClick={(p) => void selectProject(p.id)}
        />
      )}
    </>
  );
}

export function MembersTab() {
  const { members, loading, addMember, updateMemberRole, removeMember } = useWorkspaceMembers();
  const { workspaces, selectedWorkspaceID } = useCurrentProject();
  const workspace = workspaces.find((w) => w.id === selectedWorkspaceID);
  const access = projectAccess(workspace ? { role: workspace.role, is_demo: workspace.is_demo } : null);
  // Membership is owner/admin territory; the shared demo's viewers see the
  // roster and can change none of it.
  const canManage = access.canWrite && (workspace?.role === 'owner' || workspace?.role === 'admin');
  const [inviting, setInviting] = useState(false);

  const columns = useMemo<DataColumn<WorkspaceMember>[]>(() => [
    {
      key: 'name',
      header: 'Name',
      renderCell: (m) => <span>{m.name || '—'}</span>,
    },
    {
      key: 'email',
      header: 'Email',
      renderCell: (m) => <span className="text-[var(--color-text-secondary)]">{m.email}</span>,
    },
    {
      key: 'role',
      header: 'Role',
      renderCell: (m) => (
        <Selector
          label="Role"
          isLabelHidden
          size="sm"
          options={ROLES as string[]}
          value={m.role}
          onChange={(v) => void updateMemberRole(m.user_id, v as WorkspaceRole)}
          isDisabled={m.role === 'owner' || !canManage}
        />
      ),
    },
    {
      key: 'actions',
      header: '',
      hideable: false,
      sortable: false,
      align: 'end',
      renderCell: (m) => m.role !== 'owner' && canManage
        ? <Button variant="ghost" size="sm" onClick={() => void removeMember(m.user_id)}><span style={{ color: 'var(--danger)' }}>Remove</span></Button>
        : null,
    },
  ], [updateMemberRole, removeMember, canManage]);

  return (
    <>
      {inviting ? (
        <PromptDialog title="Invite member" label="Email address" placeholder="teammate@company.com" submitLabel="Send invite" onSubmit={(email) => void addMember(email, 'member')} onClose={() => setInviting(false)} />
      ) : null}
      {loading && members.length === 0 ? (
        <Panel title="Members"><Loading label="Loading members…" /></Panel>
      ) : (
        <DataTable
          title="Members"
          columns={columns}
          data={members}
          searchPlaceholder="Search members…"
          action={<Button variant="outline" size="sm" icon={<Plus size={15} />} disabled={!canManage} tooltip={access.reason || undefined} onClick={() => setInviting(true)}>Invite</Button>}
          emptyMessage="No members yet."
        />
      )}
    </>
  );
}

export function ApiKeysTab() {
  const { project, rotateKey } = useCurrentProject();
  const access = useProjectAccess();
  const [revealed, setRevealed] = useState(false);
  const [rotating, setRotating] = useState(false);
  // The API blanks the key for a membership that may not write it, so there is
  // genuinely nothing to reveal, rotate, or paste into a page here.
  const key = project?.api_key ?? '';
  const masked = key ? `${key.slice(0, 8)}••••••••${key.slice(-4)}` : '—';

  if (!access.canWrite) {
    return (
      <Panel title="API keys">
        <EmptyState
          title="This project's key isn't yours"
          detail={`${access.reason} Your own project has its own key — switch to it and this page hands you the snippet.`}
        />
      </Panel>
    );
  }

  return (
    <Panel title="API keys">
      {rotating ? (
        <ConfirmDialog title="Rotate API key?" detail="The old key is revoked immediately. Update any running agents or integrations first." confirmLabel="Rotate key" danger onConfirm={() => void rotateKey()} onClose={() => setRotating(false)} />
      ) : null}
      <HStack align="center" gap={2} className="max-w-[560px] rounded-md bg-[var(--color-background-muted)] px-3 py-[10px] text-[12.5px]">
        <StatusDot variant="success" label="Key active" isPulsing />
        <span className="font-mono tabular-nums">{revealed ? key : masked}</span>
        <span className="text-[var(--color-text-disabled)] ms-auto">{project ? `created ${formatRelative(project.created_at)}` : ''}</span>
        <Button variant="ghost" size="sm" onClick={() => setRevealed((v) => !v)}>{revealed ? 'Hide' : 'Reveal'}</Button>
        <Button variant="ghost" size="sm" onClick={() => setRotating(true)}><span style={{ color: 'var(--danger)' }}>Rotate</span></Button>
      </HStack>
      <Text type="supporting" className="mt-2.5 block max-w-[480px]">Rotating a key immediately revokes the old one. Update any running agents or integrations first.</Text>
      {/* The key on its own is not an on-ramp. Until this existed the only
          documented install was `npm install @agentray/browser`, which is
          useless to an owner whose prototype is a Framer page — so the key sat
          here and nothing ever arrived. */}
      <div className="mt-5 border-t border-[var(--color-border)] pt-4">
        <Text type="supporting" className="mb-3 block">Paste this into your site and events start arriving. No build step required.</Text>
        <InstrumentSnippet apiKey={key} host={apiBase()} />
      </div>
    </Panel>
  );
}

export function ActivityTab() {
  const { logs, loading } = useWorkspaceAuditLogs();

  const columns = useMemo<DataColumn<WorkspaceAuditLog>[]>(() => [
    {
      key: 'actor_email',
      header: 'Who',
      renderCell: (l) => <span className="text-[var(--color-text-secondary)]">{l.actor_email}</span>,
    },
    {
      key: 'action',
      header: 'Action',
    },
    {
      key: 'target',
      header: 'Target',
      searchValue: (l) => l.target_label || l.target_type,
      sortValue: (l) => l.target_label || l.target_type,
      renderCell: (l) => <span className="text-[var(--color-text-secondary)]">{l.target_label || l.target_type}</span>,
    },
    {
      key: 'created_at',
      header: 'When',
      sortValue: (l) => l.created_at,
      renderCell: (l) => <span className="font-mono text-[var(--color-text-secondary)]">{formatRelative(l.created_at)}</span>,
    },
  ], []);

  if (loading && logs.length === 0) return <Panel title="Recent activity"><Loading label="Loading activity…" /></Panel>;
  if (logs.length === 0) return <Panel title="Recent activity"><EmptyState title="No activity yet" detail="Workspace changes will show up here." /></Panel>;

  return <DataTable title="Recent activity" columns={columns} data={logs} searchPlaceholder="Search activity…" pageSize={20} />;
}
