'use client';

import { useState } from 'react';
import { Database, MessageSquare } from 'lucide-react';
import type { Chart, Dashboard, Filters, QueryMeta } from '@/lib/api';
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger, Button, Card, CardContent } from '@/lib/lohi-ui';
import { StatusPill } from '@/modules/shared/components/lohi-evidence-primitives';
import type { ReadinessSync } from '@/modules/app/hooks/connectors';
import { evidenceTime } from '@/modules/settings/source-readiness';

export type ChartEvidenceStatus = 'loading' | 'ready' | 'bounded' | 'unknown' | 'empty' | 'unsupported' | 'error' | 'capacity' | 'non_plottable' | 'denied';
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
export type EvidenceState = 'ready' | 'syncing' | 'empty' | 'stale' | 'error' | 'immature' | 'read-only-denied' | 'query-denied' | 'readiness-error';

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

export function chartEvidenceStatus(status: ChartEvidenceStatus, meta?: QueryMeta): ChartEvidenceStatus {
  if (status !== 'ready') return status;
  if (meta?.result_completeness === 'bounded' || meta?.truncated === true) return 'bounded';
  if (!meta || meta.result_completeness === 'unknown' || meta.availability_reason === 'query_evidence_unavailable') return 'unknown';
  return status;
}

export function queryLimitation(evidence: ChartEvidence | undefined, savedLimitation: string): string {
  const meta = evidence?.meta;
  if (evidence?.status === 'bounded' || meta?.result_completeness === 'bounded' || meta?.truncated === true) {
    return 'Query result is bounded to the returned rows; additional rows may have been omitted.';
  }
  if (evidence?.status === 'unknown' || (!meta && evidence?.status === 'ready') || meta?.result_completeness === 'unknown' || meta?.availability_reason === 'query_evidence_unavailable') {
    return 'Query result completeness is unknown; query evidence is unavailable.';
  }
  if (meta?.availability_reason) return meta.availability_reason.replace(/[_-]+/g, ' ');
  if (evidence && evidence.status !== 'ready') return `Current query status: ${evidence.status.replace(/_/g, ' ')}`;
  return savedLimitation;
}

export function resolveEvidenceState({ loading, denied, readinessError = false, charts, syncs, chartStatuses, cohortEligibility }: { loading: boolean; denied: boolean; readinessError?: boolean; charts: readonly Chart[]; syncs: readonly ReadinessSync[]; chartStatuses: readonly ChartEvidenceStatus[]; cohortEligibility?: string }): EvidenceState {
  if (denied) return 'read-only-denied';
  if (readinessError) return 'readiness-error';
  if (chartStatuses.some((status) => status === 'denied')) return 'query-denied';
  if (chartStatuses.some((status) => status === 'bounded' || status === 'unknown' || status === 'error' || status === 'unsupported' || status === 'capacity' || status === 'non_plottable')) return 'error';
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
  'query-denied': { label: 'Query access denied', detail: 'This credential can read the saved figure, but it cannot run the figure’s SQL query.', pill: 'denied' },
  'readiness-error': { label: 'Readiness check failed', detail: 'Source readiness could not be refreshed. Retry before treating this evidence as current.', pill: 'error' },
};

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

export function EvidencePanel({ dashboard, charts, syncs, readinessLoading, readinessDenied, readinessError, chartEvidence, appliedFilters }: { dashboard: Dashboard | null; charts: Chart[]; syncs: ReadinessSync[]; readinessLoading: boolean; readinessDenied: boolean; readinessError: boolean; chartEvidence: Record<string, ChartEvidence>; appliedFilters: Filters }) {
  const browserTimezone = typeof Intl !== 'undefined' ? Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC' : 'UTC';
  const savedEvidence = parseBoardEvidence(dashboard?.description ?? '', browserTimezone);
  const filterKey = evidenceFilterKey(appliedFilters);
  const sqlCharts = charts.filter((chart) => !!chart.sql);
  const [selectedChartID, setSelectedChartID] = useState<string | null>(null);
  const selectedChart = sqlCharts.find((chart) => chart.id === selectedChartID) ?? sqlCharts[0];
  const selectedEvidence = selectedChart && chartEvidence[selectedChart.id]?.filterKey === filterKey ? chartEvidence[selectedChart.id] : undefined;
  const chartStatuses = sqlCharts.map((chart) => chartEvidence[chart.id]?.filterKey === filterKey ? chartEvidence[chart.id].status : 'loading');
  const state = resolveEvidenceState({ loading: readinessLoading, denied: readinessDenied, readinessError, charts, syncs, chartStatuses, cohortEligibility: selectedEvidence?.cohortEligibility });
  const copy = STATE_COPY[state];
  const lastComplete = syncs.map((sync) => sync.readiness?.last_complete_at).filter((value): value is string => !!value).sort()[0];
  const staleLastComplete = syncs
    .filter((sync) => sync.readiness?.state === 'stale')
    .map((sync) => sync.readiness?.last_complete_at)
    .filter((value): value is string => !!value)
    .sort()[0];
  const freshness = [lastComplete ? `Source last complete ${evidenceTime(lastComplete)}` : null, selectedEvidence?.meta?.executed_at ? `Query executed ${evidenceTime(selectedEvidence.meta.executed_at)}` : null].filter(Boolean).join(' · ') || 'Freshness not verified';
  const queryRef = selectedEvidence?.meta?.query_ref || 'Query reference pending';
  const limitation = queryLimitation(selectedEvidence, savedEvidence.limitation);
  const definition = selectedEvidence?.definition || selectedChart?.name || 'Definition not supplied';
  const unit = selectedEvidence?.unit || (selectedChart?.y_field ? `Unit not declared for ${selectedChart.y_field}` : 'Not declared');
  const range = selectedEvidence?.range?.label || pendingRange(appliedFilters);
  const timezone = selectedEvidence?.range?.kind === 'applied'
    ? 'UTC'
    : selectedEvidence?.range
      ? 'Timezone not verified for fixed query'
      : browserTimezone;
  // Collection timestamps and a project watermark are neither query lineage
  // nor the business-date coverage of this result. Until the API supplies
  // those facts, an honest evidence panel keeps both claims unverified.
  const coverage = 'Coverage not verified';
  const cohortEligibility = selectedEvidence?.cohortEligibility || 'Eligibility not verified for this query';

  return (
    <section aria-labelledby="board-evidence-title" aria-live="polite">
      <Card>
        <CardContent>
          <div className="mb-3 flex flex-wrap items-start gap-3">
            <div className="min-w-0 flex-1"><h2 id="board-evidence-title" className="m-0 text-base font-semibold">Evidence</h2><p className="m-0 mt-1 text-sm text-[var(--lohi-muted-foreground)]">{copy.detail}</p></div>
            <StatusPill status={copy.pill} label={copy.label} grow={false} pulse={state === 'syncing'} />
          </div>
          {sqlCharts.length > 1 ? (
            <div className="lohi-evidence-figures" role="group" aria-label="Choose figure evidence">
              <span>Figure evidence</span>
              {sqlCharts.map((chart) => (
                <Button
                  key={chart.id}
                  type={selectedChart?.id === chart.id ? 'default' : 'outlined'}
                  color={selectedChart?.id === chart.id ? 'brand' : 'neutral'}
                  size="large"
                  aria-pressed={selectedChart?.id === chart.id}
                  onClick={() => setSelectedChartID(chart.id)}
                >
                  {chart.name || 'Untitled chart'}
                </Button>
              ))}
            </div>
          ) : null}
          <dl className="lohi-evidence-grid">
            <div className="lohi-evidence-fact"><dt>Timezone</dt><dd>{timezone}</dd></div>
            <div className="lohi-evidence-fact"><dt>Source coverage</dt><dd>{coverage}</dd></div>
            <div className="lohi-evidence-fact"><dt>Partial day</dt><dd>{partialDay(selectedEvidence?.range)}</dd></div>
            <div className="lohi-evidence-fact"><dt>Cohort eligibility</dt><dd>{cohortEligibility}</dd></div>
          </dl>
          {staleLastComplete ? <p className="lohi-evidence-stale-age">Source last complete {evidenceTime(staleLastComplete)}</p> : null}
          <Accordion type="single" collapsible>
            <AccordionItem value="evidence-detail">
              <AccordionTrigger>Review evidence</AccordionTrigger>
              <AccordionContent>
                <dl className="lohi-evidence-detail">
                  <div><dt>Definition</dt><dd>{definition}</dd></div>
                  <div><dt>Unit</dt><dd>{unit}</dd></div>
                  <div><dt>Range</dt><dd>{range}</dd></div>
                  <div><dt>Query reference</dt><dd><code>{queryRef}</code></dd></div>
                  <div><dt>Source bindings</dt><dd>Bindings not verified</dd></div>
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
          {state === 'query-denied' ? <p className="m-0 text-sm text-[var(--lohi-muted-foreground)]">Ask a workspace owner for SQL query access. No request is sent automatically.</p> : null}
        </CardContent>
      </Card>
    </section>
  );
}
