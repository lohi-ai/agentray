'use client';

// PROTOTYPE — bs-1jektso6. Throwaway. implement rebuilds from ticket design.md.
// Not linked from production navigation.
// Open (after sign-in): http://localhost:3200/prototype/activation-candidates

import { useState } from 'react';
import { AlertTriangle, Sparkles } from 'lucide-react';
import { Button, Callout, EmptyState, Panel } from '@/modules/shared/components/signal-primitives';
import { EventNameCombobox } from '@/modules/shared/components/event-name-picker';
import { formatCompact, formatFractionAsPercent } from '@/lib/format';

// Mirrors the proposed GET /api/projects/:id/activation-candidates payload.
type Candidate = {
  event_name: string;
  users: number;        // distinct new users who fired it within 7d of first seen
  reach: number;        // share of the new-user cohort (0–1)
  d7_return: number;    // D7 return rate of users who fired it (0–1)
  baseline_d7: number;  // D7 return rate of all new users (0–1)
  lift: number;         // d7_return - baseline_d7
};

const CANDIDATES: Candidate[] = [
  { event_name: 'onboarding.completed', users: 412, reach: 0.61, d7_return: 0.48, baseline_d7: 0.22, lift: 0.26 },
  { event_name: 'project.created', users: 355, reach: 0.52, d7_return: 0.41, baseline_d7: 0.22, lift: 0.19 },
  { event_name: 'invite.sent', users: 128, reach: 0.19, d7_return: 0.55, baseline_d7: 0.22, lift: 0.33 },
];

const CATALOG_ONLY = [
  { name: 'page_view', users: 680 },
  { name: 'onboarding.completed', users: 412 },
  { name: 'session_start', users: 655 },
];

type Fixture = 'prompt' | 'settings' | 'not_ready' | 'error';

export default function ActivationCandidatesPrototype() {
  const [fixture, setFixture] = useState<Fixture>('prompt');
  return (
    <div className="min-h-dvh bg-[var(--color-background-body)] p-6">
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <span className="text-xs text-[var(--color-text-secondary)]">Prototype fixtures — not production chrome</span>
        {(['prompt', 'settings', 'not_ready', 'error'] as const).map((id) => (
          <Button key={id} size="sm" variant={fixture === id ? 'primary' : 'outline'} onClick={() => setFixture(id)}>
            {id}
          </Button>
        ))}
      </div>
      <div className="mx-auto max-w-[760px]">
        {fixture === 'prompt' ? <PromptStage key="p" /> : null}
        {fixture === 'settings' ? <SettingsPanel key="s" /> : null}
        {fixture === 'not_ready' ? <PromptStage key="n" notReady /> : null}
        {fixture === 'error' ? <PromptStage key="e" failed /> : null}
      </div>
    </div>
  );
}

// One ranked suggestion row: name + the evidence the ranking computed.
// Reuses the goal-prompt row shape (border, radius, min-h-44, aria-pressed).
function SuggestionRow({
  c,
  picked,
  onPick,
  disabled,
}: {
  c: Candidate;
  picked: boolean;
  onPick: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      aria-pressed={picked}
      onClick={onPick}
      disabled={disabled}
      className={`flex min-h-[44px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] border px-4 py-3 text-left transition-[border-color,background-color] ${
        picked
          ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
          : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
      }`}
    >
      <span className="flex min-w-0 flex-col gap-0.5">
        <span className="truncate font-mono text-sm">{c.event_name}</span>
        <span className="text-xs text-[var(--color-text-secondary)]">
          {formatCompact(c.users)} people · {formatFractionAsPercent(c.reach, 0)} of new users · D7 {formatFractionAsPercent(c.d7_return, 0)} vs {formatFractionAsPercent(c.baseline_d7, 0)} baseline
        </span>
      </span>
      <span
        className={`shrink-0 rounded-[var(--radius-md)] px-2 py-0.5 text-xs font-medium ${
          c.lift > 0
            ? 'text-[var(--color-text-secondary)]'
            : 'text-[var(--color-text-secondary)]'
        }`}
      >
        {c.lift > 0 ? `+${formatFractionAsPercent(c.lift, 0)} D7` : `${formatFractionAsPercent(c.lift, 0)} D7`}
      </span>
    </button>
  );
}

// GoalPrompt's activation_mapping stage with server-ranked suggestions.
function PromptStage({ notReady, failed }: { notReady?: boolean; failed?: boolean }) {
  const [selected, setSelected] = useState('');
  const [saved, setSaved] = useState(false);
  return (
    <Panel
      title="Which event means “activated”?"
      action={
        <Button variant="ghost" size="sm" className="min-h-[44px]">
          Decide later
        </Button>
      }
    >
      <p className="mb-3 text-sm text-[var(--color-text-secondary)]">
        Pick the event a new user fires when they first get real value. This becomes the project’s
        activation metric — the overview tile starts computing once it’s set.
      </p>

      {failed ? (
        <div className="mb-3" role="alert">
          <Callout
            tone="warn"
            icon={<AlertTriangle size={16} />}
            label="Suggestions"
            title="Couldn’t rank your events"
            detail="Showing the raw event list instead — pick one, or decide later."
          />
        </div>
      ) : null}

      {notReady ? (
        <div className="mb-3">
          <Callout
            tone="growth"
            icon={<Sparkles size={16} />}
            label="Suggestions"
            title="Not enough data to rank yet"
            detail="Suggestions appear once a 7-day cohort has matured. Until then, pick from the events already arriving."
          />
        </div>
      ) : null}

      <div className="flex flex-col gap-2" role="group" aria-label="Select activation event">
        {notReady || failed
          ? CATALOG_ONLY.map((evt) => (
              <button
                key={evt.name}
                type="button"
                aria-pressed={selected === evt.name}
                onClick={() => setSelected(evt.name)}
                className={`flex min-h-[44px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] border px-4 py-3 text-left transition-[border-color,background-color] ${
                  selected === evt.name
                    ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
                    : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
                }`}
              >
                <span className="font-mono text-sm">{evt.name}</span>
                <span className="text-xs text-[var(--color-text-secondary)]">
                  {evt.users === 1 ? '1 person' : `${evt.users.toLocaleString()} people`}
                </span>
              </button>
            ))
          : CANDIDATES.map((c) => (
              <SuggestionRow key={c.event_name} c={c} picked={selected === c.event_name} onPick={() => setSelected(c.event_name)} />
            ))}

        <button
          type="button"
          aria-pressed={selected === ''}
          onClick={() => setSelected('')}
          className={`flex min-h-[44px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] border px-4 py-3 text-left transition-[border-color,background-color] ${
            selected === ''
              ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
              : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
          }`}
        >
          <span className="font-mono text-sm text-[var(--color-text-secondary)]">None of these yet</span>
          <span className="text-xs text-[var(--color-text-secondary)]">keep it unset</span>
        </button>
      </div>

      <div className="mt-4 flex flex-wrap items-center gap-3 [&_button]:min-h-[44px]">
        <Button variant="primary" size="sm" onClick={() => setSaved(true)} disabled={!selected}>
          {saved ? 'Saved' : 'Save activation event'}
        </Button>
        <Button variant="ghost" size="sm">
          Decide later
        </Button>
      </div>
    </Panel>
  );
}

// Settings → Projects → Improvement goal: the combobox stays for free text;
// ranked suggestions sit above it as one-click picks.
function SettingsPanel() {
  const [draft, setDraft] = useState('');
  const [saved, setSaved] = useState('');
  return (
    <Panel title="Improvement goal">
      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-start justify-between gap-4 border-t border-[var(--color-border)] pt-4">
          <div className="max-w-[440px]">
            <div className="font-semibold">Activation event</div>
            <p className="text-xs text-[var(--color-text-secondary)]">
              The event that counts as a new user reaching first value. The overview activation tile computes once this is set.
            </p>
            <p className="mt-2 text-xs text-[var(--color-text-secondary)]">
              Suggested from your data — ranked by how many new users reach it and how much it predicts day-7 return.
            </p>
          </div>
          <div className="flex min-w-[320px] flex-1 flex-col gap-2">
            {CANDIDATES.map((c) => (
              <button
                key={c.event_name}
                type="button"
                onClick={() => { setDraft(c.event_name); setSaved(''); }}
                className={`flex min-h-[44px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] border px-4 py-2 text-left transition-[border-color,background-color] ${
                  draft === c.event_name
                    ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
                    : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
                }`}
              >
                <span className="flex min-w-0 flex-col">
                  <span className="truncate font-mono text-sm">{c.event_name}</span>
                  <span className="text-xs text-[var(--color-text-secondary)]">
                    {formatCompact(c.users)} people · {formatFractionAsPercent(c.reach, 0)} of new users · +{formatFractionAsPercent(c.lift, 0)} D7
                  </span>
                </span>
                <span className="shrink-0 text-xs text-[var(--color-text-secondary)]">Use</span>
              </button>
            ))}
            <div className="mt-1 flex items-center gap-2">
              <EventNameCombobox
                value={draft}
                onChange={(v) => { setDraft(v); setSaved(''); }}
                placeholder="e.g. onboarding.completed"
                className="flex-1"
              />
              <Button
                variant="outline"
                size="sm"
                className="min-h-[44px]"
                disabled={!draft.trim() || draft.trim() === saved}
                onClick={() => setSaved(draft.trim())}
              >
                {saved ? 'Saved' : 'Save'}
              </Button>
            </div>
          </div>
        </div>
      </div>
    </Panel>
  );
}
