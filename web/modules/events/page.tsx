'use client';

import { useEffect, useMemo, useState } from 'react';
import { useRouter } from 'next/navigation';
import { AlertTriangle, Pause, Play, Search } from 'lucide-react';
import { TextInput } from '@astryxdesign/core/TextInput';
import type { Event } from '@/lib/api';
import { useFiltersStore } from '@/lib/app-state';
import { formatCompact, formatCost, formatLatency, formatRelative } from '@/lib/format';
import { useFilters, useLiveEvents } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { DataTable, type DataColumn } from '@/modules/shared/components/data-table';
import { EventNameSelect } from '@/modules/shared/components/event-name-picker';
import { FilterBar } from '@/modules/shared/components/filter-bar';
import { Button, Intro, Loading, StatsStrip, StatusPill } from '@/modules/shared/components/signal-primitives';

export function EventsPage() {
  const router = useRouter();
  const { filters, setFilters, refresh } = useFilters();
  const appliedEventName = useFiltersStore((s) => s.appliedFilters.event_name);
  const [search, setSearch] = useState(filters.search);
  // The table polls the raw stream so it reads as realtime; the toggle lets people
  // pause when they want a stable view to read or inspect.
  const [live, setLive] = useState(true);
  // Client-side lens over the loaded window: surface only events flagged as off
  // the tracking plan. Derived from the already-fetched set (like the error/agent
  // counts below), so it needs no extra request.
  const [unplannedOnly, setUnplannedOnly] = useState(false);
  const { explorer, loading, fetching, updatedAt } = useLiveEvents(live);

  // Tick once a second so the "updated …" label and the relative timestamps in
  // the table keep counting up between refetches — that motion is what makes the
  // stream feel live even on a quiet project.
  const [, setNow] = useState(0);
  useEffect(() => {
    if (!live) return;
    const t = setInterval(() => setNow((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, [live]);

  function applySearch() {
    const next = { ...filters, search };
    setFilters(next);
    void refresh(next);
  }

  // Picking a name from the catalog narrows the stream to that name (or clears the
  // filter when "All events" is chosen). No free text — people pick a known name.
  function applyEventName(name: string) {
    const next = { ...filters, event_name: name };
    setFilters(next);
    void refresh(next);
  }

  const columns = useMemo<DataColumn<Event>[]>(() => [
    {
      key: 'event_name',
      header: 'Event',
      renderCell: (e) => (
        <span className="inline-flex items-center gap-1.5">
          {e.event_name}
          {e.tool_name ? <span className="text-[var(--color-text-secondary)]"> · {e.tool_name}</span> : null}
          {e.is_unplanned ? (
            <span
              title="Not in this project's established tracking plan — likely a typo or newly-shipped, un-documented event."
              className="inline-flex items-center rounded-md bg-[color:color-mix(in_srgb,var(--color-warning)_16%,transparent)] px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-[color:var(--color-warning)]"
            >
              unplanned
            </span>
          ) : null}
        </span>
      ),
    },
    {
      key: 'event_type',
      header: 'Type',
      renderCell: (e) => <span className="text-[var(--color-text-secondary)]">{e.event_type}</span>,
    },
    {
      key: 'distinct_id',
      header: 'Person',
      renderCell: (e) => <span className="font-mono text-[var(--color-text-secondary)]">{e.distinct_id.slice(0, 12)}</span>,
    },
    {
      key: 'tokens',
      header: 'Tokens',
      sortValue: (e) => (e.tokens_input ?? 0) + (e.tokens_output ?? 0),
      renderCell: (e) => <span className="font-mono tabular-nums">{formatCompact((e.tokens_input ?? 0) + (e.tokens_output ?? 0))}</span>,
    },
    {
      key: 'cost',
      header: 'Cost',
      sortValue: (e) => e.cost_usd ?? 0,
      renderCell: (e) => <span className="font-mono tabular-nums">{formatCost(e.cost_usd ?? 0)}</span>,
    },
    {
      key: 'latency',
      header: 'Latency',
      sortValue: (e) => e.latency_ms ?? 0,
      renderCell: (e) => <span className="font-mono tabular-nums">{formatLatency(e.latency_ms ?? 0)}</span>,
    },
    {
      key: 'timestamp',
      header: 'When',
      renderCell: (e) => <span className="font-mono text-[var(--color-text-secondary)]">{formatRelative(e.timestamp)}</span>,
    },
  ], []);

  const header = (
    <div className="flex items-center gap-2">
      <EventNameSelect value={filters.event_name} onChange={applyEventName} placeholder="All events" className="w-[230px]" />
      <TextInput
        label="Search events"
        isLabelHidden
        size="sm"
        startIcon={Search}
        value={search}
        placeholder="Search events…"
        onChange={(v) => setSearch(v)}
        onEnter={applySearch}
        width={200}
      />
      <Button
        variant={unplannedOnly ? 'primary' : 'outline'}
        size="sm"
        icon={<AlertTriangle size={14} />}
        onClick={() => setUnplannedOnly((v) => !v)}
      >
        Unplanned only
      </Button>
      <Button
        variant={live ? 'outline' : 'primary'}
        size="sm"
        icon={live ? <Pause size={14} /> : <Play size={14} />}
        onClick={() => setLive((v) => !v)}
      >
        {live ? 'Pause' : 'Go live'}
      </Button>
    </div>
  );

  // The live status reads on the FilterBar: a pulsing dot + how fresh the data is.
  const liveStatus = (
    <span className="inline-flex items-center gap-2">
      <StatusPill status={live ? 'working' : 'paused'} label={live ? 'Live' : 'Paused'} grow={false} />
      {updatedAt ? (
        <span className="text-[11px] text-[var(--color-text-secondary)]">
          {fetching && live ? 'updating…' : `updated ${formatRelative(new Date(updatedAt).toISOString())}`}
        </span>
      ) : null}
    </span>
  );

  if (loading && !explorer) {
    return (
      <AppShell active="traffic">
        <Intro title="Events" sub="Raw event stream across people, sessions, and agents." action={header} />
        <FilterBar extra={liveStatus} />
        <Loading label="Loading events…" />
      </AppShell>
    );
  }

  const events = explorer?.events ?? [];
  const errors = events.filter((e) => e.is_error).length;
  const agentEv = events.filter((e) => e.event_type === 'agent').length;
  const unplanned = events.filter((e) => e.is_unplanned).length;
  const visibleEvents = unplannedOnly ? events.filter((e) => e.is_unplanned) : events;

  const filterExtra = (
    <span className="inline-flex items-center gap-2">
      {liveStatus}
      {appliedEventName ? (
        <span className="inline-flex h-8 items-center gap-1.5 rounded-md bg-[var(--color-background-muted)] px-2.5 text-xs text-[var(--color-text-secondary)]"><span>Event</span> <b className="font-medium text-[var(--color-text-primary)]">{appliedEventName}</b></span>
      ) : null}
    </span>
  );

  return (
    <AppShell active="traffic">
      <Intro title="Events" sub="Raw event stream across people, sessions, and agents." action={header} />
      <FilterBar extra={filterExtra} />
      <StatsStrip stats={[
        { label: 'Events', value: formatCompact(events.length) },
        { label: 'Errors', value: String(errors), tone: errors ? 'danger' : undefined },
        { label: 'Unplanned', value: String(unplanned), tone: unplanned ? 'warning' : undefined },
        { label: 'Agent events', value: formatCompact(agentEv), tone: 'agent' },
      ]} />
      <DataTable
        title={unplannedOnly ? 'Unplanned events' : 'Recent events'}
        columns={columns}
        data={visibleEvents}
        pageSize={20}
        onRowClick={(e) => { if (e.session_id) router.push(`/replay?session=${encodeURIComponent(e.session_id)}`); }}
        rowClassName={(e) => (e.is_error ? 'text-[color:var(--danger)]' : undefined)}
        emptyMessage={unplannedOnly ? 'No unplanned events in this window — instrumentation matches the plan.' : 'No events match these filters.'}
      />
    </AppShell>
  );
}
