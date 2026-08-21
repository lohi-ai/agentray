'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { ChevronDown, ChevronUp, LayoutGrid, Plus, Sparkles } from 'lucide-react';
import { useAuthStore } from '@/lib/app-state';
import type { Chart } from '@/lib/api';
import { formatCompact } from '@/lib/format';
import { firstSessionNotice, firstValuePath, settingsPath } from '@/lib/ia';
import { useActivity, useDashboards, useEventNames, useProjectAccess } from '@/modules/app/hooks';
import { useWorkspaceModels } from '@/modules/agent/hooks';
import { Callout } from '@/modules/shared/components/signal-primitives';
import { AppShell } from '@/modules/shared/components/app-shell';
import { RelatedSurfacesLabel } from '@/modules/shared/components/related-surfaces';
import { FilterBar } from '@/modules/shared/components/filter-bar';
import { PromptDialog } from '@/modules/shared/components/modal';
import { Button, EmptyState, Intro, Loading, StatsStrip } from '@/modules/shared/components/signal-primitives';
import { Selector } from '@astryxdesign/core/Selector';
import { IconButton } from '@astryxdesign/core/IconButton';
import { ChartCard } from './chart-card';
import { ChartEditor } from './chart-editor';
import { DailyReadout } from './daily-readout';
import { FirstEventQuickstart } from './first-event-quickstart';

// spanClass maps a chart's column span onto the 3-column board grid; it collapses
// to a single column on narrow screens where the grid itself is one column.
function spanClass(span: number): string {
  if (span >= 3) return 'col-span-3 [@media(max-width:980px)]:col-span-1';
  if (span === 2) return 'col-span-2 [@media(max-width:980px)]:col-span-1';
  return '';
}

export function DashboardPage() {
  const projectID = useAuthStore((s) => s.project?.id);
  const projectName = useAuthStore((s) => s.project?.name);
  const router = useRouter();
  const { dashboards, selectedDashboard, charts, loading, setSelectedDashboardID, createDashboard, saveChart, deleteChart, reorderCharts } = useDashboards();
  const { summary } = useActivity();
  const { names: eventNames, loading: catalogLoading } = useEventNames();
  const { models, modelsLoading } = useWorkspaceModels();
  const catalogReady = !catalogLoading && !!projectID;
  const firstValue = firstValuePath({ eventNames, catalogReady });
  const sessionNotice = firstSessionNotice({
    eventNames,
    catalogReady,
    hasModelKey: modelsLoading ? undefined : !!models?.has_key,
  });
  // Whether this board can be edited at all. A visitor reading the shared demo
  // owns none of these charts, and the API refuses every write to them.
  const access = useProjectAccess();
  // Provenance, not decoration: a stranger arrives here from the tour, and the
  // subtitle has to say whose board they are reading.
  const boardSubtitle = access.isDemo
    ? `Built on ${projectName}’s own traffic. Your board looks like this once your events are arriving.`
    : 'The weekly check: what moved, and whether the last test worked.';

  // Only stats we actually have. A tile that renders "—" because the summary is
  // still loading is worse than one fewer tile, so the strip is built from what
  // resolved and hidden entirely when nothing did.
  const headlineStats = summary
    ? [
        { label: 'Events', value: formatCompact(summary.event_count) },
        { label: 'People', value: formatCompact(summary.distinct_users) },
        { label: 'Sessions', value: formatCompact(summary.sessions) },
        { label: 'Agent events', value: formatCompact(summary.agent_events) },
      ]
    : [];

  const [dialog, setDialog] = useState<'view' | null>(null);
  // null = closed; 'new' = create; a Chart = edit that chart.
  const [editing, setEditing] = useState<Chart | 'new' | null>(null);
  // Local mirror of the server chart order so drag feels instant. We resync
  // during render (React's recommended pattern over an effect) whenever the
  // server list changes — add/delete, dashboard switch, or a persisted reorder —
  // keyed on the id+order signature so a stable list never triggers a reset.
  const chartsKey = charts.map((c) => c.id).join(',');
  const [order, setOrder] = useState<Chart[]>(charts);
  const [orderKey, setOrderKey] = useState(chartsKey);
  if (chartsKey !== orderKey) {
    setOrderKey(chartsKey);
    setOrder(charts);
  }

  function onReorder(next: Chart[]) {
    setOrder(next);
    if (selectedDashboard) void reorderCharts(selectedDashboard.id, next.map((c) => c.id));
  }

  // Astryx has no drag-and-drop primitive, so reordering is explicit: each card
  // carries move-earlier / move-later controls that swap it with its neighbour
  // and persist the new order. Cheaper and more accessible than a DnD library.
  function moveChart(index: number, dir: -1 | 1) {
    const j = index + dir;
    if (j < 0 || j >= order.length) return;
    const next = [...order];
    [next[index], next[j]] = [next[j], next[index]];
    onReorder(next);
  }

  function onAddChart() {
    if (!selectedDashboard) { setDialog('view'); return; }
    setEditing('new');
  }

  // Hand off to the agent chat to build a chart. The agent writes & runs the SQL
  // and pins the chart itself via its create_chart tool — no special endpoint. The
  // composer is prefilled with a lead-in the user finishes describing.
  function onAskAI() {
    router.push(`/chat?q=${encodeURIComponent('Build a chart for my dashboard that shows ')}`);
  }

  const selector = dashboards.length > 0 ? (
    <Selector
      label="Select dashboard"
      isLabelHidden
      size="sm"
      placeholder="Select dashboard"
      value={selectedDashboard?.id}
      onChange={(id) => void setSelectedDashboardID(id)}
      options={dashboards.map((d) => ({ value: d.id, label: d.name }))}
    />
  ) : null;

  return (
    <AppShell active="dashboards">
      {dialog === 'view' ? (
        <PromptDialog
          title="New view"
          label="View name"
          placeholder="e.g. Weekly growth"
          submitLabel="Create view"
          onSubmit={(name) => void createDashboard({ name, description: '' })}
          onClose={() => setDialog(null)}
        />
      ) : null}
      {editing !== null ? (
        <ChartEditor
          chart={editing === 'new' ? undefined : editing}
          onSubmit={(input, chartID) => void saveChart(input, chartID)}
          onClose={() => setEditing(null)}
        />
      ) : null}
      <Intro
        title="Dashboards"
        sub={boardSubtitle}
        action={<>
          {/* Asking stays live for a demo viewer — it is the one thing a viewer
              is here to do, and the API allows it on purpose. Building and
              editing do not. */}
          <Button variant="outline" icon={<LayoutGrid size={15} />} onClick={() => setDialog('view')} disabled={!access.canWrite} tooltip={access.reason || undefined}>New view</Button>
          <Button variant="agent" icon={<Sparkles size={15} />} onClick={onAskAI}>Ask the agent</Button>
          <Button variant="primary" icon={<Plus size={15} />} onClick={onAddChart} disabled={!access.canWrite} tooltip={access.reason || undefined}>Add chart</Button>
        </>}
      />
      <div className="mb-3"><RelatedSurfacesLabel parentHref="/dashboard" /></div>
      <FilterBar extra={selector} />

      {/* Headline numbers in one strip above the board, never scattered across
          the chart cards (DESIGN.md §Data Trust). */}
      {headlineStats.length ? <StatsStrip stats={headlineStats} /> : null}

      <FirstEventQuickstart />

      {sessionNotice?.kind === 'noticed' ? (
        <div className="mb-4">
          <Callout
            tone="growth"
            icon={<Sparkles size={16} />}
            label="What I noticed"
            title={sessionNotice.title}
            detail={sessionNotice.detail}
            action={<Button variant="agent" size="sm" icon={<Sparkles size={14} />} onClick={() => router.push(`/chat?q=${encodeURIComponent(sessionNotice.ask)}`)}>Write that down</Button>}
          />
        </div>
      ) : null}

      <DailyReadout />

      {loading && charts.length === 0 ? (
        <Loading label="Loading dashboard…" />
      ) : order.length === 0 ? (
        // Empty board: a full-width, centered hero empty state rather than a tile
        // cramped into one of three columns. Leads with both ways to fill it.
        <div className="rounded-xl border border-dashed border-[var(--color-border)] bg-[var(--color-background-card)] py-14">
          <EmptyState
            icon={<LayoutGrid size={26} />}
            title={firstValue.showFirstEvent ? 'Nothing to chart yet' : 'No saved views yet'}
            detail={firstValue.showFirstEvent
              ? 'Send a first event, then ask Growth Lead what to look at.'
              : 'Ask Growth Lead to pin a chart, or add one yourself.'}
            action={(
              <div className="flex items-center justify-center gap-2">
                {firstValue.showFirstAsk
                  ? <Button variant="agent" size="sm" icon={<Sparkles size={14} />} onClick={() => router.push('/chat')}>Ask a first question</Button>
                  : <Button variant="primary" size="sm" icon={<Plus size={14} />} onClick={onAddChart} disabled={!access.canWrite} tooltip={access.reason || undefined}>Add chart</Button>}
                {firstValue.showFirstAsk
                  ? <Button variant="outline" size="sm" icon={<Plus size={14} />} onClick={onAddChart} disabled={!access.canWrite} tooltip={access.reason || undefined}>Add chart</Button>
                  : <Button variant="agent" size="sm" icon={<Sparkles size={14} />} onClick={onAskAI}>Ask AI</Button>}
              </div>
            )}
          />
        </div>
      ) : (
        <div className="grid gap-3.5 grid-cols-3 [@media(max-width:980px)]:grid-cols-1">
          {order.map((chart, i) => (
            <div key={chart.id} className={spanClass(chart.col_span)}>
              {/* A reader who cannot write this board gets no edit, no delete,
                  and no reorder handle — every one of those is a 403 waiting to
                  happen on somebody else's charts. */}
              <ChartCard
                chart={chart}
                summary={summary}
                projectID={projectID}
                onEdit={access.canWrite ? () => setEditing(chart) : undefined}
                onDelete={access.canWrite ? () => void deleteChart(chart.id) : undefined}
                handle={access.canWrite ? (
                  <span className="-ms-1 me-1 inline-flex items-center">
                    <IconButton label="Move chart earlier" variant="ghost" size="sm" icon={<ChevronUp size={14} />} isDisabled={i === 0} onClick={() => moveChart(i, -1)} />
                    <IconButton label="Move chart later" variant="ghost" size="sm" icon={<ChevronDown size={14} />} isDisabled={i === order.length - 1} onClick={() => moveChart(i, 1)} />
                  </span>
                ) : undefined}
              />
            </div>
          ))}
          {/* Add-tile: a dashed card that aligns with the chart cards and reads as
              an affordance — the whole tile is the click target. Absent entirely
              for a reader who may not write here; a disabled dashed tile would
              just be a hole in the grid. */}
          {access.canWrite ? (
            <button
              type="button"
              onClick={onAddChart}
              className="flex min-h-[220px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-[var(--color-border)] bg-transparent text-[var(--color-text-secondary)] transition-[background,border-color,color] duration-[var(--fast)] ease-[var(--ease)] hover:border-primary hover:bg-[var(--color-background-card)] hover:text-primary"
            >
              <Plus size={22} />
              <span className="text-[13px] font-medium">Add another chart</span>
            </button>
          ) : null}
        </div>
      )}
    </AppShell>
  );
}
