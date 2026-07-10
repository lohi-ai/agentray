'use client';

import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, FlaskConical, Settings2 } from 'lucide-react';
import { Table } from '@astryxdesign/core/Table';
import type { AgentRun } from '@/lib/api';
import { formatCompact, formatCost, formatLatency, formatRelative } from '@/lib/format';
import { useAgentMonitorDetail } from '@/modules/agent-monitor/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Intro, Loading, Panel, StatsStrip, StatusPill } from '@/modules/shared/components/signal-primitives';

function runLatency(run: AgentRun): string {
  if (!run.finished_at) return '—';
  return formatLatency(new Date(run.finished_at).getTime() - new Date(run.started_at).getTime());
}

export function AgentMonitorPage() {
  const params = useParams<{ agentId: string }>();
  const router = useRouter();
  const agentID = params.agentId;
  const { agent, runs, isLoading } = useAgentMonitorDetail(agentID);

  if (isLoading && !agent) {
    return <AppShell active="monitor"><Intro title="Agent" sub="Per-agent health and recent runs." /><Loading label="Loading agent…" /></AppShell>;
  }
  if (!agent) {
    return <AppShell active="monitor"><Intro title="Agent" sub="Per-agent health and recent runs." /><EmptyState title="Agent not found" detail="This agent may have been removed." action={<Button variant="outline" size="sm" onClick={() => router.push('/agents/monitor')}>Back to fleet</Button>} /></AppShell>;
  }

  const status = !agent.enabled ? { s: 'paused', l: 'Paused' } : agent.error_count > 0 ? { s: 'attention', l: 'Attention' } : agent.running_count > 0 ? { s: 'working', l: 'Working' } : { s: 'healthy', l: 'Healthy' };

  return (
    <AppShell active="monitor">
      <Intro
        title={<span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}><button className="flex-none grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]" onClick={() => router.push('/agents/monitor')}><ArrowLeft size={15} /></button>{agent.name}</span>}
        sub="Per-agent health and recent runs."
        action={<><StatusPill status={status.s} label={status.l} grow={false} /><Button variant="outline" icon={<Settings2 size={15} />} onClick={() => router.push(`/agents/${agentID}/setup`)}>Set up</Button><Button variant="agent" icon={<FlaskConical size={15} />} onClick={() => router.push(`/agents/${agentID}/lab`)}>Open lab</Button></>}
      />
      <StatsStrip stats={[
        { label: 'Runs (24h)', value: formatCompact(agent.run_count) },
        { label: 'Running now', value: String(agent.running_count), tone: agent.running_count ? 'agent' : undefined },
        { label: 'Failures', value: String(agent.error_count), tone: agent.error_count ? 'danger' : undefined },
        { label: 'Tokens', value: formatCompact(agent.token_input + agent.token_output) },
        { label: 'Spend', value: formatCost(agent.cost_usd) },
        { label: 'Last run', value: agent.last_run_at ? formatRelative(agent.last_run_at) : '—' },
      ]} />
      <Panel title="Recent runs">
        {runs.length === 0 ? <p style={{ color: 'var(--muted-foreground)', fontSize: 12.5, margin: 0 }}>No runs recorded yet.</p> : (
          /* Astryx migration: recent runs render through the data-driven Astryx
             Table (compact density, themed cells); failed-status danger tint and
             end-aligned monospace numerics preserved via renderCell. */
          <Table
            data={runs}
            idKey="id"
            density="compact"
            columns={[
              { key: 'run', header: 'Run', align: 'start', renderCell: (run) => run.summary || run.id.slice(0, 12) },
              { key: 'trigger', header: 'Trigger', align: 'start', renderCell: (run) => <span className="text-[var(--color-text-disabled)]">{run.trigger}</span> },
              { key: 'status', header: 'Status', align: 'start', renderCell: (run) => <span style={run.status === 'failed' ? { color: 'var(--danger)' } : undefined}>{run.status}</span> },
              { key: 'latency', header: 'Latency', align: 'end', renderCell: (run) => <span className="font-mono tabular-nums">{runLatency(run)}</span> },
              { key: 'tokens', header: 'Tokens', align: 'end', renderCell: (run) => <span className="font-mono tabular-nums">{formatCompact(run.token_input + run.token_output)}</span> },
              { key: 'cost', header: 'Cost', align: 'end', renderCell: (run) => <span className="font-mono tabular-nums">{formatCost(run.cost_usd)}</span> },
              { key: 'when', header: 'When', align: 'end', renderCell: (run) => <span className="font-mono tabular-nums text-[var(--color-text-disabled)]">{formatRelative(run.finished_at || run.started_at)}</span> },
            ]}
          />
        )}
      </Panel>
    </AppShell>
  );
}
