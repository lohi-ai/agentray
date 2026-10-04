'use client';

import Link from 'next/link';
import { Database, MessageSquare } from 'lucide-react';
import type { Chart, Dashboard, Filters, QueryMeta } from '@/lib/api';
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger, Button, Card, CardContent } from '@/lib/lohi-ui';
import { StatusPill } from '@/modules/shared/components/lohi-evidence-primitives';
import type { ReadinessSync } from '@/modules/app/hooks/connectors';
import { evidenceTime } from '@/modules/settings/source-readiness';

export type ChartEvidenceStatus = 'loading' | 'ready' | 'empty' | 'unsupported' | 'error' | 'capacity' | 'non_plottable';
export type ChartEvidence = {
  status: ChartEvidenceStatus;
  filterKey: string;
  sql?: string;
  range?: { kind: 'applied' | 'fixed'; label: string; from?: string; to?: string };
  definition?: string;
  unit?: string;
  cohortEligibility?: string;
  meta?: QueryMeta;
};
export type EvidenceState = 'ready' | 'syncing' | 'empty' | 'stale' | 'error' | 'immature' | 'read-only-denied';

export type BoardEvidence = {
  definition: string;
  unit: string;
  range: string;
  timezone: string;
  coverage: string;
  partialDay: string;
  cohortEligibility: string;
  recipeRef: string;
  limitation: string;
};

const LABELS: Record<string, keyof BoardEvidence> = {
  definition: 'definition', metric_definition: 'definition', definition_version: 'definition',
  unit: 'unit', exact_range: 'range', range: 'range', timezone: 'timezone',
  coverage: 'coverage', source_coverage: 'coverage', partial_day: 'partialDay',
  cohort_eligibility: 'cohortEligibility', cohort: 'cohortEligibility', recipe_ref: 'recipeRef', query_ref: 'recipeRef',
  limitation: 'limitation', limitations: 'limitation',
};

export function parseBoardEvidence(description: string, browserTimezone = 'UTC'): BoardEvidence {
  const result: BoardEvidence = {
    definition: 'Definition not supplied', unit: 'Not declared', range: 'Applied dashboard range', timezone: browserTimezone,
    coverage: 'Coverage not verified', partialDay: 'Partial-day status not declared', cohortEligibility: 'Eligibility not declared',
    recipeRef: 'Query reference pending', limitation: 'No limitation was supplied with this saved board.',
  };
  const trimmed = description.trim();
  if (!trimmed) return result;
  try {
    const object = JSON.parse(trimmed) as Record<string, unknown>;
    for (const [key, value] of Object.entries(object)) {
      const target = LABELS[key.trim().toLowerCase()];
      if (target && (typeof value === 'string' || typeof value === 'number')) result[target] = String(value);
    }
  } catch {
    for (const part of trimmed.split(/[\n;|]+/)) {
      const match = part.match(/^\s*([a-zA-Z][a-zA-Z0-9 _-]{1,40})\s*[:=]\s*(.+?)\s*$/);
      if (!match) continue;
      const target = LABELS[match[1].trim().toLowerCase().replace(/\s+/g, '_')];
      if (target) result[target] = match[2].trim();
    }
  }
  return result;
}

export function evidenceFilterKey(filters: Filters): string {
  return JSON.stringify([filters.from || '', filters.to || '', filters.hours]);
}

export function resolveEvidenceState({ loading, denied, charts, syncs, chartStatuses, cohortEligibility }: { loading: boolean; denied: boolean; charts: readonly Chart[]; syncs: readonly ReadinessSync[]; chartStatuses: readonly ChartEvidenceStatus[]; cohortEligibility?: string }): EvidenceState {
  if (denied) return 'read-only-denied';
  if (chartStatuses.some((status) => status === 'error' || status === 'unsupported' || status === 'capacity' || status === 'non_plottable')) return 'error';
  if (syncs.some((s) => s.readiness?.state === 'error' || s.readiness?.state === 'incomplete')) return 'error';
  if (syncs.some((s) => s.readiness?.state === 'stale')) return 'stale';
  if (loading || chartStatuses.some((status) => status === 'loading') || syncs.some((s) => s.readiness?.state === 'syncing')) return 'syncing';
  if (charts.length === 0 || chartStatuses.length === 0 || chartStatuses.some((status) => status === 'empty') || syncs.length === 0 || syncs.some((s) => !s.readiness || s.readiness.state === 'not_configured')) return 'empty';
  if (/under\s*14|not ready|immature/i.test(cohortEligibility ?? '')) return 'immature';
  return 'ready';
}

const STATE_COPY: Record<EvidenceState, { label: string; detail: string; pill: string }> = {
  ready: { label: 'Evidence ready', detail: 'Queryable sources and execution evidence are available.', pill: 'ready' },
  syncing: { label: 'Evidence updating', detail: 'Previous complete evidence remains timestamped while the new range is checked.', pill: 'working' },
  empty: { label: 'Evidence unavailable', detail: 'Connect and complete a source sync to verify this view.', pill: 'idle' },
  stale: { label: 'Evidence is stale', detail: 'Do not treat the previous result as current; its completion time is shown below.', pill: 'attention' },
  error: { label: 'Evidence incomplete', detail: 'The current source or query result is incomplete. No current conclusion is shown.', pill: 'error' },
  immature: { label: 'Cohort not ready', detail: 'The current query reports that the cohort has not reached its maturity window.', pill: 'immature' },
  'read-only-denied': { label: 'Read-only evidence', detail: 'Saved definitions remain readable, but source readiness is not granted to this credential.', pill: 'denied' },
};

function sourceBindings(chartEvidence: ChartEvidence | undefined): NonNullable<QueryMeta['serving_data_watermark']>['sources'] {
  const watermark = chartEvidence?.meta?.serving_data_watermark;
  if (!watermark || watermark.sources_truncated || !chartEvidence.sql) return [];
  return watermark.sources.filter((source) => {
    const escaped = source.table.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    return new RegExp(`(?:['\"]${escaped}['\"]|\\b(?:from|join)\\s+${escaped}\\b)`, 'i').test(chartEvidence.sql!);
  });
}

function verifiedCoverage(chartEvidence: ChartEvidence | undefined, bindings: NonNullable<QueryMeta['serving_data_watermark']>['sources']): string {
  const meta = chartEvidence?.meta;
  const range = chartEvidence?.range;
  if (!meta || meta.result_completeness !== 'complete' || range?.kind !== 'applied' || !range.from || !range.to || bindings.length === 0) return 'Coverage not verified';
  const from = Date.parse(range.from);
  const to = Date.parse(range.to);
  const covered = Number.isFinite(from) && Number.isFinite(to) && bindings.every((source) => {
    const started = Date.parse(source.capture_started_at ?? '');
    const finished = Date.parse(source.capture_finished_at ?? '');
    return Number.isFinite(started) && Number.isFinite(finished) && started <= from && finished >= to;
  });
  return covered ? 'Verified for applied range' : 'Coverage not verified';
}

function partialDay(range: ChartEvidence['range']): string {
  if (!range?.from || !range.to) return 'Partial-day status not verified';
  const from = new Date(range.from);
  const to = new Date(range.to);
  if (!Number.isFinite(from.getTime()) || !Number.isFinite(to.getTime())) return 'Partial-day status not verified';
  const isMidnight = (date: Date) => date.getUTCHours() === 0 && date.getUTCMinutes() === 0 && date.getUTCSeconds() === 0 && date.getUTCMilliseconds() === 0;
  return isMidnight(from) && isMidnight(to) ? 'No (UTC)' : 'Yes (UTC)';
}

function pendingRange(filters: Filters): string {
  return filters.from && filters.to
    ? `${filters.from} to ${filters.to} (pending execution)`
    : `Last ${filters.hours} hours (pending execution)`;
}

export function EvidencePanel({ dashboard, charts, syncs, readinessLoading, readinessDenied, chartEvidence, appliedFilters }: { dashboard: Dashboard | null; charts: Chart[]; syncs: ReadinessSync[]; readinessLoading: boolean; readinessDenied: boolean; chartEvidence: Record<string, ChartEvidence>; appliedFilters: Filters }) {
  const browserTimezone = typeof Intl !== 'undefined' ? Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC' : 'UTC';
  const savedEvidence = parseBoardEvidence(dashboard?.description ?? '', browserTimezone);
  const filterKey = evidenceFilterKey(appliedFilters);
  const sqlCharts = charts.filter((chart) => !!chart.sql);
  const selectedChart = sqlCharts[0];
  const selectedEvidence = selectedChart && chartEvidence[selectedChart.id]?.filterKey === filterKey ? chartEvidence[selectedChart.id] : undefined;
  const chartStatuses = sqlCharts.map((chart) => chartEvidence[chart.id]?.filterKey === filterKey ? chartEvidence[chart.id].status : 'loading');
  const state = resolveEvidenceState({ loading: readinessLoading, denied: readinessDenied, charts, syncs, chartStatuses, cohortEligibility: selectedEvidence?.cohortEligibility });
  const copy = STATE_COPY[state];
  const bindings = sourceBindings(selectedEvidence);
  const sourceTables = [...new Set(bindings.map((source) => source.table))];
  const lastComplete = syncs.map((sync) => sync.readiness?.last_complete_at).filter((value): value is string => !!value).sort()[0];
  const freshness = [lastComplete ? `Source last complete ${evidenceTime(lastComplete)}` : null, selectedEvidence?.meta?.executed_at ? `Query executed ${evidenceTime(selectedEvidence.meta.executed_at)}` : null].filter(Boolean).join(' · ') || 'Freshness not verified';
  const queryRef = selectedEvidence?.meta?.query_ref || 'Query reference pending';
  const limitation = selectedEvidence?.meta?.availability_reason
    ? selectedEvidence.meta.availability_reason.replace(/[_-]+/g, ' ')
    : selectedEvidence && selectedEvidence.status !== 'ready'
      ? `Current query status: ${selectedEvidence.status.replace(/_/g, ' ')}`
      : savedEvidence.limitation;
  const definition = selectedEvidence?.definition || selectedChart?.name || 'Definition not supplied';
  const unit = selectedEvidence?.unit || (selectedChart?.y_field ? `Unit not declared for ${selectedChart.y_field}` : 'Not declared');
  const range = selectedEvidence?.range?.label || pendingRange(appliedFilters);
  const timezone = selectedEvidence?.range?.kind === 'applied'
    ? 'UTC'
    : selectedEvidence?.range
      ? 'Timezone not verified for fixed query'
      : browserTimezone;
  const coverage = verifiedCoverage(selectedEvidence, bindings);
  const cohortEligibility = selectedEvidence?.cohortEligibility || 'Eligibility not verified for this query';

  return (
    <section aria-labelledby="board-evidence-title" aria-live="polite">
      <Card>
        <CardContent>
          <div className="mb-3 flex flex-wrap items-start gap-3">
            <div className="min-w-0 flex-1"><h2 id="board-evidence-title" className="m-0 text-base font-semibold">Evidence</h2><p className="m-0 mt-1 text-sm text-[var(--lohi-muted-foreground)]">{copy.detail}</p></div>
            <StatusPill status={copy.pill} label={copy.label} grow={false} pulse={state === 'syncing'} />
          </div>
          <dl className="lohi-evidence-grid">
            <div className="lohi-evidence-fact"><dt>Timezone</dt><dd>{timezone}</dd></div>
            <div className="lohi-evidence-fact"><dt>Source coverage</dt><dd>{coverage}</dd></div>
            <div className="lohi-evidence-fact"><dt>Partial day</dt><dd>{partialDay(selectedEvidence?.range)}</dd></div>
            <div className="lohi-evidence-fact"><dt>Cohort eligibility</dt><dd>{cohortEligibility}</dd></div>
          </dl>
          <Accordion type="single" collapsible>
            <AccordionItem value="evidence-detail">
              <AccordionTrigger>Review evidence</AccordionTrigger>
              <AccordionContent>
                <dl className="lohi-evidence-detail">
                  <div><dt>Definition</dt><dd>{definition}</dd></div>
                  <div><dt>Unit</dt><dd>{unit}</dd></div>
                  <div><dt>Range</dt><dd>{range}</dd></div>
                  <div><dt>Query reference</dt><dd><code>{queryRef}</code></dd></div>
                  <div><dt>Source bindings</dt><dd>{sourceTables.length ? sourceTables.join(', ') : 'Bindings not verified'}</dd></div>
                  <div><dt>Freshness</dt><dd>{freshness}</dd></div>
                  <div><dt>Limitation</dt><dd>{limitation}</dd></div>
                </dl>
                <div className="lohi-evidence-actions">
                  <Button href={`/chat?q=${encodeURIComponent(`Review the evidence for ${dashboard?.name || 'this saved dashboard'} (${queryRef}).`)}`} size="large" icon={<MessageSquare size={16} />}>Ask the agent</Button>
                  <Button href={selectedEvidence?.sql ? `/sql?q=${encodeURIComponent(selectedEvidence.sql)}` : '/sql'} type="outlined" color="neutral" size="large" icon={<Database size={16} />}>Open SQL</Button>
                </div>
              </AccordionContent>
            </AccordionItem>
          </Accordion>
          {state === 'read-only-denied' ? <p className="m-0 text-sm text-[var(--lohi-muted-foreground)]">Ask a workspace owner for source-read access if you need live readiness. No request is sent automatically.</p> : null}
        </CardContent>
      </Card>
    </section>
  );
}
