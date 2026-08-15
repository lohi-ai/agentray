'use client';

import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Typeahead } from '@astryxdesign/core/Typeahead';
import { effectiveTierWindow, formatTokens, type ModelPickerItem } from './model-picker';

// The three workspace tiers, ordered the way the job happens: Default is the
// one that must be set, Lite and Pro are refinements of it.
//
// Wording is not invented. `title` matches the tier labels already shipped on
// the agent setup page (TIER_LABELS in modules/agents/[agentId]/setup/page.tsx)
// so the two screens agree, and `does`/`note` restate that page's TASK_LABELS,
// faithful to the real default task→tier map in
// internal/dataplane/store/agent_task_tiers.go (triage + compaction → lite,
// run → flash, reflection → pro).
export const TIERS = [
  {
    key: 'flash',
    title: 'Default (balanced)',
    does: 'Doing the work — the main analysis and answers.',
    note: 'Every agent starts here unless you send it somewhere else.',
    required: true,
  },
  {
    key: 'lite',
    title: 'Lite (fastest, cheapest)',
    does: 'Quick routing and summarizing history.',
    note: 'Deciding what a message needs, and compressing long conversations to save cost.',
    required: false,
  },
  {
    key: 'pro',
    title: 'Pro (deepest)',
    does: 'Learning & review.',
    note: 'Reflecting on past runs to improve. Costs the most — used the least.',
    required: false,
  },
] as const;

export type TierKey = (typeof TIERS)[number]['key'];
export type TierSpec = (typeof TIERS)[number];

export function TierBlock({
  tier,
  value,
  inherit,
  onInheritChange,
  onChange,
  contextWindow,
  onContextWindowChange,
  searchSource,
  isLoading,
}: {
  tier: TierSpec;
  value: ModelPickerItem | null;
  inherit: boolean;
  onInheritChange: ((checked: boolean) => void) | null;
  onChange: (item: ModelPickerItem | null) => void;
  /** The operator's override in tokens; 0 means "use what we detected". */
  contextWindow: number;
  onContextWindowChange: (tokens: number) => void;
  searchSource: { search: (q: string) => ModelPickerItem[]; bootstrap: () => ModelPickerItem[] };
  isLoading: boolean;
}) {
  return (
    <div className="border-t border-[var(--color-border)] pt-4 first:border-t-0 first:pt-0">
      <div className="mb-1 flex flex-wrap items-center gap-2">
        <span className="text-[13px] text-[var(--color-text-primary)]">{tier.title}</span>
        {tier.required ? <span className="text-[11.5px] text-[var(--color-text-secondary)]">Required</span> : null}
      </div>
      <p className="mb-1 max-w-[560px] text-[12.5px] text-[var(--color-text-primary)]">{tier.does}</p>
      <p className="mb-3 max-w-[560px] text-[12px] text-[var(--color-text-secondary)]">{tier.note}</p>

      {onInheritChange ? (
        <div className="mb-2">
          <CheckboxInput
            label="Same as Default"
            description="Leave this on unless you want a different model here."
            value={inherit}
            onChange={onInheritChange}
          />
        </div>
      ) : null}

      {!inherit ? (
        <div className="max-w-[420px]">
          <Typeahead
            label="Model"
            description={
              isLoading ? 'Loading the models your providers offer…' : 'Type to search — every model your providers offer.'
            }
            placeholder="Search models…"
            hasEntriesOnFocus
            debounceMs={0}
            maxMenuItems={8}
            searchSource={searchSource}
            value={value}
            onChange={onChange}
            width="100%"
            emptySearchResultsText="No model matches that."
            renderItem={(item: ModelPickerItem) => (
              <span className="flex w-full items-baseline justify-between gap-3">
                <span className="text-[13px]">{item.label}</span>
                <span className="text-[11.5px] text-[var(--color-text-secondary)]">{item.auxiliaryData.provider}</span>
              </span>
            )}
          />
          {value ? (
            <ContextWindowField
              detected={value.auxiliaryData.contextWindow}
              override={contextWindow}
              onChange={onContextWindowChange}
            />
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// How much conversation this model can hold. It is shown, not asked: for the
// audience this tab was built for the right number is already known, and the
// only reason to type one is an endpoint no catalog can describe — a self-hosted
// model, or a gateway serving a smaller window than the model's name implies.
//
// Getting this wrong upward is what kills a long run (the agent never compacts,
// then the provider rejects the request), so the unknown case says so plainly
// rather than showing a confident default.
function ContextWindowField({
  detected,
  override,
  onChange,
}: {
  detected: number;
  override: number;
  onChange: (tokens: number) => void;
}) {
  const manual = override > 0;
  const effective = effectiveTierWindow(detected, override);

  return (
    <div className="mt-3">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="text-[12px] text-[var(--color-text-secondary)]">Conversation size limit</span>
        <span className="text-[12.5px] text-[var(--color-text-primary)]">
          {effective > 0 ? `${formatTokens(effective)} tokens` : 'Not known for this model'}
        </span>
        <button
          type="button"
          className="text-[12px] text-[var(--color-text-secondary)] underline underline-offset-2 hover:text-[var(--color-text-primary)]"
          onClick={() => onChange(manual ? 0 : detected || 128000)}
        >
          {manual ? 'Use the detected size' : 'Set it myself'}
        </button>
      </div>

      {manual ? (
        <div className="mt-2 max-w-[220px]">
          <TextInput
            label="Tokens"
            value={String(override)}
            placeholder="128000"
            // Digits only: the field feeds a token count, and a stray character
            // would otherwise parse to NaN and silently save as "no override".
            onChange={(v: string) => onChange(Number(v.replace(/\D/g, '')) || 0)}
            width="100%"
          />
        </div>
      ) : (
        <p className="mt-1 max-w-[560px] text-[11.5px] text-[var(--color-text-secondary)]">
          {effective > 0
            ? 'Taken from the model. The agent summarizes older messages before reaching it.'
            : 'The agent will fall back to the workspace default. Set it yourself if this endpoint holds less.'}
        </p>
      )}
    </div>
  );
}
