'use client';

import { useRouter } from 'next/navigation';
import { Radio, TriangleAlert } from 'lucide-react';
import { Table } from '@astryxdesign/core/Table';
import type { AgentMonitorRow } from '@/lib/api';
import { formatCompact, formatCost, formatRelative } from '@/lib/format';
import { useAgentMonitor } from '@/modules/agent-monitor/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, Callout, Intro, Loading, Panel, StatsStrip, StatusPill, rowNavPlugin } from '@/modules/shared/components/signal-primitives';

function statusOf(row: AgentMonitorRow): { status: string; label: string } {
  if (!row.enabled) return { status: 'paused', label: 'Paused' };
  if (row.error_count > 0) return { status: 'attention', label: 'Attention' };
  if (row.running_count > 0) return { status: 'working', label: 'Working' };
  return { status: 'healthy', label: 'Healthy' };
}

// rank orders the fleet so failures and active agents float to the top.
function rank(row: AgentMonitorRow): number {
  if (row.enabled && row.error_count > 0) return 0;
  if (row.enabled && row.running_count > 0) return 1;
  if (row.enabled) return 2;
  return 3;
}

export function AgentsMonitorPage() {
  const router = useRouter();
  const { agents, isLoading } = useAgentMonitor();

  const healthy = agents.filter((a) => a.enabled && a.error_count === 0 && a.running_count === 0).length;
  const working = agents.filter((a) => a.enabled && a.running_count > 0).length;
  const failures = agents.reduce((sum, a) => sum + a.error_count, 0);
  const tokens = agents.reduce((sum, a) => sum + a.token_input + a.token_output, 0);
  const spend = agents.reduce((sum, a) => sum + a.cost_usd, 0);
  const needsReview = agents.find((a) => a.enabled && a.error_count > 0);
  const sorted = [...agents].sort((a, b) => rank(a) - rank(b));

  return (
    <AppShell active="monitor">
      <Intro title="Agent health" sub="Know what's safe, active, or needs review." action={<Button variant="primary" icon={<Radio size={15} />} onClick={() => router.push('/chat')}>Open live monitor</Button>} />
      <StatsStrip
        stats={[
          { label: 'Healthy', value: String(healthy), tone: healthy ? 'success' : undefined },
          { label: 'Working now', value: String(working), tone: 'agent' },
          { label: 'Failures (24h)', value: String(failures), tone: failures ? 'danger' : undefined },
          { label: 'Agents', value: String(agents.length) },
          { label: 'Tokens (24h)', value: formatCompact(tokens) },
          { label: 'Spend (24h)', value: formatCost(spend) },
        ]}
      />
      {needsReview ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} />}
          label="Needs review"
          title={`${needsReview.name} failed ${needsReview.error_count} run${needsReview.error_count > 1 ? 's' : ''}`}
          detail="Open the agent's monitor to inspect the failing runs and their tool traces."
          action={<Button variant="agent" size="sm" onClick={() => router.push(`/agents/${needsReview.id}/monitor`)}>Inspect</Button>}
        />
      ) : null}
      {isLoading && agents.length === 0 ? <Loading label="Loading fleet…" /> : (
        <Panel title="Fleet" action={<span className="text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">failures &amp; active first</span>}>
          {/* Astryx migration: the fleet table now renders through the data-driven
              Astryx <Table> (compact density, themed cells). The whole row stays
              clickable via onRowClick; StatusPill, danger tint, and end-aligned
              monospace numbers are preserved through renderCell. */}
          <Table
            data={sorted}
            idKey="id"
            density="compact"
            hasHover
            plugins={{ nav: rowNavPlugin<AgentMonitorRow>((row) => router.push(`/agents/${row.id}/monitor`)) }}
            columns={[
              { key: 'agent', header: 'Agent', align: 'start', renderCell: (row) => row.name },
              { key: 'status', header: 'Status', align: 'start', renderCell: (row) => { const { status, label } = statusOf(row); return <StatusPill status={status} label={label} grow={false} />; } },
              {
                key: 'work',
                header: 'Current work',
                align: 'start',
                renderCell: (row) => {
                  const { status } = statusOf(row);
                  const work = status === 'attention' ? `${row.error_count} failed` : status === 'working' ? `${row.running_count} running` : status === 'paused' ? '—' : 'idle';
                  return <span className={status === 'attention' ? 'text-danger' : 'text-[var(--color-text-disabled)]'}>{work}{status !== 'paused' && row.last_run_at ? <span className="text-[var(--color-text-disabled)]"> · {formatRelative(row.last_run_at)}</span> : null}</span>;
                },
              },
              { key: 'runs', header: 'Runs', align: 'end', renderCell: (row) => <span className="font-mono tabular-nums">{row.run_count}</span> },
              { key: 'tokens', header: 'Tokens', align: 'end', renderCell: (row) => <span className="font-mono tabular-nums">{formatCompact(row.token_input + row.token_output)}</span> },
              { key: 'cost', header: 'Cost', align: 'end', renderCell: (row) => <span className="font-mono tabular-nums">{formatCost(row.cost_usd)}</span> },
            ]}
          />
        </Panel>
      )}
    </AppShell>
  );
}
