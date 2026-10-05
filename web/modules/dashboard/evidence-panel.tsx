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

type DeclaredSourceBinding = { connectorID: string; table: string };
type SQLToken = { kind: 'word' | 'string' | 'number' | 'punctuation'; value: string };
export type SourceEvidence = { coverage: string; bindings: string };

const SAVED_LIMITATION_FALLBACK = 'No limitation was supplied with this saved board.';
const COVERAGE_FALLBACK = 'Coverage not verified';
const BINDINGS_FALLBACK = 'Bindings not verified';

// Keep lexical regions as tokens, never erase a quoted expression into proof.
// The range scanner masks quoted identifiers/extended literals for its own
// purpose; this verifier instead rejects those unsupported lexical forms.
function executableTokens(sql: string): SQLToken[] | null {
  const tokens: SQLToken[] = [];
  for (let index = 0; index < sql.length;) {
    const rest = sql.slice(index);
    const character = sql[index];
    if (/[\t\n\f\r ]/.test(character)) {
      index += 1;
      continue;
    }
    if (rest.startsWith('--')) {
      index += 2;
      while (index < sql.length && !/[\r\n]/.test(sql[index])) index += 1;
      continue;
    }
    if (rest.startsWith('/*')) {
      let depth = 1;
      index += 2;
      while (index < sql.length && depth > 0) {
        if (sql.slice(index, index + 2) === '/*') { depth += 1; index += 2; }
        else if (sql.slice(index, index + 2) === '*/') { depth -= 1; index += 2; }
        else index += 1;
      }
      if (depth !== 0) return null;
      continue;
    }
    if (character === "'") {
      let value = '';
      let closed = false;
      index += 1;
      while (index < sql.length) {
        if (sql[index] === "'" && sql[index + 1] === "'") {
          value += "'";
          index += 2;
        } else if (sql[index] === "'") {
          index += 1;
          closed = true;
          break;
        } else {
          value += sql[index];
          index += 1;
        }
      }
      if (!closed) return null;
      tokens.push({ kind: 'string', value });
      continue;
    }
    const word = rest.match(/^[A-Za-z_][A-Za-z0-9_$]*/)?.[0];
    if (word) {
      tokens.push({ kind: 'word', value: word.toLowerCase() });
      index += word.length;
      continue;
    }
    const number = rest.match(/^[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/)?.[0];
    if (number) {
      tokens.push({ kind: 'number', value: number });
      index += number.length;
      continue;
    }
    const punctuation = rest.match(/^(?:>=|<=|<>|!=|[().,;=<>*+-])/)?.[0];
    if (!punctuation) return null;
    tokens.push({ kind: 'punctuation', value: punctuation });
    index += punctuation.length;
  }
  // Accept one terminator, never a second statement.
  if (tokens.at(-1)?.kind === 'punctuation' && tokens.at(-1)?.value === ';') tokens.pop();
  if (tokens.some((token) => token.kind === 'punctuation' && token.value === ';')) return null;
  return tokens;
}

function isWord(token: SQLToken | undefined, value: string): boolean {
  return token?.kind === 'word' && token.value === value;
}

function isPunctuation(token: SQLToken | undefined, value: string): boolean {
  return token?.kind === 'punctuation' && token.value === value;
}

function tokenDepths(tokens: readonly SQLToken[]): number[] | null {
  const depths: number[] = [];
  let depth = 0;
  for (const token of tokens) {
    if (isPunctuation(token, ')')) depth -= 1;
    if (depth < 0) return null;
    depths.push(depth);
    if (isPunctuation(token, '(')) depth += 1;
  }
  return depth === 0 ? depths : null;
}

function stripWrappingParentheses(tokens: readonly SQLToken[]): SQLToken[] {
  let stripped = [...tokens];
  while (isPunctuation(stripped[0], '(') && isPunctuation(stripped.at(-1), ')')) {
    const depths = tokenDepths(stripped);
    if (!depths || depths.slice(1, -1).includes(0)) break;
    stripped = stripped.slice(1, -1);
  }
  return stripped;
}

// Track CASE as well as parentheses: AND inside an inactive branch, function
// argument or subselect can never be mistaken for a top-level conjunct.
function topLevelConjuncts(tokens: readonly SQLToken[]): SQLToken[][] | null {
  const parts: SQLToken[][] = [];
  let depth = 0;
  let caseDepth = 0;
  let start = 0;
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (isPunctuation(token, '(')) depth += 1;
    else if (isPunctuation(token, ')')) depth -= 1;
    else if (isWord(token, 'case')) caseDepth += 1;
    else if (isWord(token, 'end')) caseDepth -= 1;
    else if (depth === 0 && caseDepth === 0 && isWord(token, 'and')) {
      parts.push(tokens.slice(start, index));
      start = index + 1;
    }
    if (depth < 0 || caseDepth < 0) return null;
  }
  if (depth !== 0 || caseDepth !== 0) return null;
  parts.push(tokens.slice(start));
  return parts;
}

const COMPARISONS = new Set(['=', '<', '>', '<=', '>=', '<>', '!=']);
const WHERE_BOUNDARIES = new Set(['group', 'having', 'order', 'limit', 'offset', 'window', 'qualify']);
const UNSUPPORTED_WORDS = new Set(['or', 'case', 'end', 'not', 'exists', 'union', 'intersect', 'except', 'with', 'join']);

// Positive grammar: [alias.]column comparison literal. No functions, casts,
// BETWEEN/IN, nested booleans, or discarded opaque terms are accepted.
function simpleAtom(tokens: readonly SQLToken[], alias: string): { column: string; operator: string; literal: SQLToken } | null {
  const atom = stripWrappingParentheses(tokens);
  let cursor = 0;
  if (atom[0]?.kind !== 'word') return null;
  if (isPunctuation(atom[1], '.')) {
    if (atom[0].value !== alias) return null;
    cursor = 2;
  }
  const column = atom[cursor];
  const operator = atom[cursor + 1];
  let literal = atom[cursor + 2];
  let length = cursor + 3;
  if (isPunctuation(literal, '-') || isPunctuation(literal, '+')) {
    literal = atom[cursor + 3];
    if (literal?.kind !== 'number') return null;
    length += 1;
  }
  if (atom.length !== length || column?.kind !== 'word' || operator?.kind !== 'punctuation'
    || !COMPARISONS.has(operator.value) || !literal
    || !(literal.kind === 'string' || literal.kind === 'number' || isWord(literal, 'true') || isWord(literal, 'false'))) return null;
  return { column: column.value, operator: operator.value, literal };
}

function declaredSourceBindings(sql: string): DeclaredSourceBinding[] | null {
  const tokens = executableTokens(sql);
  if (!tokens) return null;
  if (isWord(tokens[0], 'with')) {
    // A saved query commonly puts the governed relation in a small CTE, then
    // shapes it in later CTEs. Inspect each CTE body with the same strict flat
    // SELECT verifier used below. Only the body that directly reads
    // external_rows can establish a binding; later CTEs cannot manufacture one.
    const depths = tokenDepths(tokens);
    if (!depths) return null;
    const bindings: DeclaredSourceBinding[] = [];
    let cursor = 1;
    if (isWord(tokens[cursor], 'recursive')) return null;
    while (cursor < tokens.length && tokens[cursor]?.kind === 'word') {
      cursor += 1; // CTE name
      if (!isWord(tokens[cursor], 'as') || !isPunctuation(tokens[cursor + 1], '(')) return null;
      const open = cursor + 1;
      let close = open + 1;
      while (close < tokens.length && !(isPunctuation(tokens[close], ')') && depths[close] === 0)) close += 1;
      if (close >= tokens.length) return null;
      const body = tokens.slice(open + 1, close).map((token) => (
        token.kind === 'string' ? `'${token.value.replace(/'/g, "''")}'` : token.value
      )).join(' ');
      const sourceBindings = declaredSourceBindings(body);
      if (sourceBindings === null) return null;
      bindings.push(...sourceBindings);
      cursor = close + 1;
      if (isPunctuation(tokens[cursor], ',')) {
        cursor += 1;
        continue;
      }
      break;
    }
    if (!isWord(tokens[cursor], 'select')) return null;
    const sourceCount = tokens.filter((token, index) => (
      (isWord(token, 'from') || isWord(token, 'join')) && isWord(tokens[index + 1], 'external_rows')
    )).length;
    if (sourceCount !== bindings.length) return null;
    return bindings.length ? bindings : [];
  }
  const depths = tokenDepths(tokens);
  if (!depths) return null;
  const sources = tokens.flatMap((token, index) => (
    (isWord(token, 'from') || isWord(token, 'join')) && isWord(tokens[index + 1], 'external_rows') ? [index] : []
  ));
  if (sources.length === 0) return [];
  // Only a single flat SELECT/FROM is in the whitelist. CTEs, subqueries,
  // joins and set operations require semantic scope analysis we do not claim.
  if (!isWord(tokens[0], 'select') || sources.length !== 1
    || tokens.filter((token) => isWord(token, 'select')).length !== 1
    || tokens.filter((token) => isWord(token, 'from')).length !== 1
    || tokens.some((token) => token.kind === 'word' && UNSUPPORTED_WORDS.has(token.value))) return null;
  const sourceIndex = sources[0];
  if (depths[sourceIndex] !== 0) return null;
  let cursor = sourceIndex + 2;
  let alias = 'external_rows';
  if (isWord(tokens[cursor], 'as')) {
    cursor += 1;
    if (tokens[cursor]?.kind !== 'word') return null;
    alias = tokens[cursor++].value;
  } else if (tokens[cursor]?.kind === 'word' && !isWord(tokens[cursor], 'where')) {
    alias = tokens[cursor++].value;
  }
  if (!isWord(tokens[cursor], 'where') || depths[cursor] !== 0) return null;
  let whereEnd = tokens.length;
  for (let index = cursor + 1; index < tokens.length; index += 1) {
    if (depths[index] === 0 && tokens[index].kind === 'word' && WHERE_BOUNDARIES.has(tokens[index].value)) {
      whereEnd = index;
      break;
    }
  }
  const conjuncts = topLevelConjuncts(tokens.slice(cursor + 1, whereEnd));
  if (!conjuncts) return null;
  const connectors = new Set<string>();
  const tables = new Set<string>();
  for (const conjunct of conjuncts) {
    const atom = simpleAtom(conjunct, alias);
    if (!atom) return null;
    if (atom.column === 'connector_id' || atom.column === 'table_name') {
      if (atom.operator !== '=' || atom.literal.kind !== 'string') return null;
      (atom.column === 'connector_id' ? connectors : tables).add(atom.literal.value);
    }
  }
  if (connectors.size !== 1 || tables.size !== 1) return null;
  return [{ connectorID: [...connectors][0], table: [...tables][0] }];
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
