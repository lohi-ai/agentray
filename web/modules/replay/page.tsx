'use client';

import { useEffect, useState } from 'react';
import { Play } from 'lucide-react';
import { Table } from '@astryxdesign/core/Table';
import { TextInput } from '@astryxdesign/core/TextInput';
import { formatCompact, formatCost, formatLatency, formatRelative } from '@/lib/format';
import { useReplay } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Intro, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';

export function ReplayPage() {
  const { replay, loadReplay } = useReplay();
  const [sessionID, setSessionID] = useState('');
  const [loading, setLoading] = useState(false);

  async function load(id: string = sessionID) {
    if (!id.trim()) return;
    setLoading(true);
    try { await loadReplay(id.trim()); } finally { setLoading(false); }
  }

  // Deep-link: opening /replay?session=… (e.g. from an Events row) prefills the
  // box and loads the session immediately. Read from the URL on mount only, so
  // the user can keep typing without a param fight. The one-time setState here
  // is intentional (syncing from the URL), so the cascading-render rule is off.
  useEffect(() => {
    const linked = new URLSearchParams(window.location.search).get('session');
    if (linked) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setSessionID(linked);
      void load(linked);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const input = (
    <TextInput
      label="Session ID"
      isLabelHidden
      size="sm"
      value={sessionID}
      placeholder="Session ID…"
      onChange={(v) => setSessionID(v)}
      onEnter={() => void load()}
      width={220}
    />
  );

  return (
    <AppShell active="monitor">
      <Intro title="Session replay" sub="Step through every event in a session to see what the agent saw." action={<>{input}<Button variant="primary" icon={<Play size={15} />} onClick={() => void load()}>Replay</Button></>} />
      {loading ? <Panel title="Loading…"><span /></Panel> : !replay ? (
        <EmptyState title="No session loaded" detail="Paste a session ID, or open a session from People or Events." />
      ) : (
        <>
          <StatsStrip stats={[
            { label: 'Events', value: formatCompact(replay.event_count) },
            { label: 'Tokens in', value: formatCompact(replay.total_tokens_in) },
            { label: 'Tokens out', value: formatCompact(replay.total_tokens_out) },
            { label: 'Cost', value: formatCost(replay.total_cost_usd) },
          ]} />
          <Panel title={`Timeline · ${replay.session_id.slice(0, 16)}`}>
            {/* Astryx migration: the timeline now renders through the data-driven
                Astryx <Table> (compact density, themed cells). Error rows keep the
                prototype's danger tint via a renderCell wrapper; numeric columns are
                end-aligned + monospace. */}
            <Table
              data={replay.events}
              idKey="event_id"
              density="compact"
              columns={[
                {
                  key: 'event',
                  header: 'Event',
                  align: 'start',
                  renderCell: (e) => (
                    <span style={e.is_error ? { color: 'var(--danger)' } : undefined}>
                      {e.event_name}{e.tool_name ? <span className="text-[var(--color-text-disabled)]"> · {e.tool_name}</span> : null}
                    </span>
                  ),
                },
                { key: 'type', header: 'Type', align: 'start', renderCell: (e) => <span className="text-[var(--color-text-disabled)]">{e.event_type}</span> },
                { key: 'tokens', header: 'Tokens', align: 'end', renderCell: (e) => <span className="font-mono tabular-nums">{formatCompact((e.tokens_input ?? 0) + (e.tokens_output ?? 0))}</span> },
                { key: 'latency', header: 'Latency', align: 'end', renderCell: (e) => <span className="font-mono tabular-nums">{formatLatency(e.latency_ms ?? 0)}</span> },
                { key: 'when', header: 'When', align: 'end', renderCell: (e) => <span className="font-mono tabular-nums text-[var(--color-text-disabled)]">{formatRelative(e.timestamp)}</span> },
              ]}
            />
          </Panel>
        </>
      )}
    </AppShell>
  );
}
