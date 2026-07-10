'use client';

import { useState } from 'react';
import { Pencil, Plus, Trash2 } from 'lucide-react';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import type { AudienceInput, AudienceKind, ProjectAudience, SubscriptionMapping, SubscriptionMappingInput } from '@/lib/api';
import { SUBSCRIPTION_KINDS } from '@/lib/api';
import { useCohortAudiences, useSubscriptionMapping } from '@/modules/app/hooks';
import { Button } from '@/modules/shared/components/signal-primitives';
import { Modal } from '@/modules/shared/components/modal';

// Static-trait kinds are always offered (all-time event aggregates). Subscription
// kinds need a configured mapping (they read point-in-time status off the
// projection) so they are gated below.
const STATIC_KIND_OPTIONS = [
  { value: 'paid', label: 'Paid — ever fired a revenue event' },
  { value: 'plan', label: 'Plan — latest plan in a set' },
];
const SUBSCRIPTION_KIND_OPTIONS = [
  { value: 'active_subscriber', label: 'Active subscriber — subscribed (or trialing) now' },
  { value: 'trialing', label: 'Trialing — currently on a trial' },
  { value: 'churned', label: 'Churned — was active, now expired/cancelled' },
  { value: 'plan_active', label: 'Plan (active) — active AND plan in a set' },
];

// describeRule renders a human summary of an audience's matching rule, shown in
// the list so a viewer understands what each group captures without editing it.
function describeRule(a: ProjectAudience): string {
  switch (a.kind) {
    case 'paid':
      return 'Ever fired a revenue event';
    case 'plan':
      return `Latest plan in ${a.plans.join(', ') || '—'}`;
    case 'active_subscriber':
      return 'Active or trialing subscriber now';
    case 'trialing':
      return 'Currently on a trial';
    case 'churned':
      return 'Was active, now expired or cancelled';
    case 'plan_active':
      return `Active on plan in ${a.plans.join(', ') || '—'}`;
    default:
      return a.kind;
  }
}

// planKinds need a comma-separated plan list; the rest detect from status alone.
const PLAN_KINDS: AudienceKind[] = ['plan', 'plan_active'];

type Draft = { id: string | null; label: string; kind: AudienceKind; plans: string };
type Tab = 'audiences' | 'subscription';

const EMPTY_DRAFT: Draft = { id: null, label: '', kind: 'paid', plans: '' };

// AudienceManager is the per-project editor for custom cohort audiences. It lists
// the project's groups and edits them through a single inline draft form; the
// underlying rule is structured (kind + plan values) so nothing here is raw SQL —
// the server compiles it. Static-trait kinds (paid/plan) work off all-time event
// aggregates; subscription-state kinds (active/trialing/churned/plan_active) read
// the point-in-time projection and unlock once the Subscription tab is configured.
export function AudienceManager({ onClose }: { onClose: () => void }) {
  const { audiences, loading, busy, create, update, remove } = useCohortAudiences();
  const { mapping } = useSubscriptionMapping();
  const [tab, setTab] = useState<Tab>('audiences');
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT);
  const [error, setError] = useState('');

  const editing = draft.id !== null;
  const subsReady = !!mapping?.configured;
  const kindOptions = subsReady ? [...STATIC_KIND_OPTIONS, ...SUBSCRIPTION_KIND_OPTIONS] : STATIC_KIND_OPTIONS;
  const needsPlans = PLAN_KINDS.includes(draft.kind);

  function startEdit(a: ProjectAudience) {
    setError('');
    setTab('audiences');
    setDraft({ id: a.id, label: a.label, kind: a.kind, plans: a.plans.join(', ') });
  }

  function reset() {
    setDraft(EMPTY_DRAFT);
    setError('');
  }

  async function save() {
    const label = draft.label.trim();
    if (!label) {
      setError('Give the audience a name.');
      return;
    }
    const plans = needsPlans ? draft.plans.split(',').map((p) => p.trim()).filter(Boolean) : [];
    if (needsPlans && plans.length === 0) {
      setError('List at least one plan value.');
      return;
    }
    const input: AudienceInput = { label, kind: draft.kind, plans };
    try {
      if (draft.id) await update(draft.id, input);
      else await create(input);
      reset();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not save the audience.');
    }
  }

  return (
    <Modal title="Manage audiences" onClose={onClose} wide>
      {/* Tabs */}
      <div className="mb-3 flex gap-1 border-b border-[var(--color-border)]">
        <TabButton active={tab === 'audiences'} onClick={() => setTab('audiences')}>
          Audiences
        </TabButton>
        <TabButton active={tab === 'subscription'} onClick={() => setTab('subscription')}>
          Subscription setup
          {!subsReady ? <span className="ml-1.5 text-[10px] text-[var(--color-text-disabled)]">·  not set</span> : null}
        </TabButton>
      </div>

      {tab === 'audiences' ? (
        <>
          <p className="mb-3 text-[12.5px] leading-relaxed text-[var(--color-text-secondary)]">
            Define groups beyond the built-ins (Everyone, Users, Guests, Paid, Premium). Static groups match all-time
            event history; subscription groups read point-in-time status and need the{' '}
            <button type="button" className="text-[var(--color-primary)] underline" onClick={() => setTab('subscription')}>
              Subscription setup
            </button>{' '}
            tab configured first.
          </p>

          {/* Existing custom audiences */}
          <div className="mb-4 flex flex-col gap-1.5">
            {loading ? (
              <p className="text-[12.5px] text-[var(--color-text-disabled)]">Loading…</p>
            ) : audiences.length === 0 ? (
              <p className="rounded-md bg-[var(--color-background-muted)] px-3 py-2.5 text-[12.5px] text-[var(--color-text-secondary)]">
                No custom audiences yet. Add one below — it appears in the segment toggle for everyone on this project.
              </p>
            ) : (
              audiences.map((a) => (
                <div
                  key={a.id}
                  className="flex items-center justify-between gap-3 rounded-md bg-[var(--color-background-muted)] px-3 py-2"
                >
                  <div className="min-w-0">
                    <div className="truncate text-[13px] font-medium text-[var(--color-text-primary)]">{a.label}</div>
                    <div className="truncate text-[11.5px] text-[var(--color-text-secondary)]">{describeRule(a)}</div>
                  </div>
                  <div className="flex shrink-0 items-center gap-1">
                    <Button variant="ghost" size="sm" icon={<Pencil size={13} />} onClick={() => startEdit(a)}>
                      Edit
                    </Button>
                    <Button variant="ghost" size="sm" icon={<Trash2 size={13} />} disabled={busy} onClick={() => remove(a.id)}>
                      Delete
                    </Button>
                  </div>
                </div>
              ))
            )}
          </div>

          {/* Draft form */}
          <div className="rounded-lg border border-[var(--color-border)] p-3">
            <div className="mb-2.5 text-[12px] font-medium text-[var(--color-text-secondary)]">
              {editing ? 'Edit audience' : 'New audience'}
            </div>
            <div className="flex flex-col gap-3">
              <TextInput
                label="Name"
                value={draft.label}
                placeholder="e.g. Pro subscribers"
                onChange={(v: string) => setDraft((d) => ({ ...d, label: v }))}
                width="100%"
              />
              <Selector
                label="Detect by"
                size="sm"
                options={kindOptions}
                value={draft.kind}
                onChange={(v: string) => setDraft((d) => ({ ...d, kind: v as AudienceKind }))}
              />
              {needsPlans ? (
                <TextInput
                  label="Plan values (comma separated)"
                  value={draft.plans}
                  placeholder="pro, pro_annual, enterprise"
                  onChange={(v: string) => setDraft((d) => ({ ...d, plans: v }))}
                  width="100%"
                />
              ) : null}
              {SUBSCRIPTION_KINDS.includes(draft.kind) ? (
                <p className="text-[11.5px] leading-relaxed text-[var(--color-text-secondary)]">
                  Point-in-time: evaluated per cohort week from your subscription mapping, so this draws a real
                  subscription-retention curve rather than “ever paid”.
                </p>
              ) : null}
            </div>
            {error ? <p className="mt-2 text-[12px] text-[var(--danger)]">{error}</p> : null}
            <div className="mt-3 flex items-center justify-end gap-2">
              {editing ? (
                <Button variant="ghost" size="sm" onClick={reset}>
                  Cancel
                </Button>
              ) : null}
              <Button variant="primary" size="sm" icon={editing ? undefined : <Plus size={14} />} disabled={busy} onClick={save}>
                {editing ? 'Save changes' : 'Add audience'}
              </Button>
            </div>
          </div>
        </>
      ) : (
        <SubscriptionSetup mapping={mapping} />
      )}
    </Modal>
  );
}

function TabButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`-mb-px border-b-2 px-3 py-1.5 text-[12.5px] font-medium transition-colors ${
        active
          ? 'border-[var(--color-primary)] text-[var(--color-text-primary)]'
          : 'border-transparent text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]'
      }`}
    >
      {children}
    </button>
  );
}

type MapDraft = Omit<SubscriptionMappingInput, 'grace_days'> & { grace_days: string };

function toMapDraft(m: SubscriptionMapping | null): MapDraft {
  return {
    start_event: m?.start_event ?? '',
    renew_event: m?.renew_event ?? '',
    cancel_event: m?.cancel_event ?? '',
    plan_prop: m?.plan_prop ?? '',
    amount_prop: m?.amount_prop ?? '',
    period_end_prop: m?.period_end_prop ?? '',
    trial_prop: m?.trial_prop ?? '',
    grace_days: String(m?.grace_days ?? 1),
  };
}

// SubscriptionSetup is the per-project mapping editor: it names which events and
// properties carry subscription lifecycle so the cohort projection can derive
// point-in-time status. Tokens are validated + escaped server-side (never raw SQL).
function SubscriptionSetup({ mapping }: { mapping: SubscriptionMapping | null }) {
  const { save, busy } = useSubscriptionMapping();
  const [form, setForm] = useState<MapDraft>(() => toMapDraft(mapping));
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);

  const set = (key: keyof MapDraft) => (v: string) => {
    setForm((f) => ({ ...f, [key]: v }));
    setSaved(false);
  };

  async function submit() {
    setError('');
    if (!form.start_event.trim()) {
      setError('A start event is required (the event that opens a subscription).');
      return;
    }
    const grace = Number(form.grace_days);
    if (!Number.isFinite(grace) || grace < 0 || grace > 90) {
      setError('Grace days must be between 0 and 90.');
      return;
    }
    try {
      await save({
        start_event: form.start_event.trim(),
        renew_event: form.renew_event.trim(),
        cancel_event: form.cancel_event.trim(),
        plan_prop: form.plan_prop.trim(),
        amount_prop: form.amount_prop.trim(),
        period_end_prop: form.period_end_prop.trim(),
        trial_prop: form.trial_prop.trim(),
        grace_days: grace,
      });
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not save the mapping.');
    }
  }

  const statusCapable = form.start_event.trim() && form.period_end_prop.trim();

  return (
    <div>
      <p className="mb-3 text-[12.5px] leading-relaxed text-[var(--color-text-secondary)]">
        Map your subscription lifecycle so cohorts can tell <em>active</em> from <em>churned</em> per week. Names must
        be event/property names already in your stream. A <strong>period-end property</strong> (the ISO timestamp a
        paid period runs until) is what makes status point-in-time — without it, only the static Paid/Plan groups work.
      </p>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <TextInput label="Start event" value={form.start_event} placeholder="subscription_started" onChange={set('start_event')} width="100%" />
        <TextInput label="Renew event" value={form.renew_event} placeholder="subscription_renewed" onChange={set('renew_event')} width="100%" />
        <TextInput label="Cancel event" value={form.cancel_event} placeholder="subscription_cancelled" onChange={set('cancel_event')} width="100%" />
        <TextInput label="Period-end property" value={form.period_end_prop} placeholder="current_period_end" onChange={set('period_end_prop')} width="100%" />
        <TextInput label="Plan property" value={form.plan_prop} placeholder="plan" onChange={set('plan_prop')} width="100%" />
        <TextInput label="Amount property" value={form.amount_prop} placeholder="amount" onChange={set('amount_prop')} width="100%" />
        <TextInput label="Trial property (boolean)" value={form.trial_prop} placeholder="is_trial" onChange={set('trial_prop')} width="100%" />
        <TextInput label="Grace days (0–90)" value={form.grace_days} placeholder="1" onChange={set('grace_days')} width="100%" />
      </div>

      {!statusCapable ? (
        <p className="mt-2.5 rounded-md bg-[var(--color-background-muted)] px-3 py-2 text-[11.5px] text-[var(--color-text-secondary)]">
          Set both a start event and a period-end property to unlock the Active / Trialing / Churned subscription
          audiences.
        </p>
      ) : null}
      {error ? <p className="mt-2 text-[12px] text-[var(--danger)]">{error}</p> : null}
      <div className="mt-3 flex items-center justify-end gap-2">
        {saved ? <span className="text-[12px] text-[var(--color-primary)]">Saved</span> : null}
        <Button variant="primary" size="sm" disabled={busy} onClick={submit}>
          Save mapping
        </Button>
      </div>
    </div>
  );
}
