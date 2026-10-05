'use client';

import { useState } from 'react';
import { Database, MessageSquare } from 'lucide-react';
import type { Chart, Dashboard, Filters, QueryMeta } from '@/lib/api';
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger, Button, Card, CardContent } from '@/lib/lohi-ui';
import { StatusPill } from '@/modules/shared/components/lohi-evidence-primitives';
import type { ReadinessSync } from '@/modules/app/hooks/connectors';
import { evidenceTime } from '@/modules/settings/source-readiness';
import { scanExecutableSQL } from './chart-query';

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

type DeclaredSourceBinding = { connectorID: string; table: string };
type SQLToken = { kind: 'word' | 'string' | 'punctuation'; value: string };
export type SourceEvidence = { coverage: string; bindings: string };

const SAVED_LIMITATION_FALLBACK = 'No limitation was supplied with this saved board.';
const COVERAGE_FALLBACK = 'Coverage not verified';
const BINDINGS_FALLBACK = 'Bindings not verified';

function executableTokens(sql: string): SQLToken[] | null {
  const scanned = scanExecutableSQL(sql);
  if (!scanned.fullyClassified) return null;
  const tokens: SQLToken[] = [];
  for (let index = 0; index < scanned.executable.length;) {
    const character = scanned.executable[index];
    if (/\s/u.test(character)) {
      index += 1;
      continue;
    }
    if (character === "'") {
      let value = '';
      let closed = false;
      index += 1;
      while (index < scanned.executable.length) {
        if (scanned.executable[index] === "'" && scanned.executable[index + 1] === "'") {
          value += "'";
          index += 2;
        } else if (scanned.executable[index] === "'") {
          index += 1;
          closed = true;
          break;
        } else {
          value += scanned.executable[index];
          index += 1;
        }
      }
      if (!closed) return null;
      tokens.push({ kind: 'string', value });
      continue;
    }
    const word = scanned.executable.slice(index).match(/^[A-Za-z_][A-Za-z0-9_$]*/)?.[0];
    if (word) {
      tokens.push({ kind: 'word', value: word.toLowerCase() });
      index += word.length;
      continue;
    }
    tokens.push({ kind: 'punctuation', value: character });
    index += 1;
  }
  return tokens;
}

const QUERY_BOUNDARIES = new Set(['union', 'intersect', 'except']);
const WHERE_BOUNDARIES = new Set(['group', 'having', 'order', 'limit', 'offset', 'window', 'qualify', 'returning', ...QUERY_BOUNDARIES]);
const ALIAS_BOUNDARIES = new Set(['where', 'join', 'inner', 'left', 'right', 'full', 'cross', 'on', ...WHERE_BOUNDARIES]);

function tokenDepths(tokens: readonly SQLToken[]): number[] | null {
  const depths: number[] = [];
  let depth = 0;
  for (const token of tokens) {
    if (token.value === ')') depth -= 1;
    if (depth < 0) return null;
    depths.push(depth);
    if (token.value === '(') depth += 1;
  }
  return depth === 0 ? depths : null;
}

function splitBoolean(tokens: readonly SQLToken[], operator: 'and' | 'or'): SQLToken[][] {
  const parts: SQLToken[][] = [];
  let depth = 0;
  let start = 0;
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (token.value === '(') depth += 1;
    else if (token.value === ')') depth -= 1;
    else if (depth === 0 && token.kind === 'word' && token.value === operator) {
      parts.push(tokens.slice(start, index));
      start = index + 1;
    }
  }
  parts.push(tokens.slice(start));
  return parts;
}

function stripWrappingParentheses(tokens: readonly SQLToken[]): SQLToken[] {
  let stripped = [...tokens];
  while (stripped[0]?.value === '(' && stripped.at(-1)?.value === ')') {
    let depth = 0;
    let closesAtEnd = false;
    for (let index = 0; index < stripped.length; index += 1) {
      if (stripped[index].value === '(') depth += 1;
      if (stripped[index].value === ')') depth -= 1;
      if (depth === 0) {
        closesAtEnd = index === stripped.length - 1;
        break;
      }
    }
    if (!closesAtEnd) break;
    stripped = stripped.slice(1, -1);
  }
  return stripped;
}

function intersectConstraints(groups: readonly Set<string>[]): Set<string> {
  if (groups.length === 0) return new Set();
  return new Set([...groups[0]].filter((constraint) => groups.every((group) => group.has(constraint))));
}

function bindingAtom(tokens: readonly SQLToken[], alias: string | null, allowUnqualified: boolean): Set<string> {
  const atom = stripWrappingParentheses(tokens);
  let column: SQLToken | undefined;
  let equals: SQLToken | undefined;
  let value: SQLToken | undefined;
  if (atom.length === 3) {
    [column, equals, value] = atom;
    if (!allowUnqualified) return new Set();
  } else if (atom.length === 5 && atom[1].value === '.') {
    const qualifier = atom[0];
    if (qualifier.kind !== 'word' || qualifier.value !== (alias ?? 'external_rows')) return new Set();
    [, , column, equals, value] = atom;
  } else {
    return new Set();
  }
  if (column?.kind !== 'word' || equals?.value !== '=' || value?.kind !== 'string') return new Set();
  if (column.value !== 'connector_id' && column.value !== 'table_name') return new Set();
  return new Set([`${column.value}:${value.value}`]);
}

// Return only constraints guaranteed by the boolean expression. AND combines
// guarantees; OR keeps only guarantees present in every branch. Everything
// else is opaque, so CASE/NOT/functions/subqueries cannot manufacture proof.
function guaranteedBindings(tokens: readonly SQLToken[], alias: string | null, allowUnqualified: boolean): Set<string> {
  const expression = stripWrappingParentheses(tokens);
  if (expression.length === 0 || expression.some((token) => token.kind === 'word' && token.value === 'select')) return new Set();
  const disjunction = splitBoolean(expression, 'or');
  if (disjunction.length > 1) {
    return intersectConstraints(disjunction.map((part) => guaranteedBindings(part, alias, allowUnqualified)));
  }
  const conjunction = splitBoolean(expression, 'and');
  if (conjunction.length > 1) {
    return new Set(conjunction.flatMap((part) => [...guaranteedBindings(part, alias, allowUnqualified)]));
  }
  return bindingAtom(expression, alias, allowUnqualified);
}

function declaredSourceBindings(sql: string): DeclaredSourceBinding[] | null {
  const tokens = executableTokens(sql);
  if (!tokens) return null;
  const depths = tokenDepths(tokens);
  if (!depths) return null;
  const sourceIndexes = tokens.flatMap((token, index) => (
    token.kind === 'word' && (token.value === 'from' || token.value === 'join')
      && tokens[index + 1]?.kind === 'word' && tokens[index + 1]?.value === 'external_rows'
      ? [index]
      : []
  ));
  if (sourceIndexes.length === 0) return [];

  const bindings: DeclaredSourceBinding[] = [];
  for (const sourceIndex of sourceIndexes) {
    // JOIN scope depends on join type and ON/WHERE placement. Until that is
    // parsed deliberately, fail closed rather than attributing a nearby pair.
    if (tokens[sourceIndex].value !== 'from') return null;
    const sourceDepth = depths[sourceIndex];
    let scopeStart = -1;
    for (let index = sourceIndex - 1; index >= 0; index -= 1) {
      if (depths[index] < sourceDepth || (depths[index] === sourceDepth && QUERY_BOUNDARIES.has(tokens[index].value))) break;
      if (depths[index] === sourceDepth && tokens[index].kind === 'word' && tokens[index].value === 'select') {
        scopeStart = index;
        break;
      }
    }
    if (scopeStart < 0) return null;

    let scopeEnd = tokens.length;
    for (let index = sourceIndex + 2; index < tokens.length; index += 1) {
      if (depths[index] < sourceDepth || (depths[index] === sourceDepth && QUERY_BOUNDARIES.has(tokens[index].value))) {
        scopeEnd = index;
        break;
      }
    }
    const sameScopeSources = sourceIndexes.filter((index) => index >= scopeStart && index < scopeEnd && depths[index] === sourceDepth);
    if (sameScopeSources.length !== 1) return null;

    let cursor = sourceIndex + 2;
    let alias: string | null = null;
    if (tokens[cursor]?.kind === 'word' && tokens[cursor].value === 'as' && tokens[cursor + 1]?.kind === 'word') {
      alias = tokens[cursor + 1].value;
      cursor += 2;
    } else if (tokens[cursor]?.kind === 'word' && !ALIAS_BOUNDARIES.has(tokens[cursor].value)) {
      alias = tokens[cursor].value;
      cursor += 1;
    }

    let whereIndex = -1;
    for (let index = cursor; index < scopeEnd; index += 1) {
      if (depths[index] === sourceDepth && tokens[index].kind === 'word' && tokens[index].value === 'where') {
        whereIndex = index;
        break;
      }
    }
    if (whereIndex < 0) return null;
    let whereEnd = scopeEnd;
    for (let index = whereIndex + 1; index < scopeEnd; index += 1) {
      if (depths[index] === sourceDepth && tokens[index].kind === 'word' && WHERE_BOUNDARIES.has(tokens[index].value)) {
        whereEnd = index;
        break;
      }
    }
    const hasOtherRelations = tokens.slice(cursor, whereIndex).some((token, offset) => (
      depths[cursor + offset] === sourceDepth
      && ((token.kind === 'word' && token.value === 'join') || token.value === ',')
    ));
    const constraints = guaranteedBindings(tokens.slice(whereIndex + 1, whereEnd), alias, !hasOtherRelations);
    const connectors = new Set([...constraints].filter((item) => item.startsWith('connector_id:')).map((item) => item.slice('connector_id:'.length)));
    const tables = new Set([...constraints].filter((item) => item.startsWith('table_name:')).map((item) => item.slice('table_name:'.length)));
    // One query scope must guarantee one exact pair. IN, reversed comparisons,
    // mixed values and ambiguous expressions stay unverified.
    if (connectors.size !== 1 || tables.size !== 1) return null;
    bindings.push({ connectorID: [...connectors][0], table: [...tables][0] });
  }
  return bindings.filter((binding, index, all) => all.findIndex((candidate) => candidate.connectorID === binding.connectorID && candidate.table === binding.table) === index);
}

function oldestTimestamp(values: Array<string | null>): string | null {
  return values
    .filter((value): value is string => !!value && Number.isFinite(Date.parse(value)))
    .sort((left, right) => Date.parse(left) - Date.parse(right))
    .at(0) ?? null;
}

export function sourceEvidence(sql: string | undefined, meta: QueryMeta | undefined): SourceEvidence {
  if (!sql) return { coverage: COVERAGE_FALLBACK, bindings: BINDINGS_FALLBACK };
  const declared = declaredSourceBindings(sql);
  if (declared === null) return { coverage: COVERAGE_FALLBACK, bindings: BINDINGS_FALLBACK };
  if (declared.length === 0) return { coverage: COVERAGE_FALLBACK, bindings: 'No external source bindings declared' };
  const watermark = meta?.serving_data_watermark;
  if (!watermark) return { coverage: COVERAGE_FALLBACK, bindings: BINDINGS_FALLBACK };

  const matched = declared.map((binding) => watermark.sources.find((source) => (
    source.connector_id === binding.connectorID && source.table === binding.table
  ))).filter((source): source is NonNullable<typeof source> => !!source);
  if (matched.length !== declared.length) {
    const detail = `${matched.length}/${declared.length} declared bindings matched`;
    return { coverage: `${COVERAGE_FALLBACK} (${detail})`, bindings: `${BINDINGS_FALLBACK} (${detail})` };
  }

  const tableNames = [...new Set(declared.map((binding) => binding.table))];
  const bindingNoun = declared.length === 1 ? 'binding' : 'bindings';
  const bindings = `Verified ${declared.length} of ${declared.length} declared source ${bindingNoun} · ${tableNames.join(', ')}`;
  const starts = matched.map((source) => source.capture_started_at);
  const finishes = matched.map((source) => source.capture_finished_at);
  if (starts.some((value) => !value) || finishes.some((value) => !value)) {
    return { coverage: `${COVERAGE_FALLBACK} (capture interval missing)`, bindings };
  }
  const sharedStart = starts.filter((value): value is string => !!value).sort((left, right) => Date.parse(left) - Date.parse(right)).at(-1) ?? null;
  const sharedFinish = finishes.filter((value): value is string => !!value).sort((left, right) => Date.parse(left) - Date.parse(right)).at(0) ?? null;
  const sharedStartAt = sharedStart ? Date.parse(sharedStart) : Number.NaN;
  const sharedFinishAt = sharedFinish ? Date.parse(sharedFinish) : Number.NaN;
  if (!Number.isFinite(sharedStartAt) || !Number.isFinite(sharedFinishAt) || sharedStartAt > sharedFinishAt) {
    return { coverage: `${COVERAGE_FALLBACK} (capture interval invalid or non-overlapping)`, bindings };
  }
  const landedAt = oldestTimestamp(matched.map((source) => source.landed_at));
  const total = watermark.total_sources;
  const sourceNoun = declared.length === 1 ? 'source' : 'sources';
  const parts = [
    `${evidenceTime(sharedStart)} – ${evidenceTime(sharedFinish)}`,
    `${declared.length}/${declared.length} required ${sourceNoun}`,
    total > declared.length ? `${total} source watermarks available` : null,
    landedAt ? `watermark ${evidenceTime(landedAt)}` : null,
  ].filter((part): part is string => !!part);
  return { coverage: parts.join(' · '), bindings };
}

export function parseSavedLimitation(description: string): string {
  const trimmed = description.trim();
  if (!trimmed) return SAVED_LIMITATION_FALLBACK;
  try {
    const object = JSON.parse(trimmed) as Record<string, unknown>;
    let saved = SAVED_LIMITATION_FALLBACK;
    for (const [key, value] of Object.entries(object)) {
      const label = key.trim().toLowerCase();
      if ((label === 'limitation' || label === 'limitations') && (typeof value === 'string' || typeof value === 'number')) saved = String(value);
    }
    return saved;
  } catch {
    for (const part of trimmed.split(/[\n;|]+/)) {
      const match = part.match(/^\s*([a-zA-Z][a-zA-Z0-9 _-]{1,40})\s*[:=]\s*(.+?)\s*$/);
      if (!match) continue;
      const label = match[1].trim().toLowerCase().replace(/\s+/g, '_');
      if (label === 'limitation' || label === 'limitations') return match[2].trim();
    }
  }
  return SAVED_LIMITATION_FALLBACK;
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
  const savedLimitation = parseSavedLimitation(dashboard?.description ?? '');
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
  const limitation = queryLimitation(selectedEvidence, savedLimitation);
  const definition = selectedEvidence?.definition || selectedChart?.name || 'Definition not supplied';
  const unit = selectedEvidence?.unit || (selectedChart?.y_field ? `Unit not declared for ${selectedChart.y_field}` : 'Not declared');
  const range = selectedEvidence?.range?.label || pendingRange(appliedFilters);
  const timezone = selectedEvidence?.range?.kind === 'applied'
    ? 'UTC'
    : selectedEvidence?.range
      ? 'Timezone not verified for fixed query'
      : browserTimezone;
  const source = sourceEvidence(selectedEvidence?.sql, selectedEvidence?.meta);
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
            <div className="lohi-evidence-fact"><dt>Source coverage</dt><dd>{source.coverage}</dd></div>
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
                  <div><dt>Source bindings</dt><dd>{source.bindings}</dd></div>
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
