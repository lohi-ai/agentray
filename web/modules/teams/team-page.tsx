'use client';

import { useState } from 'react';
import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, ChevronLeft, ChevronRight, Crown, Plus, Trash2 } from 'lucide-react';
import { Selector } from '@astryxdesign/core/Selector';
import type { TeamCard } from '@/lib/api';
import { useAgents } from '@/modules/agent/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { ConfirmDialog, PromptDialog } from '@/modules/shared/components/modal';
import { Button, EmptyState, Intro, Loading, Panel, StatusPill } from '@/modules/shared/components/signal-primitives';
import { useTeam } from './hooks';

const columnLabel = (status: string) => status.charAt(0).toUpperCase() + status.slice(1);

// TeamPage is one team's home: the roster (add/remove members, pick the lead)
// and the kanban board the lead orchestrates. Moving a card here is the same
// write the lead's team_board tool performs.
export function TeamPage() {
  const params = useParams<{ teamId: string }>();
  const teamID = params?.teamId ?? '';
  const router = useRouter();
  const { team, members, cards, statuses, isLoading, notFound, updateTeam, upsertMember, removeMember, createCard, updateCard, removeCard } = useTeam(teamID);
  const { agents } = useAgents();
  const [addingCard, setAddingCard] = useState(false);
  const [addAgentID, setAddAgentID] = useState('');
  const [deletingCard, setDeletingCard] = useState<TeamCard | null>(null);

  const rosterIDs = new Set(members.map((m) => m.agent_id));
  const addable = agents.filter((a) => !rosterIDs.has(a.id));
  const assigneeOptions = [{ value: '', label: 'Unassigned' }, ...members.map((m) => ({ value: m.agent_id, label: m.name }))];

  if (notFound) {
    return (
      <AppShell active="agents">
        <EmptyState title="Team not found" detail="It may have been deleted, or it belongs to another project." action={<Button variant="outline" size="sm" onClick={() => router.push('/teams')}>Back to teams</Button>} />
      </AppShell>
    );
  }

  const move = (card: TeamCard, dir: 1 | -1) => {
    const idx = statuses.indexOf(card.status);
    const next = statuses[idx + dir];
    if (next) void updateCard(card.id, { status: next });
  };

  return (
    <AppShell active="agents">
      {addingCard ? (
        <PromptDialog
          title="New card"
          label="Card title"
          placeholder="e.g. Draft the launch post"
          submitLabel="Add card"
          onSubmit={(title) => void createCard(title)}
          onClose={() => setAddingCard(false)}
        />
      ) : null}
      {deletingCard ? (
        <ConfirmDialog
          title={`Delete “${deletingCard.title}”?`}
          detail="Removes the card from the board."
          confirmLabel="Delete card"
          danger
          onConfirm={() => void removeCard(deletingCard.id)}
          onClose={() => setDeletingCard(null)}
        />
      ) : null}
      <Intro
        title={
          <span className="flex items-center gap-2.5">
            <Button variant="ghost" size="sm" icon={<ArrowLeft size={15} />} onClick={() => router.push('/teams')}>Teams</Button>
            {team?.name ?? '…'}
          </span>
        }
        sub="The lead — and only the lead — gets the orchestrator skill and works this board through its teammates."
        action={<Button variant="primary" icon={<Plus size={15} />} onClick={() => setAddingCard(true)}>New card</Button>}
      />
      {isLoading && !team ? <Loading label="Loading team…" /> : null}

      <div className="mb-4">
        <Panel
          title={`Roster (${members.length})`}
          action={
            addable.length > 0 ? (
              <span className="flex items-center gap-2">
                <Selector
                  label="Agent to add"
                  isLabelHidden
                  size="sm"
                  options={[{ value: '', label: 'Add an agent…' }, ...addable.map((a) => ({ value: a.id, label: a.name }))]}
                  value={addAgentID}
                  onChange={(v) => setAddAgentID(v)}
                />
                <Button
                  variant="outline"
                  size="sm"
                  icon={<Plus size={15} />}
                  disabled={!addAgentID}
                  onClick={() => { if (addAgentID) { void upsertMember(addAgentID); setAddAgentID(''); } }}
                >
                  Add
                </Button>
              </span>
            ) : undefined
          }
        >
          {members.length === 0 ? (
            <div className="py-2 text-[12.5px] text-[var(--color-text-secondary)]">No members yet — add at least two agents, then pick a lead.</div>
          ) : (
            <div className="flex flex-col">
              {members.map((m) => (
                <div key={m.agent_id} className="flex items-center gap-2.5 border-b border-[color-mix(in_srgb,var(--border)_55%,transparent)] py-2 last:border-b-0">
                  <span className="grid h-[26px] w-[26px] place-items-center rounded-lg bg-[color-mix(in_srgb,var(--agent)_18%,transparent)] text-[12px] font-bold text-agent">
                    {(m.name || '?').charAt(0).toUpperCase()}
                  </span>
                  <span className="text-[13px] font-medium">{m.name}</span>
                  {m.is_lead ? <StatusPill status="working" label="Lead" grow={false} /> : null}
                  {!m.enabled ? <StatusPill status="paused" label="Disabled" grow={false} /> : null}
                  {m.role ? <span className="text-[11.5px] text-[var(--color-text-secondary)]">{m.role}</span> : null}
                  <span className="flex-1" />
                  {!m.is_lead ? (
                    <Button variant="ghost" size="sm" icon={<Crown size={14} />} onClick={() => void updateTeam({ lead_agent_id: m.agent_id })}>Make lead</Button>
                  ) : null}
                  <Button variant="ghost" size="sm" icon={<Trash2 size={14} />} onClick={() => void removeMember(m.agent_id)}>Remove</Button>
                </div>
              ))}
            </div>
          )}
        </Panel>
      </div>

      <div className="grid grid-cols-4 gap-3.5 max-[1100px]:grid-cols-2 max-[640px]:grid-cols-1">
        {statuses.map((status) => {
          const column = cards.filter((c) => c.status === status);
          return (
            <div key={status} className="flex min-h-[180px] flex-col gap-2.5 rounded-xl bg-[color-mix(in_srgb,var(--surface-2)_55%,transparent)] p-3">
              <div className="flex items-center gap-2 text-[12px] font-semibold uppercase tracking-wide text-[var(--color-text-secondary)]">
                {columnLabel(status)}
                <span className="font-mono text-[11px] tabular-nums">{column.length}</span>
                <span className="flex-1" />
                {status === 'backlog' ? (
                  <Button variant="ghost" size="sm" icon={<Plus size={14} />} onClick={() => setAddingCard(true)}>Add</Button>
                ) : null}
              </div>
              {column.map((card) => (
                <div key={card.id} className="flex flex-col gap-2 rounded-lg bg-[var(--color-background-card)] p-3">
                  <div className="text-[13px] font-medium leading-snug">{card.title}</div>
                  {card.body ? <div className="text-[12px] leading-[1.5] text-[var(--color-text-secondary)]">{card.body}</div> : null}
                  <Selector
                    label="Assignee"
                    isLabelHidden
                    size="sm"
                    options={assigneeOptions}
                    value={card.assignee_agent_id}
                    onChange={(v) => void updateCard(card.id, { assignee_agent_id: v })}
                  />
                  <div className="flex items-center gap-1">
                    <Button variant="ghost" size="sm" icon={<ChevronLeft size={14} />} disabled={statuses.indexOf(card.status) === 0} onClick={() => move(card, -1)}>{''}</Button>
                    <Button variant="ghost" size="sm" icon={<ChevronRight size={14} />} disabled={statuses.indexOf(card.status) === statuses.length - 1} onClick={() => move(card, 1)}>{''}</Button>
                    <span className="flex-1" />
                    <Button variant="ghost" size="sm" icon={<Trash2 size={14} />} onClick={() => setDeletingCard(card)}>{''}</Button>
                  </div>
                </div>
              ))}
              {column.length === 0 ? <div className="py-3 text-center text-[11.5px] text-[var(--color-text-secondary)]">Empty</div> : null}
            </div>
          );
        })}
      </div>
    </AppShell>
  );
}
