'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { Crown, Plus, Trash2, UsersRound } from 'lucide-react';
import { useTeams } from './hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { ConfirmDialog, PromptDialog } from '@/modules/shared/components/modal';
import { Button, EmptyState, Intro, StatsStrip } from '@/modules/shared/components/signal-primitives';

// TeamsPage lists the project's agent teams. A team groups existing agents
// around a shared kanban board; the picked lead gets the orchestrator skill at
// run time and delegates cards to the other members.
export function TeamsPage() {
  const router = useRouter();
  const { teams, teamsLoading, createTeam, removeTeam } = useTeams();
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<{ id: string; name: string } | null>(null);

  const withLead = teams.filter((t) => t.lead_agent_id).length;
  const members = teams.reduce((sum, t) => sum + t.member_count, 0);
  const cards = teams.reduce((sum, t) => sum + t.card_count, 0);

  const onCreate = () => setCreating(true);

  return (
    <AppShell active="agents">
      {creating ? (
        <PromptDialog
          title="New team"
          label="Team name"
          placeholder="e.g. Marketing"
          submitLabel="Create team"
          onSubmit={(name) => void createTeam(name)}
          onClose={() => setCreating(false)}
        />
      ) : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          detail="Removes the team, its roster, and its board. The agents themselves are untouched."
          confirmLabel="Delete team"
          danger
          onConfirm={() => void removeTeam(deleting.id)}
          onClose={() => setDeleting(null)}
        />
      ) : null}
      <Intro
        title="Agent teams"
        sub="Group agents around a board, pick a lead, and let it orchestrate the work."
        action={<Button variant="primary" icon={<Plus size={15} />} onClick={onCreate}>New team</Button>}
      />
      <StatsStrip
        stats={[
          { label: 'Teams', value: String(teams.length) },
          { label: 'With a lead', value: String(withLead), tone: 'agent' },
          { label: 'Members', value: String(members) },
          { label: 'Cards', value: String(cards) },
        ]}
      />
      {teams.length === 0 && !teamsLoading ? (
        <EmptyState
          icon={<UsersRound size={22} />}
          title="No teams yet"
          detail="Create a team, add two or more agents, and pick a lead to run the board."
          action={<Button variant="outline" size="sm" onClick={onCreate}>Create team</Button>}
        />
      ) : (
        <div className="grid grid-cols-3 gap-3.5 max-[980px]:grid-cols-1">
          {teams.map((team) => (
            <div
              key={team.id}
              className="relative flex cursor-pointer flex-col gap-[11px] overflow-hidden rounded-xl bg-[var(--color-background-card)] p-[15px] transition-[transform,background,box-shadow] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:-translate-y-0.5 hover:shadow-[0_6px_20px_-12px_rgba(0,0,0,0.7)]"
              onClick={() => router.push(`/teams/${team.id}`)}
            >
              <div className="flex items-center gap-2.5">
                <span className="grid h-[30px] w-[30px] place-items-center rounded-[9px] bg-[color-mix(in_srgb,var(--agent)_18%,transparent)] text-[13px] font-bold text-agent">
                  {(team.name || '?').charAt(0).toUpperCase()}
                </span>
                <span className="text-[13.5px] font-semibold">{team.name}</span>
                {team.lead_agent_id ? <Crown size={14} className="text-agent" aria-label="Lead picked" /> : null}
              </div>
              <div className="min-h-9 text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">
                {team.lead_agent_id ? 'Lead picked — the lead orchestrates this board.' : 'No lead yet — pick one so the board gets worked.'}
              </div>
              <div className="flex gap-3.5 pt-0.5 text-[11.5px] text-[var(--color-text-secondary)]">
                <span>members <b className="font-mono font-medium text-[var(--color-text-primary)] tabular-nums">{team.member_count}</b></span>
                <span>cards <b className="font-mono font-medium text-[var(--color-text-primary)] tabular-nums">{team.card_count}</b></span>
              </div>
              <div className="mt-0.5 flex items-center gap-2" onClick={(e) => e.stopPropagation()}>
                <Button variant="primary" size="sm" onClick={() => router.push(`/teams/${team.id}`)}>Open board</Button>
                <Button variant="ghost" size="sm" icon={<Trash2 size={15} />} onClick={() => setDeleting({ id: team.id, name: team.name })}>Delete</Button>
              </div>
            </div>
          ))}
          <div className="relative flex flex-col gap-[11px] overflow-hidden rounded-xl border border-dashed border-[color-mix(in_srgb,var(--border)_70%,transparent)] bg-[color-mix(in_srgb,var(--surface-2)_55%,transparent)] p-[15px] transition-[transform,background,box-shadow] duration-[var(--fast)] ease-[var(--ease)] hover:border-[color-mix(in_srgb,var(--agent)_45%,var(--border))] hover:bg-[var(--color-background-muted)]">
            <div className="flex items-center gap-2.5">
              <span className="grid h-[30px] w-[30px] place-items-center rounded-[9px] bg-[var(--color-background-surface)] text-[13px] font-bold text-[var(--color-text-secondary)]"><Plus size={16} /></span>
              <span className="text-[13.5px] font-semibold">New team</span>
            </div>
            <div className="min-h-9 text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">Group existing agents around a kanban board and pick a lead to orchestrate.</div>
            <div className="mt-0.5 flex items-center gap-2"><Button variant="outline" size="sm" onClick={onCreate}>Create team</Button></div>
          </div>
        </div>
      )}
    </AppShell>
  );
}
