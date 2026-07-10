'use client';

import { useMemo, useState } from 'react';
import { Plus } from 'lucide-react';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import type { Project, WorkspaceAuditLog, WorkspaceMember, WorkspaceRole } from '@/lib/api';
import { formatCompact, formatNumber, formatRelative } from '@/lib/format';
import { useCurrentProject, useWorkspaceAuditLogs, useWorkspaceMembers, useWorkspaceUsage } from '@/modules/app/hooks';
import { ConfirmDialog, PromptDialog } from '@/modules/shared/components/modal';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { Button, EmptyState, Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';

const ROLES: WorkspaceRole[] = ['owner', 'admin', 'member'];

export function WorkspaceTab() {
  const { workspaces, selectedWorkspaceID, updateWorkspace } = useCurrentProject();
  const { usage } = useWorkspaceUsage();
  const current = workspaces.find((w) => w.id === selectedWorkspaceID);
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
          />
        </div>
        <Button variant="primary" size="sm" onClick={() => name.trim() && void updateWorkspace(name.trim())}>Save changes</Button>
      </Panel>
      <Panel title="Usage">
        <StatsStrip stats={[
          { label: 'Projects', value: formatNumber(usage?.project_count ?? 0) },
          { label: 'Events', value: formatCompact(usage?.event_count ?? 0) },
          { label: 'People', value: formatCompact(usage?.distinct_users ?? 0) },
        ]} />
      </Panel>
    </Grid>
  );
}

export function ProjectsTab() {
  const { projects, project, selectProject, createProject, updateProject } = useCurrentProject();
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
        ? <Button variant="ghost" size="sm" onClick={() => setDialog('rename')}>Rename</Button>
        : null,
    },
  ], [project?.id]);

  return (
    <>
      {dialog === 'create' ? (
        <PromptDialog title="New project" label="Project name" placeholder="e.g. Production" submitLabel="Create project" onSubmit={(n) => void createProject(n)} onClose={() => setDialog(null)} />
      ) : null}
      {dialog === 'rename' && project ? (
        <PromptDialog title="Rename project" label="Project name" defaultValue={project.name} submitLabel="Rename" onSubmit={(n) => void updateProject(n)} onClose={() => setDialog(null)} />
      ) : null}
      {projects.length === 0 ? (
        <Panel title="Projects" action={<Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={() => setDialog('create')}>New project</Button>}>
          <EmptyState title="No projects" detail="Create a project to start ingesting events." />
        </Panel>
      ) : (
        <DataTable
          title="Projects"
          columns={columns}
          data={projects}
          action={<Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={() => setDialog('create')}>New project</Button>}
          onRowClick={(p) => void selectProject(p.id)}
        />
      )}
    </>
  );
}

export function MembersTab() {
  const { members, loading, addMember, updateMemberRole, removeMember } = useWorkspaceMembers();
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
          isDisabled={m.role === 'owner'}
        />
      ),
    },
    {
      key: 'actions',
      header: '',
      hideable: false,
      sortable: false,
      align: 'end',
      renderCell: (m) => m.role !== 'owner'
        ? <Button variant="ghost" size="sm" onClick={() => void removeMember(m.user_id)}><span style={{ color: 'var(--danger)' }}>Remove</span></Button>
        : null,
    },
  ], [updateMemberRole, removeMember]);

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
          action={<Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={() => setInviting(true)}>Invite</Button>}
          emptyMessage="No members yet."
        />
      )}
    </>
  );
}

export function ApiKeysTab() {
  const { project, rotateKey } = useCurrentProject();
  const [revealed, setRevealed] = useState(false);
  const [rotating, setRotating] = useState(false);
  const key = project?.api_key ?? '';
  const masked = key ? `${key.slice(0, 8)}••••••••${key.slice(-4)}` : '—';

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
