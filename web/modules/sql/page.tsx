'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { Clipboard, Columns3, Download, Pencil, Play, Save, Sparkles, Trash2, Wand2 } from 'lucide-react';
import { Banner } from '@astryxdesign/core/Banner';
import { Table } from '@astryxdesign/core/Table';
import { HStack } from '@astryxdesign/core/HStack';
import { TextInput } from '@astryxdesign/core/TextInput';
import type { SavedQuery } from '@/lib/api';
import { useSavedQueries, useSQL } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { EventCatalog } from '@/modules/shared/components/event-name-picker';
import { Button, EmptyState, Intro, Panel } from '@/modules/shared/components/signal-primitives';
import { SqlEditor } from './sql-editor';
import { SchemaReference } from './schema-reference';

const SAMPLE = 'SELECT event_name, count() AS c FROM events GROUP BY event_name ORDER BY c DESC LIMIT 20';
const MAX_DISPLAY_ROWS = 100;

// openChat sends the user to the agent chat with a seeded question. The agent
// (Data Analyst / Growth Analyst) owns the SQL writing, running, and charting via
// its generic tools — this page just hands off the context, no special endpoint.
function chatHref(prompt: string): string {
  return `/chat?q=${encodeURIComponent(prompt)}`;
}

// CSV serialization for copy + download. Quote any cell containing a comma,
// quote, or newline (RFC 4180), doubling embedded quotes.
function toCSV(rows: Array<Record<string, unknown>>): string {
  if (rows.length === 0) return '';
  const cols = Object.keys(rows[0]);
  const esc = (v: unknown) => {
    const s = v == null ? '' : String(v);
    return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  return [cols.join(','), ...rows.map((r) => cols.map((c) => esc(r[c])).join(','))].join('\n');
}

// Astryx migration: dynamic-column SQL results now render through the data-driven
// Astryx <Table> (compact density, themed cells). Numeric columns derive end
// alignment + monospace from the first row's value type, as before.
function RowsTable({ rows }: { rows: Array<Record<string, unknown>> }) {
  if (rows.length === 0) return <EmptyState title="No rows" detail="Run a query to see results." />;
  const cols = Object.keys(rows[0]);
  const columns = cols.map((c) => {
    const numeric = typeof rows[0][c] === 'number';
    return {
      key: c,
      header: c,
      align: (numeric ? 'end' : 'start') as 'end' | 'start',
      renderCell: (row: Record<string, unknown>) => (
        <span className={numeric ? 'font-mono tabular-nums' : undefined}>{String(row[c] ?? '')}</span>
      ),
    };
  });
  return <Table data={rows.slice(0, MAX_DISPLAY_ROWS)} columns={columns} density="compact" />;
}

export function SQLPage() {
  const router = useRouter();
  const { sqlRows, run, running, error, elapsedMs, clearError } = useSQL();
  const { savedQueries, savedResult, createSavedQuery, runSavedQuery, renameSavedQuery, deleteSavedQuery, busy } = useSavedQueries();
  const [sql, setSql] = useState(SAMPLE);
  const [ask, setAsk] = useState('');
  const [showReference, setShowReference] = useState(false);
  const [copied, setCopied] = useState(false);

  // CodeMirror owns its own caret, so clicks from the reference rail append to the
  // editor buffer (with a separating space) rather than splicing at a textarea caret.
  function insertText(text: string) {
    setSql((s) => (s === '' || /\s$/.test(s) ? s + text : `${s} ${text}`));
  }

  function runQuery() {
    if (!sql.trim()) return;
    run(sql);
  }

  // Hand the plain-language question to the agent chat, carrying the current query
  // as context so the agent can refine it rather than start from scratch.
  function askAI() {
    if (!ask.trim()) return;
    const context = sql.trim() && sql.trim() !== SAMPLE ? `\n\nMy current query (refine if relevant):\n${sql.trim()}` : '';
    router.push(chatHref(`${ask.trim()}${context}`));
  }

  function explain() {
    if (!sql.trim()) return;
    router.push(chatHref(`Explain this SQL query in plain language:\n\n${sql.trim()}`));
  }

  // Saved-query management. Load drops the SQL back into the editor; rename/delete
  // hit the new PATCH/DELETE routes via the hook (window prompts keep it lightweight).
  function loadSaved(q: SavedQuery) {
    setSql(q.generated_sql);
    clearError();
  }
  function renameSaved(q: SavedQuery) {
    const next = window.prompt('Rename saved query', q.natural_language || '');
    if (next != null && next.trim() && next.trim() !== q.natural_language) void renameSavedQuery(q.id, next.trim());
  }
  function deleteSaved(q: SavedQuery) {
    if (window.confirm(`Delete saved query "${q.natural_language || q.generated_sql.slice(0, 40)}"?`)) void deleteSavedQuery(q.id);
  }

  const rows = savedResult?.rows ?? sqlRows;
  const hasRows = rows.length > 0;

  async function copyResults() {
    await navigator.clipboard.writeText(toCSV(rows));
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  }
  function downloadCSV() {
    const blob = new Blob([toCSV(rows)], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `agentray-query-${Date.now()}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  }

  // Results meta line: "N rows · 0.18s" (elapsed only for editor runs, not cached
  // saved-query results) plus a truncation note when we cap the table at 100 rows.
  const resultsMeta = (
    <span className="text-[11.5px] text-[var(--color-text-secondary)]">
      {hasRows ? (
        <>
          {rows.length.toLocaleString('en-US')} {rows.length === 1 ? 'row' : 'rows'}
          {!savedResult && elapsedMs != null ? <> · {(elapsedMs / 1000).toFixed(2)}s</> : null}
          {rows.length > MAX_DISPLAY_ROWS ? <> · showing first {MAX_DISPLAY_ROWS}</> : null}
        </>
      ) : null}
    </span>
  );

  const resultsActions = (
    <HStack align="center" gap={2}>
      {resultsMeta}
      {hasRows ? (
        <>
          <Button variant="ghost" size="sm" icon={<Clipboard size={14} />} onClick={() => void copyResults()}>{copied ? 'Copied' : 'Copy CSV'}</Button>
          <Button variant="ghost" size="sm" icon={<Download size={14} />} onClick={downloadCSV}>Export</Button>
        </>
      ) : null}
    </HStack>
  );

  return (
    <AppShell active="dashboards">
      <Intro title="SQL" sub="Query the event store directly — or describe what you want and let the agent write it." action={<><Button variant="outline" icon={<Columns3 size={15} />} onClick={() => setShowReference((v) => !v)}>{showReference ? 'Hide reference' : 'Schema & names'}</Button><Button variant="outline" icon={<Save size={15} />} onClick={() => sql.trim() && void createSavedQuery(sql.slice(0, 60), sql, true)}>Save</Button><Button variant="primary" icon={<Play size={15} />} onClick={runQuery}>Run</Button></>} />

      {/* Hand-off to the agent chat — describe the question, the agent writes & runs the SQL. */}
      <HStack align="center" gap={2} className="mb-3 rounded-xl bg-[color-mix(in_srgb,var(--agent)_8%,var(--surface-1))] p-2.5">
        <TextInput
          label="Ask the agent"
          isLabelHidden
          size="sm"
          startIcon={Sparkles}
          className="min-w-0 flex-1"
          width="100%"
          placeholder="Ask the agent in plain language — e.g. “How many signups per day in the last 2 weeks?”"
          value={ask}
          onChange={(v) => setAsk(v)}
          onEnter={askAI}
        />
        <Button variant="agent" size="sm" icon={<Wand2 size={15} />} disabled={!ask.trim()} onClick={askAI}>Ask the agent</Button>
      </HStack>

      <div className={showReference ? 'mb-4 grid grid-cols-[minmax(0,1fr)_280px] gap-3 max-[900px]:grid-cols-1' : 'mb-4'}>
        <div className="rounded-xl bg-[var(--color-background-card)] p-3">
          <div className="overflow-hidden rounded-md border border-[color-mix(in_srgb,var(--border)_60%,transparent)]">
            <SqlEditor value={sql} onChange={setSql} onRun={runQuery} />
          </div>
          <div className="mt-2 flex items-center justify-between border-t border-[color-mix(in_srgb,var(--border)_60%,transparent)] pt-2">
            <span className="text-[11px] text-[var(--color-text-secondary)]">⌘↵ to run · Tab to indent</span>
            <Button variant="ghost" size="sm" icon={<Sparkles size={14} />} disabled={!sql.trim()} onClick={explain}>Explain this query</Button>
          </div>
        </div>
        {showReference ? (
          <div className="flex flex-col gap-3">
            <SchemaReference onPick={insertText} />
            <EventCatalog onPick={(name) => insertText(`'${name}'`)} title="Event names" />
          </div>
        ) : null}
      </div>

      {error ? (
        <Banner className="mb-4" status="error" title="Query failed" description={error} isDismissable onDismiss={clearError} />
      ) : null}

      <div className="flex flex-col gap-[14px]">
        <Panel title={running ? 'Running…' : 'Results'} action={resultsActions}><RowsTable rows={rows} /></Panel>
        {savedQueries.length ? (
          <Panel title="Saved queries">
            <Table
              data={savedQueries as unknown as Array<Record<string, unknown>>}
              idKey="id"
              density="compact"
              columns={[
                { key: 'query', header: 'Query', align: 'start', renderCell: (q) => <span className="truncate">{(q.natural_language as string) || (q.generated_sql as string).slice(0, 48)}</span> },
                {
                  key: 'actions',
                  header: '',
                  align: 'end',
                  renderCell: (q) => {
                    const sq = q as unknown as SavedQuery;
                    return (
                      <HStack align="center" gap={1} justify="end">
                        <Button variant="ghost" size="sm" onClick={() => loadSaved(sq)}>Load</Button>
                        <Button variant="ghost" size="sm" icon={<Play size={13} />} onClick={() => void runSavedQuery(sq.id)}>Run</Button>
                        <Button variant="ghost" size="sm" icon={<Pencil size={13} />} disabled={busy} onClick={() => renameSaved(sq)}>Rename</Button>
                        <Button variant="ghost" size="sm" icon={<Trash2 size={13} />} disabled={busy} onClick={() => deleteSaved(sq)}>Delete</Button>
                      </HStack>
                    );
                  },
                },
              ]}
            />
          </Panel>
        ) : null}
      </div>
    </AppShell>
  );
}
