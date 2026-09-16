'use client';

import { AlertTriangle, Sparkles } from 'lucide-react';
import type { ActivationCandidate } from '@/lib/api';
import { formatCompact, formatFractionAsPercent } from '@/lib/format';
import { Callout } from './signal-primitives';

// ActivationSuggestions renders the server-ranked activation-event candidates
// with the evidence behind each rank (people reached, share of new users, D7
// lift over the cohort baseline). Both pickers — GoalPrompt's mapping stage
// and Settings → Projects — render the same row so the numbers mean the same
// thing wherever the owner meets them.
//
// The component only handles the ranked list; the caller keeps its own
// selection state and save path (accepting a suggestion is still an explicit
// updateProject write — nothing here auto-sets activation_event).

export function ActivationSuggestionRow({
  candidate,
  picked,
  onPick,
  disabled,
  action,
}: {
  candidate: ActivationCandidate;
  picked: boolean;
  onPick: () => void;
  disabled?: boolean;
  // Right-side affordance: the prompt shows the signed lift chip; settings
  // shows a "Use" hint. Callers pass what fits their surface.
  action?: React.ReactNode;
}) {
  const c = candidate;
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
      {action ?? (
        <span className="shrink-0 rounded-[var(--radius-md)] px-2 py-0.5 text-xs font-medium text-[var(--color-text-secondary)]">
          {c.lift > 0 ? `+${formatFractionAsPercent(c.lift, 0)} D7` : `${formatFractionAsPercent(c.lift, 0)} D7`}
        </span>
      )}
    </button>
  );
}

// ActivationSuggestionsNotice is the honest-empty and failure copy for the
// suggestions read: not_ready explains the cohort has not matured, an error
// says the raw catalog is still usable. Both are Callouts, not empty space.
export function ActivationSuggestionsNotice({ kind }: { kind: 'not_ready' | 'error' }) {
  if (kind === 'error') {
    return (
      <div className="mb-3" role="alert">
        <Callout
          tone="warn"
          icon={<AlertTriangle size={16} />}
          label="Suggestions"
          title="Couldn’t rank your events"
          detail="Showing the raw event list instead — pick one, or decide later."
        />
      </div>
    );
  }
  return (
    <div className="mb-3">
      <Callout
        tone="growth"
        icon={<Sparkles size={16} />}
        label="Suggestions"
        title="Not enough data to rank yet"
        detail="Suggestions appear once a 7-day cohort has matured. Until then, pick from the events already arriving."
      />
    </div>
  );
}
