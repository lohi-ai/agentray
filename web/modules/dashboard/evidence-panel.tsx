'use client';

import Link from 'next/link';
import { Database, MessageSquare } from 'lucide-react';
import type { Chart, Dashboard, QueryMeta } from '@/lib/api';
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger, Button, Card, CardContent } from '@/lib/lohi-ui';
import { StatusPill } from '@/modules/shared/components/lohi-evidence-primitives';
import type { ReadinessSync } from '@/modules/app/hooks/connectors';
import { evidenceTime } from '@/modules/settings/source-readiness';

export type ChartEvidence = { status: 'loading' | 'ready' | 'empty' | 'error'; meta?: QueryMeta };
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

export function resolveEvidenceState({ loading, denied, charts, syncs, evidence }: { loading: boolean; denied: boolean; charts: readonly Chart[]; syncs: readonly ReadinessSync[]; evidence: BoardEvidence }): EvidenceState {
  if (denied) return 'read-only-denied';
  if (/under\s*14|not ready|immature/i.test(evidence.cohortEligibility)) return 'immature';
  if (syncs.some((s) => s.readiness?.state === 'error' || s.readiness?.state === 'incomplete')) return 'error';
  if (syncs.some((s) => s.readiness?.state === 'stale')) return 'stale';
  if (loading || syncs.some((s) => s.readiness?.state === 'syncing')) return 'syncing';
  if (charts.length === 0 || syncs.length === 0 || syncs.every((s) => !s.readiness || s.readiness.state === 'not_configured')) return 'empty';
  return 'ready';
}

const STATE_COPY: Record<EvidenceState, { label: string; detail: string; pill: string }> = {
  ready: { label: 'Evidence ready', detail: 'Queryable sources and execution evidence are available.', pill: 'ready' },
  syncing: { label: 'Evidence updating', detail: 'Previous complete evidence remains timestamped while the new range is checked.', pill: 'working' },
  empty: { label: 'Evidence unavailable', detail: 'Connect and complete a source sync to verify this view.', pill: 'idle' },
  stale: { label: 'Evidence is stale', detail: 'Do not treat the previous result as current; its completion time is shown below.', pill: 'attention' },
  error: { label: 'Evidence incomplete', detail: 'Sync accepted; some rows are not available yet. No current conclusion is shown.', pill: 'error' },
  immature: { label: 'Cohort not ready', detail: 'The saved cohort has not reached its declared maturity window.', pill: 'immature' },
  'read-only-denied': { label: 'Read-only evidence', detail: 'Saved definitions remain readable, but source readiness is not granted to this credential.', pill: 'denied' },
};

function latestMeta(evidence: Record<string, ChartEvidence>, charts: Chart[]): QueryMeta | undefined {
  return charts.map((chart) => evidence[chart.id]?.meta).filter((meta): meta is QueryMeta => !!meta).sort((a, b) => Date.parse(b.executed_at) - Date.parse(a.executed_at))[0];
}

export function EvidencePanel({ dashboard, charts, syncs, readinessLoading, readinessDenied, chartEvidence }: { dashboard: Dashboard | null; charts: Chart[]; syncs: ReadinessSync[]; readinessLoading: boolean; readinessDenied: boolean; chartEvidence: Record<string, ChartEvidence> }) {
  const browserTimezone = typeof Intl !== 'undefined' ? Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC' : 'UTC';
  const evidence = parseBoardEvidence(dashboard?.description ?? '', browserTimezone);
  const queryMeta = latestMeta(chartEvidence, charts);
  const queryUpdating = charts.some((chart) => chartEvidence[chart.id]?.status === 'loading');
  const state = resolveEvidenceState({ loading: readinessLoading || queryUpdating, denied: readinessDenied, charts, syncs, evidence });
  const copy = STATE_COPY[state];
  const sourceTables = [...new Set(queryMeta?.serving_data_watermark?.sources.map((source) => source.table) ?? [])];
  const lastComplete = syncs.map((sync) => sync.readiness?.last_complete_at).filter((value): value is string => !!value).sort().at(-1);
  const freshness = queryMeta?.executed_at ? `Executed ${evidenceTime(queryMeta.executed_at)}` : lastComplete ? `Last complete ${evidenceTime(lastComplete)}` : 'Freshness not verified';
  const queryRef = queryMeta?.query_ref || evidence.recipeRef;
  const firstSQL = charts.find((chart) => chart.sql)?.sql;
  const limitation = queryMeta?.availability_reason ? queryMeta.availability_reason.replace(/[_-]+/g, ' ') : evidence.limitation;

  return (
    <section aria-labelledby="board-evidence-title" aria-live="polite">
      <Card>
        <CardContent>
          <div className="mb-3 flex flex-wrap items-start gap-3">
            <div className="min-w-0 flex-1"><h2 id="board-evidence-title" className="m-0 text-base font-semibold">Evidence</h2><p className="m-0 mt-1 text-sm text-[var(--lohi-muted-foreground)]">{copy.detail}</p></div>
            <StatusPill status={copy.pill} label={copy.label} grow={false} pulse={state === 'syncing'} />
          </div>
          <dl className="lohi-evidence-grid">
            <div className="lohi-evidence-fact"><dt>Timezone</dt><dd>{evidence.timezone}</dd></div>
            <div className="lohi-evidence-fact"><dt>Source coverage</dt><dd>{evidence.coverage}</dd></div>
            <div className="lohi-evidence-fact"><dt>Partial day</dt><dd>{evidence.partialDay}</dd></div>
            <div className="lohi-evidence-fact"><dt>Cohort eligibility</dt><dd>{evidence.cohortEligibility}</dd></div>
          </dl>
          <Accordion type="single" collapsible>
            <AccordionItem value="evidence-detail">
              <AccordionTrigger>Review evidence</AccordionTrigger>
              <AccordionContent>
                <dl className="lohi-evidence-detail">
                  <div><dt>Definition</dt><dd>{evidence.definition}</dd></div>
                  <div><dt>Unit</dt><dd>{evidence.unit}</dd></div>
                  <div><dt>Range</dt><dd>{evidence.range}</dd></div>
                  <div><dt>Query reference</dt><dd><code>{queryRef}</code></dd></div>
                  <div><dt>Source bindings</dt><dd>{sourceTables.length ? sourceTables.join(', ') : 'Bindings not verified'}</dd></div>
                  <div><dt>Freshness</dt><dd>{freshness}</dd></div>
                  <div><dt>Limitation</dt><dd>{limitation}</dd></div>
                </dl>
                <div className="lohi-evidence-actions">
                  <Button href={`/chat?q=${encodeURIComponent(`Review the evidence for ${dashboard?.name || 'this saved dashboard'} (${queryRef}).`)}`} size="large" icon={<MessageSquare size={16} />}>Ask the agent</Button>
                  <Button href={firstSQL ? `/sql?q=${encodeURIComponent(firstSQL)}` : '/sql'} type="outlined" color="neutral" size="large" icon={<Database size={16} />}>Open SQL</Button>
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
