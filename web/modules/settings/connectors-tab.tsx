'use client';

import { useMemo, useRef, useState } from 'react';
import { Plus, Sparkles } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { HStack } from '@astryxdesign/core/HStack';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import {
  AgentRayAPI,
  newIdempotencyKey,
  type ConnectorSync,
  type ConnectorSyncDraft,
  type ConnectorSyncInput,
  type ConnectorTable,
  type DataConnector,
} from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';
import { formatCompact, formatRelative } from '@/lib/format';
import { useConnectors, useConnectorSchema, useConnectorSyncs, useDatasetPreview } from '@/modules/app/hooks/connectors';
import { ConfirmDialog, Modal, PromptDialog } from '@/modules/shared/components/modal';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { Button, EmptyState, Loading, Panel } from '@/modules/shared/components/signal-primitives';

// Data connectors settings tab: configure an external source (DSN write-only),
// test it, browse its schema, and set up per-table syncs into the analytics
// store. Agents then query the landed rows through run_sql (`external_rows`).
export function ConnectorsTab() {
  const { connectors, kinds, loading, create, remove } = useConnectors();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState<DataConnector | null>(null);
  const [testResult, setTestResult] = useState<{ id: string; ok: boolean; error?: string } | null>(null);
  const [testing, setTesting] = useState<string | null>(null);

  const selected = connectors.find((c) => c.id === selectedID) ?? null;

  async function testConnector(id: string) {
    if (!projectID) return;
    setTesting(id);
    setTestResult(null);
    try {
      const res = await new AgentRayAPI(projectID).testConnector(id);
      setTestResult({ id, ok: res.ok, error: res.error });
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Unable to test connection');
    } finally {
      setTesting(null);
    }
  }

  const columns = useMemo<DataColumn<DataConnector>[]>(() => [
    {
      key: 'name',
      header: 'Connector',
      renderCell: (c) => {
        const active = c.id === selectedID;
        return <span>{active ? <b>{c.name}</b> : c.name}</span>;
      },
    },
    { key: 'kind', header: 'Kind' },
    {
      key: 'created_at',
      header: 'Added',
      sortValue: (c) => c.created_at,
      renderCell: (c) => <span className="text-[var(--color-text-secondary)]">{formatRelative(c.created_at)}</span>,
    },
    {
      key: 'actions',
      header: '',
      hideable: false,
      sortable: false,
      align: 'end',
      width: { type: 'pixel', value: 150 },
      renderCell: (c) => (
        <span className="flex justify-end gap-1">
          <Button variant="ghost" size="sm" onClick={() => void testConnector(c.id)}>
            {testing === c.id ? 'Testing…' : 'Test'}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setDeleting(c)}>
            <span style={{ color: 'var(--danger)' }}>Delete</span>
          </Button>
        </span>
      ),
    },
    // testConnector is stable enough for this table; testing drives the label.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  ], [selectedID, testing]);

  return (
    <>
      {adding ? (
        <AddConnectorDialog
          kinds={kinds}
          onSubmit={(input) => create.mutateAsync(input).then((r) => setSelectedID(r.connector.id))}
          onClose={() => setAdding(false)}
        />
      ) : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete connector “${deleting.name}”?`}
          detail="Sync configs are removed with it. Rows already synced stay queryable in the analytics store."
          confirmLabel="Delete connector"
          danger
          onConfirm={() => {
            if (selectedID === deleting.id) setSelectedID(null);
            void remove.mutate({ id: deleting.id, revision: deleting.revision, idempotencyKey: newIdempotencyKey() });
          }}
          onClose={() => setDeleting(null)}
        />
      ) : null}

      {loading && connectors.length === 0 ? (
        <Panel title="Data connectors"><Loading label="Loading connectors…" /></Panel>
      ) : connectors.length === 0 ? (
        <Panel
          title="Data connectors"
          action={<Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={() => setAdding(true)}>Add connector</Button>}
        >
          <EmptyState title="No connectors" detail="Connect an external database to sync its tables into analytics. Agents query the synced rows via run_sql." />
        </Panel>
      ) : (
        <>
          <DataTable
            title="Data connectors"
            columns={columns}
            data={connectors}
            action={<Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={() => setAdding(true)}>Add connector</Button>}
            onRowClick={(c) => setSelectedID(c.id)}
          />
          {testResult ? (
            <Text type="supporting" className="mt-2 block">
              {testResult.ok
                ? <span style={{ color: 'var(--success, var(--color-text-primary))' }}>Connection OK.</span>
                : <span style={{ color: 'var(--danger)' }}>Connection failed: {testResult.error}</span>}
            </Text>
          ) : null}
          <div className="mt-4">
            {selected ? (
              <SyncsPanel connector={selected} />
            ) : (
              <Panel title="Table syncs">
                <EmptyState title="Pick a connector" detail="Select a connector above to configure which tables to sync." />
              </Panel>
            )}
          </div>
        </>
      )}
    </>
  );
}

function AddConnectorDialog({ kinds, onSubmit, onClose }: {
  kinds: string[];
  onSubmit: (input: { name: string; kind: string; dsn: string; idempotencyKey: string }) => Promise<unknown>;
  onClose: () => void;
}) {
  const [name, setName] = useState('');
  const [kind, setKind] = useState(kinds[0] ?? 'postgres');
  const [dsn, setDsn] = useState('');
  const [submitting, setSubmitting] = useState(false);
  // Kept for this open dialog, not minted per click: after a lost response
  // the operator retries the exact transaction rather than creating a second
  // source credential/connector pair.
  const idempotencyKey = useRef(newIdempotencyKey());

  async function submit() {
    if (!name.trim() || !dsn.trim() || submitting) return;
    setSubmitting(true);
    try {
      await onSubmit({ name: name.trim(), kind, dsn: dsn.trim(), idempotencyKey: idempotencyKey.current });
      onClose();
    } catch {
      // The hook already exposes the actionable API error. Keep this dialog
      // open with its original key so a retry is an idempotent replay.
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Modal
      title="Add data connector"
      onClose={() => { if (!submitting) onClose(); }}
      footer={<><Button variant="ghost" size="sm" disabled={submitting} onClick={onClose}>Cancel</Button><Button variant="primary" size="sm" disabled={submitting} onClick={submit}>{submitting ? 'Adding…' : 'Add connector'}</Button></>}
    >
      <div className="flex flex-col gap-4 max-w-[440px]">
        <TextInput label="Name" value={name} placeholder="e.g. Production DB" onChange={setName} width="100%" />
        <Selector label="Kind" size="sm" options={kinds} value={kind} onChange={setKind} />
        <TextInput
          label="Connection string"
          type="password"
          value={dsn}
          placeholder="postgres://user:password@host:5432/db"
          onChange={setDsn}
          onEnter={submit}
          width="100%"
        />
        <Text type="supporting">The connection string is encrypted at rest and never shown again. Use a read-only database user.</Text>
      </div>
    </Modal>
  );
}

function SyncsPanel({ connector }: { connector: DataConnector }) {
  const { syncs, loading, create, update, remove, run, cancel, setEnabled } = useConnectorSyncs(connector.id);
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<ConnectorSync | null>(null);
  const [previewing, setPreviewing] = useState<ConnectorSync | null>(null);
  const [drafting, setDrafting] = useState(false);
  const [draft, setDraft] = useState<ConnectorSyncDraft | null>(null);
  const [draftLoading, setDraftLoading] = useState(false);
  const [running, setRunning] = useState<string | null>(null);
  async function requestDraft(hint: string) {
    if (!projectID) return;
    setDraftLoading(true);
    try {
      setDraft(await new AgentRayAPI(projectID).draftConnectorSyncs(connector.id, hint));
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Unable to draft syncs');
    } finally {
      setDraftLoading(false);
    }
  }

  const columns = useMemo<DataColumn<ConnectorSync>[]>(() => [
    { key: 'source_table', header: 'Table', width: { type: 'proportional', value: 1, minWidth: 90 } },
    {
      key: 'key_column',
      header: 'Key / cursor',
      sortable: false,
      width: { type: 'proportional', value: 1, minWidth: 120 },
      renderCell: (s) => <span className="font-mono text-[var(--color-text-secondary)]">{s.key_column}{s.cursor_column ? ` / ${s.cursor_column}` : ' / full re-sync'}</span>,
    },
    {
      key: 'join_key',
      header: 'Join',
      sortable: false,
      width: { type: 'proportional', value: 1, minWidth: 110 },
      renderCell: (s) => {
        if (!s.join_key) return <span className="text-[var(--color-text-disabled)]">—</span>;
        return (
          <HStack gap={1} align="center">
            <span className="font-mono text-[var(--color-text-secondary)]">{s.join_key}</span>
            {s.join_validated === 'validated' ? (
              <Badge variant="success" label="validated" />
            ) : s.join_validated === 'unvalidated' ? (
              <Badge variant="warning" label="unvalidated" />
            ) : null}
          </HStack>
        );
      },
    },
    {
      key: 'deletion_mode',
      header: 'Deletions',
      sortable: false,
      width: { type: 'proportional', value: 1, minWidth: 110 },
      renderCell: (s) =>
        s.deletion_mode === 'soft_column' ? (
          <span className="font-mono text-[var(--color-text-secondary)]" title={`${s.soft_delete_column} · ${s.soft_delete_semantics}`}>
            {s.soft_delete_column}
          </span>
        ) : (
          <span className="text-[var(--color-text-disabled)]">none</span>
        ),
    },
    {
      key: 'schedule_cron',
      header: 'Schedule',
      width: { type: 'proportional', value: 1, minWidth: 90 },
      renderCell: (s) => <span className="font-mono text-[var(--color-text-secondary)]">{s.schedule_cron || 'manual'}</span>,
    },
    {
      key: 'last_status',
      header: 'Last run',
      width: { type: 'proportional', value: 2, minWidth: 150 },
      renderCell: (s) => {
        // A live receipt outranks the sync's last_* columns: those only move
        // when the run finishes, so a queued/running run would otherwise read
        // as its predecessor's outcome.
        const live = s.latest_run;
        if (live && (live.status === 'queued' || live.status === 'running')) {
          return (
            <span style={{ color: 'var(--color-text-secondary)' }}>
              {live.status === 'queued' ? 'queued' : `running · ${formatCompact(live.rows)} rows`}
              {live.cancel_requested ? ' · cancelling' : ''} · {formatRelative(live.queued_at)}
            </span>
          );
        }
        if (live && live.status === 'cancelled') {
          return <span className="text-[var(--color-text-disabled)]">cancelled · {formatRelative(live.queued_at)}</span>;
        }
        if (!s.last_run_at) return <span className="text-[var(--color-text-disabled)]">never</span>;
        if (s.last_status === 'error') {
          // A run that landed rows before it failed is partial, not a clean
          // failure — the dataset is stale AND populated, which reads
          // differently from "error, nothing landed".
          if (s.last_rows > 0) {
            return (
              <span style={{ color: 'var(--warning)' }} title={s.last_error}>
                partial — {formatCompact(s.last_rows)} rows landed · {formatRelative(s.last_run_at)}
              </span>
            );
          }
          return <span style={{ color: 'var(--danger)' }} title={s.last_error}>error · {formatRelative(s.last_run_at)}</span>;
        }
        return <span className="text-[var(--color-text-secondary)]">ok · {formatCompact(s.last_rows)} rows · {formatRelative(s.last_run_at)}</span>;
      },
    },
    {
      key: 'total_rows',
      header: 'Total rows',
      width: { type: 'proportional', value: 1, minWidth: 80 },
      sortValue: (s) => s.total_rows,
      renderCell: (s) => <span className="tabular-nums">{formatCompact(s.total_rows)}</span>,
    },
    {
      key: 'enabled',
      header: 'Enabled',
      width: { type: 'pixel', value: 72 },
      renderCell: (s) => (
        <Button variant="ghost" size="sm" onClick={() => void setEnabled.mutate({ sync: s, enabled: !s.enabled })}>
          {s.enabled ? 'On' : 'Off'}
        </Button>
      ),
    },
    {
      key: 'actions',
      header: '',
      hideable: false,
      sortable: false,
      align: 'end',
      width: { type: 'pixel', value: 260 },
      renderCell: (s) => (
        <span className="flex justify-end gap-1">
          <Button variant="ghost" size="sm" onClick={() => setPreviewing(s)}>Preview</Button>
          <Button variant="ghost" size="sm" onClick={() => setEditing(s)}>Edit</Button>
          {s.latest_run && (s.latest_run.status === 'queued' || s.latest_run.status === 'running') ? (
            <Button
              variant="ghost"
              size="sm"
              disabled={s.latest_run.cancel_requested}
              onClick={() => void cancel.mutate(s.latest_run!.id)}
            >
              {s.latest_run.cancel_requested ? 'Cancelling…' : 'Cancel'}
            </Button>
          ) : (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                setRunning(s.id);
                void run.mutateAsync({ id: s.id, idempotencyKey: newIdempotencyKey() }).finally(() => setRunning(null));
              }}
            >
              {running === s.id ? 'Running…' : 'Run now'}
            </Button>
          )}
          <Button variant="ghost" size="sm" onClick={() => void remove.mutate(s.id)}>
            <span style={{ color: 'var(--danger)' }}>Delete</span>
          </Button>
        </span>
      ),
    },
    // update/run/cancel/remove are react-query mutations (stable identities).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  ], [running, setError]);

  const lastError = syncs.find((s) => s.last_status === 'error')?.last_error;

  return (
    <>
      {adding ? (
        <AddSyncDialog
          connectorID={connector.id}
          onSubmit={(input) => void create.mutate(input)}
          onClose={() => setAdding(false)}
        />
      ) : null}
      {editing ? (
        <EditSyncDialog
          connectorID={connector.id}
          sync={editing}
          onSubmit={(input) => void update.mutateAsync({ id: editing.id, input }).then(() => setEditing(null))}
          onClose={() => setEditing(null)}
        />
      ) : null}
      {previewing ? (
        <DatasetPreviewDialog sync={previewing} onClose={() => setPreviewing(null)} />
      ) : null}
      {drafting ? (
        <PromptDialog
          title="Draft syncs with AI"
          label="What do you want to analyze? (optional hint)"
          placeholder="e.g. reader activity and payments"
          defaultValue="propose syncs"
          submitLabel={draftLoading ? 'Drafting…' : 'Draft'}
          onSubmit={(hint) => void requestDraft(hint)}
          onClose={() => setDrafting(false)}
        />
      ) : null}
      {draft ? (
        <DraftReviewDialog
          draft={draft}
          onApprove={(input) => create.mutateAsync(input)}
          onClose={() => setDraft(null)}
        />
      ) : null}

      <Panel
        title={`Table syncs — ${connector.name}`}
        action={
          <span className="flex gap-2">
            <Button variant="ghost" size="sm" icon={<Sparkles size={14} />} onClick={() => setDrafting(true)}>
              {draftLoading ? 'Drafting…' : 'AI draft'}
            </Button>
            <Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={() => setAdding(true)}>Add sync</Button>
          </span>
        }
      >
        {loading && syncs.length === 0 ? (
          <Loading label="Loading syncs…" />
        ) : syncs.length === 0 ? (
          <EmptyState title="No syncs configured" detail="Add a table sync (or let AI draft one) to start pulling rows into analytics." />
        ) : (
          <>
            <DataTable columns={columns} data={syncs} pageSize={10} />
            {lastError ? (
              <Text type="supporting" className="mt-2 block" style={{ color: 'var(--danger)' }}>Last error: {lastError}</Text>
            ) : null}
            <Text type="supporting" className="mt-2 block">
              Synced rows land in the <span className="font-mono">external_rows</span> table — agents and SQL can read them, e.g.{' '}
              <span className="font-mono">SELECT json_extract_string(data, &apos;$.email&apos;) FROM external_rows WHERE table_name = &apos;{syncs[0]?.source_table ?? 'users'}&apos;</span>.
            </Text>
          </>
        )}
      </Panel>
    </>
  );
}

// syncInputOf rebuilds the full editable shape from a sync row — the PUT body
// overwrites every field, so a partial input would silently clear join_key
// and the deletion settings.
function syncInputOf(s: ConnectorSync, overrides: Partial<ConnectorSyncInput> = {}): ConnectorSyncInput {
  return {
    source_table: s.source_table,
    key_column: s.key_column,
    cursor_column: s.cursor_column,
    schedule_cron: s.schedule_cron,
    enabled: s.enabled,
    join_key: s.join_key,
    deletion_mode: s.deletion_mode || 'none',
    soft_delete_column: s.soft_delete_column,
    soft_delete_semantics: s.soft_delete_semantics,
    ...overrides,
  };
}

const DELETION_MODE_OPTIONS = [
  { value: 'none', label: 'Not tracked' },
  { value: 'soft_column', label: 'Source marks deletions' },
];

const SOFT_DELETE_SEMANTICS_OPTIONS = [
  { value: 'bool_true', label: 'Deleted when the value is true' },
  { value: 'non_null', label: 'Deleted when the value is set (e.g. deleted_at)' },
];

// SyncSemanticsFields is the join + deletion block shared by the add and edit
// sync dialogs: which source column joins rows to people, and how the source
// marks deleted rows.
function SyncSemanticsFields({
  columnNames,
  joinKey,
  onJoinKey,
  deletionMode,
  onDeletionMode,
  softDeleteColumn,
  onSoftDeleteColumn,
  softDeleteSemantics,
  onSoftDeleteSemantics,
}: {
  columnNames: string[];
  joinKey: string;
  onJoinKey: (v: string) => void;
  deletionMode: string;
  onDeletionMode: (v: string) => void;
  softDeleteColumn: string;
  onSoftDeleteColumn: (v: string) => void;
  softDeleteSemantics: string;
  onSoftDeleteSemantics: (v: string) => void;
}) {
  return (
    <>
      <Selector
        label="Join key (person identity column; empty = no join)"
        size="sm"
        options={['', ...columnNames]}
        value={joinKey}
        onChange={onJoinKey}
      />
      <Selector label="Deletions" size="sm" options={DELETION_MODE_OPTIONS} value={deletionMode} onChange={onDeletionMode} />
      {deletionMode === 'soft_column' ? (
        <>
          <Selector
            label="Soft-delete column"
            size="sm"
            options={columnNames}
            value={softDeleteColumn}
            onChange={onSoftDeleteColumn}
            placeholder="Pick a column…"
          />
          <Selector
            label="A row counts as deleted when"
            size="sm"
            options={SOFT_DELETE_SEMANTICS_OPTIONS}
            value={softDeleteSemantics || 'bool_true'}
            onChange={onSoftDeleteSemantics}
          />
        </>
      ) : null}
    </>
  );
}

function AddSyncDialog({ connectorID, onSubmit, onClose }: {
  connectorID: string;
  onSubmit: (input: ConnectorSyncInput) => void;
  onClose: () => void;
}) {
  const { tables, loading, error } = useConnectorSchema(connectorID, true);
  const [tableName, setTableName] = useState('');
  const [keyColumn, setKeyColumn] = useState('');
  const [cursorColumn, setCursorColumn] = useState('');
  const [cron, setCron] = useState('0 * * * *');
  const [joinKey, setJoinKey] = useState('');
  const [deletionMode, setDeletionMode] = useState('none');
  const [softDeleteColumn, setSoftDeleteColumn] = useState('');
  const [softDeleteSemantics, setSoftDeleteSemantics] = useState('bool_true');

  const table: ConnectorTable | undefined = tables.find((t) => t.name === tableName);
  const columnNames = table?.columns.map((c) => c.name) ?? [];

  function pickTable(name: string) {
    setTableName(name);
    const t = tables.find((x) => x.name === name);
    setKeyColumn(t?.columns.find((c) => c.is_primary_key)?.name ?? t?.columns[0]?.name ?? '');
    setCursorColumn('');
    setJoinKey('');
    setSoftDeleteColumn('');
  }

  function submit() {
    if (!tableName || !keyColumn) return;
    if (deletionMode === 'soft_column' && !softDeleteColumn) return;
    onSubmit({
      source_table: tableName,
      key_column: keyColumn,
      cursor_column: cursorColumn,
      schedule_cron: cron.trim(),
      enabled: true,
      join_key: joinKey,
      deletion_mode: deletionMode,
      soft_delete_column: deletionMode === 'soft_column' ? softDeleteColumn : '',
      soft_delete_semantics: deletionMode === 'soft_column' ? softDeleteSemantics : '',
    });
    onClose();
  }

  return (
    <Modal
      title="Add table sync"
      onClose={onClose}
      footer={<><Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button><Button variant="primary" size="sm" onClick={submit}>Add sync</Button></>}
    >
      {loading ? (
        <Loading label="Discovering schema…" />
      ) : error ? (
        <Text type="supporting" style={{ color: 'var(--danger)' }}>Schema discovery failed: {error}</Text>
      ) : (
        <div className="flex flex-col gap-4 max-w-[440px]">
          <Selector label="Source table" size="sm" options={tables.map((t) => t.name)} value={tableName} onChange={pickTable} placeholder="Pick a table…" />
          {tableName ? (
            <>
              <Selector label="Key column (row identity)" size="sm" options={columnNames} value={keyColumn} onChange={setKeyColumn} />
              <Selector
                label="Cursor column (incremental; empty = full re-sync)"
                size="sm"
                options={['', ...columnNames]}
                value={cursorColumn}
                onChange={setCursorColumn}
              />
              <TextInput label="Schedule (5-field cron, empty = manual only)" value={cron} placeholder="0 * * * *" onChange={setCron} onEnter={submit} width="100%" />
              <SyncSemanticsFields
                columnNames={columnNames}
                joinKey={joinKey}
                onJoinKey={setJoinKey}
                deletionMode={deletionMode}
                onDeletionMode={setDeletionMode}
                softDeleteColumn={softDeleteColumn}
                onSoftDeleteColumn={setSoftDeleteColumn}
                softDeleteSemantics={softDeleteSemantics}
                onSoftDeleteSemantics={setSoftDeleteSemantics}
              />
            </>
          ) : null}
        </div>
      )}
    </Modal>
  );
}

function DraftReviewDialog({ draft, onApprove, onClose }: {
  draft: ConnectorSyncDraft;
  onApprove: (input: ConnectorSyncInput) => Promise<unknown>;
  onClose: () => void;
}) {
  const [approved, setApproved] = useState<Set<number>>(new Set());
  const [saving, setSaving] = useState<number | null>(null);

  return (
    <Modal title="AI-drafted syncs — review before saving" onClose={onClose} wide footer={<Button variant="ghost" size="sm" onClick={onClose}>Done</Button>}>
      <div className="flex flex-col gap-2">
        {draft.warnings?.length ? (
          <Text type="supporting" style={{ color: 'var(--danger)' }}>{draft.warnings.join(' · ')}</Text>
        ) : null}
        {draft.syncs.map((s, i) => (
          <div key={`${s.source_table}-${i}`} className="flex items-center gap-3 rounded-md bg-[var(--color-background-muted)] px-3 py-2 text-sm">
            <div className="min-w-0 flex-1">
              <div className="font-mono">{s.source_table} <span className="text-[var(--color-text-secondary)]">key {s.key_column} · cursor {s.cursor_column || 'full re-sync'} · {s.schedule_cron || 'manual'}</span></div>
              {s.reason ? <div className="text-[var(--color-text-secondary)]">{s.reason}</div> : null}
            </div>
            <Button
              variant={approved.has(i) ? 'ghost' : 'primary'}
              size="sm"
              onClick={() => {
                if (approved.has(i) || saving === i) return;
                setSaving(i);
                // The mutation hook surfaces failures via setError; here a
                // failed row just stays approvable instead of reading "Added".
                onApprove({
                  source_table: s.source_table,
                  key_column: s.key_column,
                  cursor_column: s.cursor_column,
                  schedule_cron: s.schedule_cron,
                  enabled: true,
                  join_key: s.join_key ?? '',
                  deletion_mode: s.deletion_mode || 'none',
                  soft_delete_column: s.soft_delete_column ?? '',
                  soft_delete_semantics: s.soft_delete_semantics ?? '',
                })
                  .then(() => setApproved((prev) => new Set(prev).add(i)))
                  .catch(() => undefined)
                  .finally(() => setSaving((cur) => (cur === i ? null : cur)));
              }}
            >
              {approved.has(i) ? 'Added' : saving === i ? 'Adding…' : 'Add'}
            </Button>
          </div>
        ))}
      </div>
    </Modal>
  );
}

function EditSyncDialog({ connectorID, sync, onSubmit, onClose }: {
  connectorID: string;
  sync: ConnectorSync;
  onSubmit: (input: ConnectorSyncInput) => void;
  onClose: () => void;
}) {
  const { tables, loading, error } = useConnectorSchema(connectorID, true);
  const [keyColumn, setKeyColumn] = useState(sync.key_column);
  const [cursorColumn, setCursorColumn] = useState(sync.cursor_column);
  const [cron, setCron] = useState(sync.schedule_cron);
  const [enabled, setEnabled] = useState(sync.enabled);
  const [joinKey, setJoinKey] = useState(sync.join_key);
  const [deletionMode, setDeletionMode] = useState(sync.deletion_mode || 'none');
  const [softDeleteColumn, setSoftDeleteColumn] = useState(sync.soft_delete_column);
  const [softDeleteSemantics, setSoftDeleteSemantics] = useState(sync.soft_delete_semantics || 'bool_true');

  const table: ConnectorTable | undefined = tables.find((t) => t.name === sync.source_table);
  // The sync's own columns stay selectable even while the schema is still
  // loading or discovery failed — an edit must never drop a configured value.
  const columnNames = useMemo(() => {
    const names = new Set(table?.columns.map((c) => c.name) ?? []);
    for (const c of [sync.key_column, sync.cursor_column, sync.join_key, sync.soft_delete_column]) {
      if (c) names.add(c);
    }
    return [...names];
  }, [table, sync]);

  function submit() {
    if (!keyColumn) return;
    if (deletionMode === 'soft_column' && !softDeleteColumn) return;
    onSubmit(syncInputOf(sync, {
      key_column: keyColumn,
      cursor_column: cursorColumn,
      schedule_cron: cron.trim(),
      enabled,
      join_key: joinKey,
      deletion_mode: deletionMode,
      soft_delete_column: deletionMode === 'soft_column' ? softDeleteColumn : '',
      soft_delete_semantics: deletionMode === 'soft_column' ? softDeleteSemantics : '',
    }));
  }

  return (
    <Modal
      title={`Edit sync — ${sync.source_table}`}
      onClose={onClose}
      footer={<><Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button><Button variant="primary" size="sm" onClick={submit}>Save changes</Button></>}
    >
      <div className="flex flex-col gap-4 max-w-[440px]">
        <Text type="supporting">
          Source table <span className="font-mono">{sync.source_table}</span>. Changing the key or cursor column resets the resume position.
        </Text>
        {error ? (
          <Text type="supporting" style={{ color: 'var(--danger)' }}>Schema discovery failed: {error} — editing against the configured columns.</Text>
        ) : null}
        <Selector label="Key column (row identity)" size="sm" options={columnNames} value={keyColumn} onChange={setKeyColumn} />
        <Selector
          label="Cursor column (incremental; empty = full re-sync)"
          size="sm"
          options={['', ...columnNames]}
          value={cursorColumn}
          onChange={setCursorColumn}
        />
        <TextInput label="Schedule (5-field cron, empty = manual only)" value={cron} placeholder="0 * * * *" onChange={setCron} onEnter={submit} width="100%" />
        <Selector
          label="Enabled"
          size="sm"
          options={[{ value: 'on', label: 'On' }, { value: 'off', label: 'Off' }]}
          value={enabled ? 'on' : 'off'}
          onChange={(v) => setEnabled(v === 'on')}
        />
        <SyncSemanticsFields
          columnNames={columnNames}
          joinKey={joinKey}
          onJoinKey={setJoinKey}
          deletionMode={deletionMode}
          onDeletionMode={setDeletionMode}
          softDeleteColumn={softDeleteColumn}
          onSoftDeleteColumn={setSoftDeleteColumn}
          softDeleteSemantics={softDeleteSemantics}
          onSoftDeleteSemantics={setSoftDeleteSemantics}
        />
        {loading ? <Loading label="Discovering schema…" /> : null}
      </div>
    </Modal>
  );
}

// previewColumns derives the table's columns from the union of keys across the
// preview rows — landed rows are schemaless JSON, so the table shows what is
// actually there rather than the source's declared columns.
function previewColumns(rows: { data: string }[]): string[] {
  const seen = new Set<string>();
  for (const r of rows) {
    try {
      const obj = JSON.parse(r.data) as Record<string, unknown>;
      for (const k of Object.keys(obj)) {
        if (seen.size < 8) seen.add(k);
      }
    } catch {
      // A row that is not an object still renders its raw JSON in the data cell.
    }
  }
  return [...seen];
}

function DatasetPreviewDialog({ sync, onClose }: { sync: ConnectorSync; onClose: () => void }) {
  const { preview, loading, error } = useDatasetPreview(sync.id);
  const rows = useMemo(() => preview?.rows ?? [], [preview]);
  const cols = useMemo(() => previewColumns(rows), [rows]);

  const previewData = useMemo(
    () =>
      rows.map((r) => {
        let obj: Record<string, unknown> = {};
        try {
          const parsed = JSON.parse(r.data);
          if (parsed && typeof parsed === 'object') obj = parsed as Record<string, unknown>;
        } catch {
          obj = { data: r.data };
        }
        return { row_key: r.row_key, cursor: r.cursor, synced_at: r.synced_at, ...obj } as Record<string, unknown>;
      }),
    [rows],
  );

  const tableColumns = useMemo<DataColumn<Record<string, unknown>>[]>(() => [
    { key: 'row_key', header: 'Row', width: { type: 'proportional', value: 1, minWidth: 90 }, renderCell: (r) => <span className="font-mono text-[var(--color-text-secondary)]">{String(r.row_key ?? '')}</span> },
    ...cols.map((c) => ({
      key: c,
      header: c,
      sortable: false,
      width: { type: 'proportional' as const, value: 1, minWidth: 90 },
      renderCell: (r: Record<string, unknown>) => {
        const v = r[c];
        const text = v === null || v === undefined ? '' : typeof v === 'object' ? JSON.stringify(v) : String(v);
        return <span className="font-mono text-[var(--color-text-secondary)]">{text}</span>;
      },
    })),
    { key: 'synced_at', header: 'Synced', width: { type: 'proportional', value: 1, minWidth: 80 }, renderCell: (r) => <span className="text-[var(--color-text-secondary)]">{formatRelative(String(r.synced_at ?? ''))}</span> },
  ], [cols]);

  const s = preview?.sync ?? sync;

  return (
    <Modal title={`Dataset — ${sync.source_table}`} onClose={onClose} wide>
      <VStack gap={4} align="stretch">
        <VStack gap={1} align="stretch">
          <HStack gap={2} align="center" className="flex-wrap">
            {s.join_key ? (
              <>
                <Badge variant="neutral" label={<code>join {s.join_key}</code>} />
                {s.join_validated === 'validated' ? (
                  <Badge variant="success" label="validated" />
                ) : s.join_validated === 'unvalidated' ? (
                  <Badge variant="warning" label="unvalidated" />
                ) : null}
              </>
            ) : (
              <Text type="supporting">No join key — rows are not linked to people.</Text>
            )}
            <Badge
              variant="neutral"
              label={s.deletion_mode === 'soft_column' ? `deletions: ${s.soft_delete_column}` : 'deletions: not tracked'}
            />
          </HStack>
          <Text type="supporting">
            Last success {s.last_success_at ? formatRelative(s.last_success_at) : 'never'} · last attempt{' '}
            {s.last_run_at ? formatRelative(s.last_run_at) : 'never'} · landed watermark{' '}
            <span className="font-mono">{preview?.landed_watermark || '—'}</span> · resume cursor{' '}
            <span className="font-mono">{s.cursor || '—'}</span>
            {s.cursor_key ? <> (<span className="font-mono">{s.cursor_key}</span>)</> : null}
          </Text>
          {preview?.warnings?.length ? (
            <VStack gap={0.5} align="stretch">
              {preview.warnings.map((w) => (
                <Text key={w} type="supporting" style={{ color: 'var(--warning)' }}>
                  {w}
                </Text>
              ))}
            </VStack>
          ) : null}
          {s.last_status === 'error' && s.last_rows > 0 ? (
            <Text type="supporting" style={{ color: 'var(--warning)' }}>
              partial — {formatCompact(s.last_rows)} rows landed before the last run failed{s.last_error ? `: ${s.last_error}` : ''}
            </Text>
          ) : s.last_status === 'error' && s.last_error ? (
            <Text type="supporting" style={{ color: 'var(--danger)' }}>Last error: {s.last_error}</Text>
          ) : null}
        </VStack>

        {loading ? (
          <Loading label="Reading landed rows…" />
        ) : error ? (
          <Text type="supporting" style={{ color: 'var(--danger)' }}>
            Preview unavailable: {error}
          </Text>
        ) : rows.length === 0 ? (
          <EmptyState
            title="No rows landed yet"
            detail="Run the sync to pull rows in. The preview reads the deduped dataset with the soft-delete filter applied — what agents and SQL see."
          />
        ) : (
          <>
            <Text type="supporting">
              {formatCompact(preview?.total_rows ?? 0)} rows in the deduped dataset
              {s.deletion_mode === 'soft_column' ? ' (soft-deleted rows excluded)' : ''} — showing the {rows.length} most recent.
            </Text>
            <DataTable columns={tableColumns} data={previewData} pageSize={10} />
          </>
        )}
      </VStack>
    </Modal>
  );
}
