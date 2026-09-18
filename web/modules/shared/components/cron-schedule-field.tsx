import { useState } from 'react';
import { cronToWords } from '@/modules/operations/lib/cron-words';
import { Text } from '@astryxdesign/core/Text';

export interface CronPreset {
  label: string;
  expr: string;
}

export const CRON_PRESETS: CronPreset[] = [
  { label: 'Weekdays 9am', expr: '0 9 * * 1-5' },
  { label: 'Every day 9am', expr: '0 9 * * *' },
  { label: 'Weekly (Mon 9am)', expr: '0 9 * * 1' },
  { label: 'Hourly', expr: '0 * * * *' },
];

export function CronScheduleField({
  id = 'cron-field',
  value,
  onChange,
  className = '',
}: {
  id?: string;
  value: string;
  onChange: (val: string) => void;
  className?: string;
}) {
  const words = value.trim() ? cronToWords(value) : '';

  return (
    <div className={`flex flex-col gap-2 ${className}`}>
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-2xs uppercase tracking-[0.05em] text-[var(--color-text-disabled)] me-1">
          Presets:
        </span>
        {CRON_PRESETS.map((p) => {
          const active = value.trim() === p.expr;
          return (
            <button
              key={p.expr}
              type="button"
              onClick={() => onChange(p.expr)}
              className={`rounded px-2 py-0.5 text-xs font-medium border transition-colors cursor-pointer ${
                active
                  ? 'border-agent bg-[color-mix(in_srgb,var(--agent)_12%,var(--surface-2))] text-[var(--color-text-primary)]'
                  : 'border-[var(--color-border)] bg-[var(--color-background-muted)] text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)] hover:border-[var(--color-border-hover)]'
              }`}
            >
              {p.label}
            </button>
          );
        })}
      </div>
      <div className="flex items-center gap-2">
        <input
          id={id}
          className="h-8 max-w-[280px] rounded-md border border-[var(--color-border)] bg-[var(--surface-1)] px-2.5 font-mono text-xs text-[var(--color-text-primary)] outline-none focus:border-agent transition-colors"
          value={value}
          placeholder="0 9 * * 1"
          onChange={(e) => onChange(e.target.value)}
        />
        {words && words !== value ? (
          <span className="text-xs font-medium text-agent">
            {words}
          </span>
        ) : null}
      </div>
      <Text type="supporting">
        {value.trim() && words === value
          ? 'Custom 5-field cron: minute hour day month weekday.'
          : !value.trim()
          ? 'Choose a preset above or type a 5-field cron expression.'
          : null}
      </Text>
    </div>
  );
}
