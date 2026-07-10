'use client';

import { useMemo, useState, type ReactNode } from 'react';
import { CalendarRange, RotateCcw, TriangleAlert } from 'lucide-react';
import { Calendar, type DateRange } from '@astryxdesign/core/Calendar';
import { Popover } from '@astryxdesign/core/Popover';
import { Selector } from '@astryxdesign/core/Selector';
import { defaultFilters, type Filters } from '@/lib/api';
import { useAuthStore, useFiltersStore } from '@/lib/app-state';
import { formatDate } from '@/lib/format';
import { useFilters } from '@/modules/app/hooks';

// RANGE_PRESETS are the windows offered by the range dropdown, in hours. They
// match rangeLabel's phrasing so the trigger reads back exactly what was picked.
const RANGE_PRESETS: Array<{ hours: number; label: string }> = [
  { hours: 1, label: 'Last hour' },
  { hours: 24, label: 'Last 24 hours' },
  { hours: 168, label: 'Last 7 days' },
  { hours: 720, label: 'Last 30 days' },
  { hours: 2160, label: 'Last 90 days' },
];

const CUSTOM = '__custom__';

// EVENT_TYPES is the default facet for the event_type filter. Pages may pass a
// data-derived list via `eventTypes`; '' is rendered as "All types".
const EVENT_TYPES = ['agent', 'web', 'product'];

// chip is the shared read-only pill (project, custom-range summary).
function Chip({ children }: { children: ReactNode }) {
  return <span className="inline-flex h-8 items-center gap-1.5 rounded-md bg-[var(--color-background-muted)] px-2.5 text-xs text-[var(--color-text-secondary)]">{children}</span>;
}

// FilterBar is the single, interactive filter surface shared across the
// dashboard and analytics pages. It reads/writes the global Filters store and
// commits on every change (which re-runs the console query). Page-specific
// facets render through `extra`; the time range, event type, and errors-only
// toggle are universal.
export function FilterBar({
  extra,
  eventTypes = EVENT_TYPES,
  showEventType = true,
  showErrors = true,
}: {
  extra?: ReactNode;
  eventTypes?: string[];
  showEventType?: boolean;
  showErrors?: boolean;
}) {
  const project = useAuthStore((s) => s.project);
  const applied = useFiltersStore((s) => s.appliedFilters);
  const { refresh } = useFilters();
  const [calendarOpen, setCalendarOpen] = useState(false);

  const hasCustom = !!applied.from && !!applied.to;
  const rangeValue = hasCustom ? CUSTOM : String(applied.hours);

  // The Reset affordance only appears once any bar-controlled field departs from
  // its default, so a clean view stays uncluttered.
  const dirty =
    applied.hours !== defaultFilters.hours ||
    !!applied.from ||
    !!applied.to ||
    !!applied.event_type ||
    applied.error_only ||
    !!applied.event_name ||
    !!applied.search;

  // Astryx Calendar uses ISO date strings (YYYY-MM-DD); the filter store keeps
  // full ISO datetimes, so slice/parse at the boundary.
  const range: DateRange | undefined = useMemo(
    () => (hasCustom ? ({ start: applied.from.slice(0, 10), end: applied.to.slice(0, 10) } as DateRange) : undefined),
    [hasCustom, applied.from, applied.to],
  );

  function apply(patch: Partial<Filters>) {
    void refresh({ ...applied, ...patch });
  }

  function onRangeChange(value: string) {
    if (value === CUSTOM) {
      setCalendarOpen(true);
      return;
    }
    // Picking a preset clears any custom window so `hours` governs again.
    apply({ hours: Number(value), from: '', to: '' });
  }

  function onCustomRange(next: DateRange) {
    if (!next?.start || !next?.end) return;
    apply({ from: new Date(next.start).toISOString(), to: new Date(next.end).toISOString() });
    setCalendarOpen(false);
  }

  const customLabel = hasCustom
    ? `${formatDate(applied.from, { month: 'short', year: undefined })} → ${formatDate(applied.to, { month: 'short', year: undefined })}`
    : 'Custom range…';

  return (
    <div className="my-0.5 mb-4 flex flex-wrap items-center gap-2">
      <Chip><span>Project</span> <b className="font-medium text-[var(--color-text-primary)]">{project?.name || '—'}</b></Chip>

      {/* Time range: presets + a custom-range option that opens the calendar. */}
      <div className="flex items-center gap-1">
        <Selector
          label="Time range"
          isLabelHidden
          size="sm"
          value={rangeValue}
          onChange={onRangeChange}
          options={[
            ...RANGE_PRESETS.map((p) => ({ value: String(p.hours), label: p.label })),
            { value: CUSTOM, label: hasCustom ? customLabel : 'Custom range…' },
          ]}
        />
        <Popover
          isOpen={calendarOpen}
          onOpenChange={setCalendarOpen}
          placement="below"
          alignment="start"
          label="Pick custom date range"
          content={<Calendar mode="range" numberOfMonths={2} value={range} onChange={onCustomRange} />}
        >
          <button
            className={`grid h-8 w-8 place-items-center rounded-md border border-[var(--color-border)] bg-transparent transition-colors hover:bg-[var(--color-background-surface)] ${hasCustom ? 'text-primary' : 'text-[var(--color-text-secondary)]'}`}
            aria-label="Pick custom date range"
            title="Pick custom date range"
          >
            <CalendarRange size={15} />
          </button>
        </Popover>
      </div>

      {showEventType ? (
        <Selector
          label="Event type"
          isLabelHidden
          size="sm"
          placeholder="All types"
          value={applied.event_type || 'all'}
          onChange={(v) => apply({ event_type: v === 'all' ? '' : v })}
          options={[{ value: 'all', label: 'All types' }, ...eventTypes.map((t) => ({ value: t, label: t }))]}
        />
      ) : null}

      {showErrors ? (
        <button
          className={`inline-flex h-8 items-center gap-1.5 rounded-md border px-2.5 text-xs transition-colors ${applied.error_only ? 'border-transparent bg-[color-mix(in_srgb,var(--danger)_16%,transparent)] text-danger' : 'border-[var(--color-border)] bg-transparent text-[var(--color-text-secondary)] hover:bg-[var(--color-background-surface)]'}`}
          aria-pressed={applied.error_only}
          onClick={() => apply({ error_only: !applied.error_only })}
        >
          <TriangleAlert size={13} /> Errors only
        </button>
      ) : null}

      {extra}

      {dirty ? (
        <button
          className="ms-auto inline-flex h-8 items-center gap-1.5 rounded-md border border-transparent bg-transparent px-2.5 text-xs text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-surface)] hover:text-[var(--color-text-primary)]"
          onClick={() => void refresh({ ...defaultFilters })}
          title="Clear all filters"
        >
          <RotateCcw size={13} /> Reset
        </button>
      ) : null}
    </div>
  );
}
