'use client';

import { useMemo } from 'react';
import type { Person } from '@/lib/api';
import { formatCompact, formatNumber, formatPercent, formatRelative } from '@/lib/format';
import { platformLabel } from '@/lib/platform';
import { useActivity, usePersons } from '@/modules/app/hooks';
import { Chart } from '@/modules/shared/components/charts';
import { AppShell } from '@/modules/shared/components/app-shell';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { FilterBar } from '@/modules/shared/components/filter-bar';
import { Loading, Panel, StatsStrip } from '@/modules/shared/components/signal-primitives';

// personLabel is the display identity used in the table and for global search:
// a real name/email when we have it, otherwise the truncated distinct id.
function personLabel(p: Person): string {
  return p.name || p.email || p.distinct_id;
}

// traitText renders a $set/$set_once value compactly. Values arrive as parsed
// JSON, so strings are already unquoted; objects/arrays fall back to JSON.
function traitText(value: unknown): string {
  if (value == null) return '';
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

// TraitChips shows a person's merged profile properties (the $set/$set_once
// traits the persons API now returns). Capped so a wide profile doesn't blow out
// the row; the rest are summarised as "+N".
function TraitChips({ traits }: { traits?: Record<string, unknown> }) {
  const entries = traits ? Object.entries(traits) : [];
  if (entries.length === 0) return <span className="text-[var(--color-text-secondary)]">—</span>;
  const shown = entries.slice(0, 3);
  const rest = entries.length - shown.length;
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {shown.map(([k, v]) => (
        <span
          key={k}
          title={`${k}: ${traitText(v)}`}
          className="inline-flex max-w-[220px] items-center gap-1 rounded-md bg-[var(--color-background-muted)] px-2 py-0.5 text-2xs text-[var(--color-text-secondary)]"
        >
          <span className="shrink-0 whitespace-nowrap font-medium text-[var(--color-text-primary)]">{k}</span>
          <span className="min-w-0 truncate">{traitText(v)}</span>
        </span>
      ))}
      {rest > 0 ? <span className="text-2xs text-[var(--color-text-secondary)]">+{rest}</span> : null}
    </span>
  );
}

export function PersonsPage() {
  const { persons, focusPerson } = usePersons();
  // Self-hiding for the same reason the filter bar's facet is: a single-app
  // product would get a column that only ever repeats itself. It earns its width
  // the moment a second app starts sending.
  const { summary } = useActivity();
  const showApps = (summary?.platforms?.length ?? 0) > 1;

  const columns = useMemo<DataColumn<Person>[]>(() => [
    {
      key: 'person',
      header: 'Person',
      searchValue: personLabel,
      sortValue: personLabel,
      renderCell: (p) =>
        p.name || p.email
          ? <span className="font-medium">{p.name || p.email}</span>
          : <span className="font-mono text-[var(--color-text-secondary)]">{p.distinct_id.slice(0, 16)}</span>,
    },
    {
      key: 'traits',
      header: 'Traits',
      renderCell: (p) => <TraitChips traits={p.traits} />,
    },
    ...(showApps ? [{
      key: 'platforms',
      header: 'Apps',
      // Two apps on one row is the whole point: it says this is one human who
      // used the site and then the app, not two anonymous halves. '—' means the
      // classifier could not tell, never "web".
      renderCell: (p: Person) => p.platforms?.length
        ? (
          <span className="inline-flex flex-wrap gap-1">
            {p.platforms.map((platform) => (
              <span
                key={platform}
                className="inline-flex items-center rounded-md bg-[var(--color-background-muted)] px-2 py-0.5 text-2xs text-[var(--color-text-secondary)]"
              >
                {platformLabel(platform)}
              </span>
            ))}
          </span>
        )
        : <span className="text-[var(--color-text-secondary)]">—</span>,
    }] : []),
    {
      key: 'last_event_name',
      header: 'Last event',
      renderCell: (p) => <span className="text-[var(--color-text-secondary)]">{p.last_event_name || '—'}</span>,
    },
    {
      key: 'event_count',
      header: 'Events',
      renderCell: (p) => <span className="font-mono tabular-nums">{formatCompact(p.event_count)}</span>,
    },
    {
      key: 'sessions',
      header: 'Sessions',
      renderCell: (p) => <span className="font-mono tabular-nums">{formatCompact(p.sessions)}</span>,
    },
    {
      key: 'last_seen',
      header: 'Last seen',
      sortValue: (p) => p.last_seen,
      renderCell: (p) => <span className="font-mono text-[var(--color-text-secondary)]">{formatRelative(p.last_seen)}</span>,
    },
  ], [showApps]);

  if (!persons) {
    return (
      <AppShell active="traffic" title="People" sub="Who is behind the events — identified and anonymous.">
        <Loading label="Loading people…" />
      </AppShell>
    );
  }

  const identifiedShare = persons.total ? (persons.identified / persons.total) * 100 : 0;

  return (
    <AppShell active="traffic" title="People" sub="Who is behind the events — identified and anonymous.">
      <FilterBar showEventType={false} showErrors={false} />
      <StatsStrip stats={[
        { label: 'People', value: formatNumber(persons.total) },
        { label: 'Identified', value: formatNumber(persons.identified), tone: 'success' },
        { label: 'Anonymous', value: formatNumber(persons.anonymous) },
        { label: 'Identified %', value: formatPercent(identifiedShare) },
      ]} />
      <div>
        <Panel title="Active people">
          {persons.active_timeline.length === 0 ? (
            <div className="flex h-[140px] items-center justify-center text-sm text-[var(--color-text-secondary)]">
              No active people yet
            </div>
          ) : (
            <Chart spec={{
              type: 'area',
              x: persons.active_timeline.map((p) => p.hour),
              series: [{ name: 'Active people', data: persons.active_timeline.map((p) => p.count) }],
              height: 200,
              // A count of discrete people — no smoothed curve through fractional
              // values, and the y-axis should only ever print whole numbers.
              smooth: false,
              integerY: true,
            }} />
          )}
        </Panel>
      </div>
      <DataTable
        title="People"
        columns={columns}
        data={persons.persons}
        idKey="distinct_id"
        searchPlaceholder="Search people…"
        pageSize={20}
        onRowClick={(p) => void focusPerson(p.distinct_id)}
        emptyMessage="No people in this range."
      />
    </AppShell>
  );
}
