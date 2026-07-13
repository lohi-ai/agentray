'use client';

import { useMemo, useState } from 'react';
import { Plus, Sparkles } from 'lucide-react';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { Text } from '@astryxdesign/core/Text';
import {
  AgentRayAPI,
  type ConnectorSync,
  type ConnectorSyncDraft,
  type ConnectorSyncInput,
  type ConnectorTable,
  type DataConnector,
} from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';
import { formatCompact, formatRelative } from '@/lib/format';
import { useConnectors, useConnectorSchema, useConnectorSyncs } from '@/modules/app/hooks/connectors';
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
          onSubmit={(input) => void create.mutateAsync(input).then((r) => setSelectedID(r.connector.id))}
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
            void remove.mutate(deleting.id);
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
  onSubmit: (input: { name: string; kind: string; dsn: string }) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState('');
  const [kind, setKind] = useState(kinds[0] ?? 'postgres');
  const [dsn, setDsn] = useState('');

  function submit() {
    if (!name.trim() || !dsn.trim()) return;
    onSubmit({ name: name.trim(), kind, dsn: dsn.trim() });
    onClose();
  }

  return (
    <Modal
      title="Add data connector"
      onClose={onClose}
      footer={<><Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button><Button variant="primary" size="sm" onClick={submit}>Add connector</Button></>}
    >
      <div className="flex flex-col gap-3.5 max-w-[440px]">
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
  const { syncs, loading, create, update, remove, run } = useConnectorSyncs(connector.id);
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);
  const [adding, setAdding] = useState(false);
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
      key: 'schedule_cron',
      header: 'Schedule',
      width: { type: 'proportional', value: 1, minWidth: 90 },
      renderCell: (s) => <span className="font-mono text-[var(--color-text-secondary)]">{s.schedule_cron || 'manual'}</span>,
    },
    {
      key: 'last_status',
      header: 'Last run',
      width: { type: 'proportional', value: 2, minWidth: 140 },
      renderCell: (s) => {
        if (!s.last_run_at) return <span className="text-[var(--color-text-disabled)]">never</span>;
        if (s.last_status === 'error') {
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
        <Button variant="ghost" size="sm" onClick={() => void update.mutate({ id: s.id, input: { source_table: s.source_table, key_column: s.key_column, cursor_column: s.cursor_column, schedule_cron: s.schedule_cron, enabled: !s.enabled } })}>
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
      width: { type: 'pixel', value: 180 },
      renderCell: (s) => (
        <span className="flex justify-end gap-1">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              setRunning(s.id);
              void run.mutateAsync(s.id).then((r) => {
                if (r && !r.ok && r.error) setError(`Sync failed: ${r.error}`);
              }).finally(() => setRunning(null));
            }}
          >
            {running === s.id ? 'Running…' : 'Run now'}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void remove.mutate(s.id)}>
            <span style={{ color: 'var(--danger)' }}>Delete</span>
          </Button>
        </span>
      ),
    },
    // update/run/remove are react-query mutations (stable identities).
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
          <span className="flex gap-1.5">
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
              <span className="font-mono">SELECT JSONExtractString(data, &apos;email&apos;) FROM external_rows WHERE table_name = &apos;{syncs[0]?.source_table ?? 'users'}&apos;</span>.
            </Text>
          </>
        )}
      </Panel>
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

  const table: ConnectorTable | undefined = tables.find((t) => t.name === tableName);
  const columnNames = table?.columns.map((c) => c.name) ?? [];

  function pickTable(name: string) {
    setTableName(name);
    const t = tables.find((x) => x.name === name);
    setKeyColumn(t?.columns.find((c) => c.is_primary_key)?.name ?? t?.columns[0]?.name ?? '');
    setCursorColumn('');
  }

  function submit() {
    if (!tableName || !keyColumn) return;
    onSubmit({ source_table: tableName, key_column: keyColumn, cursor_column: cursorColumn, schedule_cron: cron.trim(), enabled: true });
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
        <div className="flex flex-col gap-3.5 max-w-[440px]">
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
          <div key={`${s.source_table}-${i}`} className="flex items-center gap-3 rounded-md bg-[var(--color-background-muted)] px-3 py-2 text-[12.5px]">
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
                onApprove({ source_table: s.source_table, key_column: s.key_column, cursor_column: s.cursor_column, schedule_cron: s.schedule_cron, enabled: true })
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
