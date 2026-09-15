'use client';

import { useMemo, useState } from 'react';
import { AlertTriangle, ArrowUpRight, DollarSign, Repeat, Sparkles } from 'lucide-react';
import type { EventCatalogEntry, Project } from '@/lib/api';
import type { ProjectAccess } from '@/lib/ia';
import { Button, Callout, Panel } from '@/modules/shared/components/signal-primitives';

export const GOAL_OPTIONS = [
  {
    id: 'activation',
    label: 'Activation',
    situation: 'First value',
    outcome: 'More signups reaching the moment the product proves itself.',
    icon: Sparkles,
  },
  {
    id: 'retention',
    label: 'Retention',
    situation: 'Coming back',
    outcome: 'More people returning on day 1, 7, and 30.',
    icon: Repeat,
  },
  {
    id: 'revenue',
    label: 'Revenue',
    situation: 'Getting paid',
    outcome: 'More checkouts, upgrades, and net revenue.',
    icon: DollarSign,
  },
  {
    id: 'traffic',
    label: 'Traffic',
    situation: 'More of the right visits',
    outcome: 'Growing sessions and the sources that convert.',
    icon: ArrowUpRight,
  },
] as const;

export type GoalId = typeof GOAL_OPTIONS[number]['id'];

export function GoalPrompt({
  project,
  eventNames,
  onUpdateProject,
  access,
}: {
  project: Project;
  eventNames?: readonly (string | EventCatalogEntry)[];
  onUpdateProject: (patch: { goal?: string; activation_event?: string }) => Promise<void>;
  access: ProjectAccess;
}) {
  const [selectedGoal, setSelectedGoal] = useState<GoalId | null>(null);
  const [stage, setStage] = useState<'prompt' | 'activation_mapping'>('prompt');
  const [selectedEvent, setSelectedEvent] = useState<string>('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [dismissed, setDismissed] = useState(false);

  // Candidate events from the catalog, excluding the verification receipt
  const catalogEvents = useMemo(() => {
    if (!eventNames) return [];
    const list: Array<{ name: string; users: number }> = [];
    for (const entry of eventNames) {
      if (typeof entry === 'string') {
        if (entry && entry !== 'onboarding_verified') {
          list.push({ name: entry, users: 0 });
        }
      } else if (entry && typeof entry === 'object') {
        const name = entry.event_name;
        if (name && name !== 'onboarding_verified') {
          list.push({ name, users: entry.users || 0 });
        }
      }
    }
    return list.slice(0, 8);
  }, [eventNames]);

  // Hide if the prompt was dismissed, or goal is already recorded and not in active flow,
  // or user cannot write
  if (dismissed || !access.canWrite) return null;
  if (stage === 'prompt' && project.goal !== undefined && project.goal !== null) {
    return null;
  }

  async function handlePickGoal(goal: GoalId) {
    setSelectedGoal(goal);
    setError(null);
    setSaving(true);
    try {
      await onUpdateProject({ goal });
      if (goal === 'activation') {
        setStage('activation_mapping');
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : access.reason || 'Could not save the goal');
    } finally {
      setSaving(false);
    }
  }

  async function handleSkip() {
    setError(null);
    setSaving(true);
    try {
      await onUpdateProject({ goal: 'skipped' });
    } catch (err) {
      setError(err instanceof Error ? err.message : access.reason || 'Could not skip the goal');
    } finally {
      setSaving(false);
    }
  }

  async function handleSaveActivationEvent() {
    setError(null);
    setSaving(true);
    try {
      await onUpdateProject({ activation_event: selectedEvent });
      setDismissed(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not save activation event');
    } finally {
      setSaving(false);
    }
  }

  if (stage === 'activation_mapping') {
    return (
      <Panel
        title="Which event means “activated”?"
        action={
          <Button
            variant="ghost"
            size="sm"
            className="min-h-[44px]"
            onClick={() => setDismissed(true)}
            disabled={saving}
          >
            Decide later
          </Button>
        }
      >
        <p className="mb-3 text-sm text-[var(--color-text-secondary)]">
          Pick the event a new user fires when they first get real value. This becomes the project’s
          activation metric — the overview tile starts computing once it’s set.
        </p>

        {error ? (
          <div className="mb-3" role="alert">
            <Callout
              tone="warn"
              icon={<AlertTriangle size={16} />}
              label="Goal"
              title="Couldn’t save activation event"
              detail={error}
            />
          </div>
        ) : null}

        <div className="flex flex-col gap-2" role="group" aria-label="Select activation event">
          {catalogEvents.map((evt) => {
            const isPicked = selectedEvent === evt.name;
            return (
              <button
                key={evt.name}
                type="button"
                aria-pressed={isPicked}
                onClick={() => setSelectedEvent(evt.name)}
                disabled={saving}
                className={`flex min-h-[44px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] border px-4 py-3 text-left transition-[border-color,background-color] ${
                  isPicked
                    ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
                    : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
                }`}
              >
                <span className="font-mono text-sm">{evt.name}</span>
                {evt.users > 0 ? (
                  <span className="text-xs text-[var(--color-text-secondary)]">
                    {evt.users === 1 ? '1 person' : `${evt.users.toLocaleString()} people`}
                  </span>
                ) : null}
              </button>
            );
          })}

          <button
            type="button"
            aria-pressed={selectedEvent === ''}
            onClick={() => setSelectedEvent('')}
            disabled={saving}
            className={`flex min-h-[44px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] border px-4 py-3 text-left transition-[border-color,background-color] ${
              selectedEvent === ''
                ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
                : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
            }`}
          >
            <span className="font-mono text-sm text-[var(--color-text-secondary)]">None of these yet</span>
            <span className="text-xs text-[var(--color-text-secondary)]">keep it unset</span>
          </button>
        </div>

        <div className="mt-4 flex flex-wrap items-center gap-3 [&_button]:min-h-[44px]">
          <Button
            variant="primary"
            size="sm"
            onClick={handleSaveActivationEvent}
            disabled={saving || !selectedEvent}
          >
            {saving ? 'Saving…' : 'Save activation event'}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDismissed(true)}
            disabled={saving}
          >
            Decide later
          </Button>
        </div>
      </Panel>
    );
  }

  return (
    <Panel
      title="What are you trying to improve?"
      action={
        <Button
          variant="ghost"
          size="sm"
          className="min-h-[44px]"
          aria-label="Skip for now"
          onClick={handleSkip}
          disabled={saving}
        >
          Skip for now
        </Button>
      }
    >
      <p className="mb-4 text-sm text-[var(--color-text-secondary)]">
        Events are arriving. Pick the one thing this project should get better at —
        AgentRay points the metrics, the findings, and the first suggested experiment at it.
        You can change this any time in Settings.
      </p>

      {error ? (
        <div className="mb-3" role="alert">
          <Callout
            tone="warn"
            icon={<AlertTriangle size={16} />}
            label="Goal"
            title="Couldn’t save the goal"
            detail={error}
          />
        </div>
      ) : null}

      <div
        className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4"
        role="group"
        aria-label="Improvement goal"
      >
        {GOAL_OPTIONS.map((g) => {
          const Icon = g.icon;
          const isPressed = selectedGoal === g.id;
          return (
            <button
              key={g.id}
              type="button"
              aria-pressed={isPressed}
              onClick={() => handlePickGoal(g.id)}
              disabled={saving}
              className={`flex min-h-[44px] flex-col rounded-[var(--radius-lg)] border p-4 text-left transition-[border-color,background-color] ${
                isPressed
                  ? 'border-[var(--agent)] bg-[color-mix(in_srgb,var(--agent)_6%,transparent)]'
                  : 'border-[var(--color-border)] bg-[var(--color-background-surface)] hover:border-[var(--color-border-strong)]'
              }`}
            >
              <span className="mb-2 flex items-center gap-2 text-sm font-semibold uppercase tracking-[0.06em] text-[var(--agent)]">
                <Icon size={16} aria-hidden />
                <span>{g.label}</span>
              </span>
              <span className="block text-base font-semibold text-[var(--color-text-primary)]">
                {g.situation}
              </span>
              <span className="mt-1 block text-sm leading-[1.5] text-[var(--color-text-secondary)]">
                {g.outcome}
              </span>
            </button>
          );
        })}
      </div>
    </Panel>
  );
}
