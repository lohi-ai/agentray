'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { MessagesSquare, Play, Plus, Settings2, Share2, Wrench } from 'lucide-react';
import type { AgentMonitorRow } from '@/lib/api';
import { formatCompact, formatCost, formatRelative } from '@/lib/format';
import { useAgents } from '@/modules/agent/hooks';
import { useAgentMonitor } from '@/modules/agent-monitor/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { PromptDialog } from '@/modules/shared/components/modal';
import { Button, ContextChips, EmptyState, Intro, StatsStrip, StatusPill } from '@/modules/shared/components/signal-primitives';
import { AssignProductsDialog } from './assign-products-dialog';

// agentStatus derives the prototype's four health states from the run rollup.
function agentStatus(row: AgentMonitorRow): { status: string; label: string } {
  if (!row.enabled) return { status: 'paused', label: 'Paused' };
  if (row.error_count > 0) return { status: 'attention', label: 'Needs attention' };
  if (row.running_count > 0) return { status: 'working', label: 'Working' };
  return { status: 'healthy', label: 'Healthy' };
}

export function AgentsPage() {
  const router = useRouter();
  const { agents, isLoading } = useAgentMonitor();
  const { createAgent, updateAgent } = useAgents();
  const [creating, setCreating] = useState(false);
  const [assigning, setAssigning] = useState<{ id: string; name: string } | null>(null);

  const active = agents.filter((a) => a.enabled).length;
  const working = agents.filter((a) => a.enabled && a.running_count > 0).length;
  const attention = agents.filter((a) => a.enabled && a.error_count > 0).length;
  const runs = agents.reduce((sum, a) => sum + a.run_count, 0);
  const tokens = agents.reduce((sum, a) => sum + a.token_input + a.token_output, 0);
  const spend = agents.reduce((sum, a) => sum + a.cost_usd, 0);

  const onCreate = () => setCreating(true);

  return (
    <AppShell active="agents">
      {creating ? (
        <PromptDialog
          title="New agent"
          label="Agent name"
          placeholder="e.g. Growth analyst"
          submitLabel="Create agent"
          onSubmit={(name) => void createAgent(name)}
          onClose={() => setCreating(false)}
        />
      ) : null}
      {assigning ? (
        <AssignProductsDialog agentID={assigning.id} agentName={assigning.name} onClose={() => setAssigning(null)} />
      ) : null}
      <Intro title="Your agents" sub="Configure and trust the teammates doing the work." action={<Button variant="primary" icon={<Plus size={15} />} onClick={onCreate}>New agent</Button>} />
      <ContextChips range="Last 24 hours" />
      <StatsStrip
        stats={[
          { label: 'Active', value: String(active) },
          { label: 'Working now', value: String(working), tone: 'agent' },
          { label: 'Needs attention', value: String(attention), tone: attention ? 'warning' : undefined },
          { label: 'Runs (24h)', value: formatCompact(runs) },
          { label: 'Tokens (24h)', value: formatCompact(tokens) },
          { label: 'Spend (24h)', value: formatCost(spend) },
        ]}
      />
      {agents.length === 0 && !isLoading ? (
        <EmptyState icon={<Plus size={22} />} title="No agents yet" detail="Spin up a teammate from a blank recipe. No backend code needed." action={<Button variant="outline" size="sm" onClick={onCreate}>Create agent</Button>} />
      ) : (
        <div className="grid grid-cols-3 gap-3.5 max-[980px]:grid-cols-1">
          {agents.map((row) => {
            const { status, label } = agentStatus(row);
            return (
              <div
                className={`relative flex flex-col gap-[11px] overflow-hidden rounded-xl bg-[var(--color-background-card)] p-[15px] transition-[transform,background,box-shadow] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:-translate-y-0.5 hover:shadow-[0_6px_20px_-12px_rgba(0,0,0,0.7)] hover:[&_.av-agent]:shadow-[0_0_0_4px_color-mix(in_srgb,var(--agent)_12%,transparent)] ${status === 'paused' ? 'opacity-[0.62]' : ''}`}
                key={row.id}
              >
                <div className="flex items-center gap-2.5">
                  <span className="grid h-[30px] w-[30px] place-items-center rounded-[9px] bg-[color-mix(in_srgb,var(--agent)_18%,transparent)] text-[13px] font-bold text-agent">{(row.name || '?').charAt(0).toUpperCase()}</span>
                  <span className="text-[13.5px] font-semibold">{row.name}</span>
                  <StatusPill status={status} label={label} />
                </div>
                <div className="min-h-9 text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">{row.is_default ? 'Default project analyst — routes and answers questions across your data.' : `Autonomy: ${row.autonomy || 'manual'}.`}</div>
                <div className="flex gap-3.5 pt-0.5 text-[11.5px] text-[var(--color-text-secondary)]">
                  <span>last run <b className="font-mono font-medium text-[var(--color-text-primary)] tabular-nums">{formatRelative(row.last_run_at)}</b></span>
                  <span>runs <b className="font-mono font-medium text-[var(--color-text-primary)] tabular-nums">{row.run_count}</b></span>
                  <span>cost <b className="font-mono font-medium text-[var(--color-text-primary)] tabular-nums">{formatCost(row.cost_usd)}</b></span>
                </div>
                <div className="mt-0.5 flex items-center gap-2">
                  {status === 'attention' ? (
                    <Button variant="agent" size="sm" icon={<Wrench size={15} />} onClick={() => router.push(`/agents/${row.id}/monitor`)}>Fix setup</Button>
                  ) : status === 'paused' ? (
                    <Button variant="outline" size="sm" icon={<Play size={15} />} onClick={() => void updateAgent(row.id, row.name, true)}>Resume</Button>
                  ) : (
                    <Button variant="primary" size="sm" icon={<MessagesSquare size={15} />} onClick={() => router.push(`/chat?agent=${row.id}`)}>Talk to agent</Button>
                  )}
                  <Button variant="ghost" size="sm" icon={<Settings2 size={15} />} onClick={() => router.push(`/agents/${row.id}/setup`)}>Set up</Button>
                  <Button variant="ghost" size="sm" icon={<Share2 size={15} />} onClick={() => setAssigning({ id: row.id, name: row.name })}>Assign</Button>
                </div>
              </div>
            );
          })}
          <div className="relative flex flex-col gap-[11px] overflow-hidden rounded-xl border border-dashed border-[color-mix(in_srgb,var(--border)_70%,transparent)] bg-[color-mix(in_srgb,var(--surface-2)_55%,transparent)] p-[15px] transition-[transform,background,box-shadow] duration-[var(--fast)] ease-[var(--ease)] hover:border-[color-mix(in_srgb,var(--agent)_45%,var(--border))] hover:bg-[var(--color-background-muted)]">
            <div className="flex items-center gap-2.5">
              <span className="grid h-[30px] w-[30px] place-items-center rounded-[9px] bg-[var(--color-background-surface)] text-[13px] font-bold text-[var(--color-text-secondary)]"><Plus size={16} /></span>
              <span className="text-[13.5px] font-semibold">New agent</span>
            </div>
            <div className="min-h-9 text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">Spin up a teammate from a template or a blank recipe. No backend code needed.</div>
            <div className="mt-0.5 flex items-center gap-2"><Button variant="outline" size="sm" onClick={onCreate}>Create agent</Button></div>
          </div>
        </div>
      )}
    </AppShell>
  );
}
