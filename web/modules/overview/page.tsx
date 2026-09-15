'use client';

import { useMemo, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertTriangle, ArrowUpRight, Clock, Lock, RefreshCw } from 'lucide-react';
import { AgentRayAPI, APIError, type AgentRecommendation, type ListFindingsResult, type OverviewMetric, type OverviewRange, type OverviewResult, type OverviewRevenueDetail, type OverviewSourceStatus } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { formatCompact, formatNumber } from '@/lib/format';
import { platformLabel } from '@/lib/platform';
import { firstValuePath, settingsPath } from '@/lib/ia';
import { useEventNames } from '@/modules/app/hooks';
import { evidenceAvailable, evidenceLine } from '@/modules/plans/lib/plans';
import { AppShell } from '@/modules/shared/components/app-shell';
import { PageShell } from '@/modules/shared/components/page-shell';
import { Chart } from '@/modules/shared/components/charts';
import { BarRows, Button, Callout, EmptyState, Loading, Panel, Segment, StatsStrip, StatusPill } from '@/modules/shared/components/signal-primitives';
import { FirstEventQuickstart } from '@/modules/dashboard/first-event-quickstart';

// The range control always offers Today plus the complete-day windows. Today
// is the explicit partial period: the backend returns no comparison for it
// and the header says so rather than implying a full day.
const PERIODS = [
  { value: 'today', label: 'Today' },
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: '90d', label: '90 days' },
];

// The platform control is always visible with the full classifier vocabulary —
// a one-platform product still gets the control, so the filter is discoverable
// before a second platform exists and "Unknown" stays reachable.
const PLATFORM_OPTIONS = ['web', 'ios', 'android', 'server', 'unknown'].map((value) => ({
  value,
  label: platformLabel(value),
}));

// metricTile renders one headline number honestly: a metric that is not "ok"
// shows its state, never a fabricated zero. "unconfigured" is an action —
// the metric is defined but the project has not told us what to measure.
export function metricTile(label: string, m: OverviewMetric): { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } {
  if (m.state !== 'ok' || m.value === undefined) {
    const stateLabel =
      m.state === 'unconfigured' ? 'Set up'
      : m.state === 'not_ready' ? 'Not ready'
      : m.state === 'no_data' ? 'No data'
      : 'Not available';
    return { label, value: stateLabel };
  }
  const tile: { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } = {
    label,
    value: formatCompact(m.value),
  };
  if (m.previous !== undefined && m.previous > 0) {
    const pct = ((m.value - m.previous) / m.previous) * 100;
    tile.delta = `${pct >= 0 ? '+' : ''}${pct.toFixed(0)}%`;
    tile.deltaTone = pct >= 0 ? 'up' : 'down';
  }
  return tile;
}

// --- Money ---------------------------------------------------------------
// An amount is only meaningful with its unit: AgentRay converts nothing, so
// every money figure the page prints carries the currency its sender declared,
// and the per-currency arithmetic is shown underneath instead of a total that
// would silently add VND to USD.
const REVENUE_LABEL = 'Net revenue';

// formatMoney renders an integer amount with thousands separators and a real
// minus sign (U+2212), so a net reversal stays legible and is never mistaken
// for a hyphen inside a table of numbers.
export function formatMoney(value: number): string {
  return `${value < 0 ? '\u2212' : ''}${formatNumber(Math.abs(value))}`;
}

// revenueTile is the Monetization headline. The signed net comes from the
// served revenue_detail, not from `OverviewMetric.value`, which the operation
// leaves unsigned — a net reversal is a real measured negative, not a state to
// be hidden behind "No data". A legacy `ok` with no detail renders "Not
// available" rather than a bare number with no currency attached.
//
// The delta follows metricTile's rule — a comparison needs a positive base —
// applied to the same currency's previous-window net: a percentage over a zero
// or reversal base would invert its own meaning, and there is no FX, so a
// different currency's net is not a base at all.
export function revenueTile(
  m: OverviewMetric,
  detail?: OverviewRevenueDetail | null,
): { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } {
  if (m.state !== 'ok') return metricTile(REVENUE_LABEL, m);
  if (!detail?.currency) return { label: REVENUE_LABEL, value: 'Not available' };
  const tile: { label: string; value: string; delta?: string; deltaTone?: 'up' | 'down' } = {
    label: REVENUE_LABEL,
    value: `${formatMoney(detail.net)} ${detail.currency}`,
  };
  const previous = detail.previous_net;
  if (previous !== undefined && previous > 0) {
    const pct = ((detail.net - previous) / previous) * 100;
    tile.delta = `${pct >= 0 ? '+' : ''}${pct.toFixed(0)}%`;
    tile.deltaTone = pct >= 0 ? 'up' : 'down';
  }
  return tile;
}

// revenueBreakdownRows is the money arithmetic behind the headline — one row
// per declared currency, which is the only set of figures that may be compared
// with each other. Values are pre-formatted strings so the rendering tests pin
// the contract (signed, unit-free cells under a currency heading).
export type RevenueBreakdownRow = { currency: string; gross: string; reversed: string; net: string; headline: boolean };

export function revenueBreakdownRows(detail?: OverviewRevenueDetail | null): RevenueBreakdownRow[] {
  if (!detail) return [];
  return detail.by_currency.map((row) => ({
    currency: row.currency,
    gross: formatMoney(row.gross),
    reversed: formatMoney(row.reversed),
    net: formatMoney(row.net),
    headline: row.currency === detail.currency,
  }));
}

function retentionLine(label: string, p: { state: string; rate: number; returned: number; eligible: number }): string {
  if (p.state === 'not_ready' || p.eligible === 0) return `${label}: Not ready — not enough mature cohorts yet`;
  if (p.state !== 'ok') return `${label}: ${p.state === 'unconfigured' ? 'Set up' : 'Unavailable'}`;
  return `${label}: ${(p.rate * 100).toFixed(1)}% (${formatCompact(p.returned)} of ${formatCompact(p.eligible)} returned)`;
}

export function retentionTile(label: string, p: { state: string; rate: number; returned: number; eligible: number }): { label: string; value: string } {
  if (p.state === 'not_ready' || p.eligible === 0) return { label, value: 'Not ready' };
  if (p.state !== 'ok') return { label, value: p.state === 'unconfigured' ? 'Set up' : 'Not available' };
  return { label, value: `${(p.rate * 100).toFixed(1)}%` };
}

// --- Per-tile provenance -------------------------------------------------
// The hard contract in docs/redesign/overview-dashboard.md: every tile carries
// a visible line naming the metric version, the range, the project timezone,
// the coverage and the freshness. Composed here from served fields only.

// A tile's input population. Provenance names what the tile actually measured —
// one blanket percentage across the page would be a claim no tile can support.
export type TileInput =
  | { kind: 'metric'; metric: OverviewMetric }
  | { kind: 'money'; metric: OverviewMetric; detail?: OverviewRevenueDetail | null }
  | { kind: 'retention'; day: 1 | 7 | 30; point: { state: string; eligible: number } }
  // Every received event — the population the retired-surface reads (traffic
  // class, AI-cited pages, platform split, top events, event volume) count.
  | { kind: 'events' }
  // A metric over that same all-events population: the metric's own state
  // decides the label, the coverage line counts events, not qualifying rows.
  | { kind: 'eventsMetric'; metric: OverviewMetric }
  | { kind: 'unserved' };

// A calendar date in the project timezone.
type LocalDate = { year: number; month: number; day: number };

// localDate reads a served instant as its calendar date in the project
// timezone. The product has exactly one day-boundary convention, and slicing a
// UTC string names the wrong day for any project east or west of it.
function localDate(iso: string, timezone: string): LocalDate {
  const parts = new Intl.DateTimeFormat('en-US', { year: 'numeric', month: '2-digit', day: '2-digit', timeZone: timezone })
    .formatToParts(new Date(iso));
  const at = (type: string) => Number(parts.find((p) => p.type === type)?.value ?? 0);
  return { year: at('year'), month: at('month'), day: at('day') };
}

// dayBefore steps one calendar day back over the calendar parts, never by
// subtracting 24 hours: a DST transition makes a local day 23 or 25 hours long,
// and a fixed subtraction prints a span that contradicts the day count beside
// it ("Mar 2–7 · 7 complete days" for a window that ends Mar 8).
function dayBefore({ year, month, day }: LocalDate): LocalDate {
  const at = new Date(Date.UTC(year, month - 1, day));
  at.setUTCDate(at.getUTCDate() - 1);
  return { year: at.getUTCFullYear(), month: at.getUTCMonth() + 1, day: at.getUTCDate() };
}

// monthDayLabel renders one calendar date's month and day. The date is already
// a project-calendar date, so it is formatted in UTC — a second conversion
// would move it again.
function monthDayLabel(d: LocalDate): { month: string; day: string } {
  const parts = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' })
    .formatToParts(new Date(Date.UTC(d.year, d.month - 1, d.day)));
  const at = (type: string) => parts.find((p) => p.type === type)?.value ?? '';
  return { month: at('month'), day: at('day') };
}

// rangeSpan is the shared "Sep 5–11 · 7 complete days" wording — the header
// sub and every tile's provenance print the same range, so one function owns
// it. The served range is half-open, so its last covered day is the one
// before `to`.
function rangeSpan(range: OverviewRange, timezone: string): string {
  if (!range.complete_days) return 'Today so far';
  // The served range is half-open — `to` is the first instant after it — so the
  // last covered day is the calendar day before `to` in the project zone.
  const from = localDate(range.from, timezone);
  const to = dayBefore(localDate(range.to, timezone));
  const start = monthDayLabel(from);
  const end = monthDayLabel(to);
  // Within one month the end day alone reads unambiguously: "Sep 5–11".
  const sameMonth = from.year === to.year && from.month === to.month;
  return `${start.month} ${start.day}–${sameMonth ? end.day : `${end.month} ${end.day}`} · ${range.days} complete days`;
}

// rangeLabel is the header sub: the shared range span plus the timezone the
// figures were computed in — "Sep 5–11 · 7 complete days · Asia/Ho_Chi_Minh",
// or "Today so far · <tz> · partial day, no comparison" for the partial range.
export function rangeLabel(res: OverviewResult): string {
  const timezone = res.context.timezone_source === 'fallback'
    ? 'UTC fallback — no project timezone set'
    : res.context.timezone;
  const span = rangeSpan(res.context.range, res.context.timezone);
  return res.context.range.complete_days ? `${span} · ${timezone}` : `${span} · ${timezone} · partial day, no comparison`;
}

// tileRange is the tile's own window. Retention runs over lifetime cohorts, so
// stamping the selected range on D1/D7/D30 would be a false claim.
function tileRange(res: OverviewResult, input: TileInput): string {
  if (input.kind === 'retention') {
    return res.retention.cohort_window === 'lifetime' ? 'lifetime cohorts' : `${res.retention.cohort_window} cohorts`;
  }
  // The money read serves its own window; stamping the context range on it
  // would be a claim about a window the arithmetic never covered.
  const range = input.kind === 'money' && input.detail ? input.detail.window : res.context.range;
  return rangeSpan(range, res.context.timezone);
}

// tileCoverage is the tile's own input population — the qualifying events the
// metric was computed from, the matured members behind a retention rate, the
// served reason a metric is unconfigured, and an explicit "not instrumented"
// for a tile the operation does not serve at all. A number is never borrowed
// from another tile's coverage.
function tileCoverage(res: OverviewResult, input: TileInput): string {
  if (input.kind === 'unserved') return 'not instrumented — no served metric';
  if (input.kind === 'events') return `coverage ${formatCompact(res.data_status.events_in_range)} events in range`;
  if (input.kind === 'eventsMetric') {
    if (input.metric.state === 'ok') return `coverage ${formatCompact(res.data_status.events_in_range)} events in range`;
    return res.data_status.ever_received ? 'no events in range' : 'no events received yet';
  }
  if (input.kind === 'retention') {
    return input.point.eligible === 0
      ? `no mature ${input.day}-day cohort yet`
      : `coverage ${formatCompact(input.point.eligible)} mature members`;
  }
  // Money coverage is the money read's own population — the deduplicated
  // bookings the arithmetic ran on, never the page's event count.
  if (input.kind === 'money') {
    const detail = input.detail;
    if (!detail) {
      // A packet without the money detail cannot state a money population: say
      // so rather than borrowing the page's event coverage for a figure it
      // never counted. An unconfigured project still gets its served reason.
      return input.metric.state === 'unconfigured'
        ? tileCoverage(res, { kind: 'metric', metric: input.metric })
        : 'money coverage not reported';
    }
    const excluded = detail.excluded_rows > 0 ? `, ${formatNumber(detail.excluded_rows)} excluded` : '';
    if (detail.deduped_rows === 0) return `0 valid money rows${excluded}`;
    return `${formatNumber(detail.deduped_rows)} deduplicated ${detail.deduped_rows === 1 ? 'row' : 'rows'}${excluded}`;
  }
  const m = input.metric;
  if (m.state === 'ok') {
    return `coverage ${formatCompact(res.data_status.qualifying_in_range)} of ${formatCompact(res.data_status.events_in_range)} events in range`;
  }
  if (m.state === 'no_data') {
    return res.data_status.ever_received ? 'no qualifying events in range' : 'no events received yet';
  }
  if (m.state === 'unconfigured') return m.notes?.[0] || 'no verified source configured';
  return 'not measured yet';
}

// tileProvenance renders the five contract fields in order, from served fields
// only: `metric <version> · <range> · <timezone> · <coverage> · <freshness>`.
// The version is the single one the operation serves (never a per-group guess),
// and freshness is the absolute receipt time — never a relative "refreshed 2
// min ago", which a cached figure cannot honestly claim.
export function tileProvenance(res: OverviewResult, input: TileInput): string {
  const timezone = res.context.timezone_source === 'fallback'
    ? 'UTC fallback — no project timezone set'
    : res.context.timezone;
  return [
    `metric ${res.context.metric_version}`,
    tileRange(res, input),
    timezone,
    tileCoverage(res, input),
    freshnessLabel(res).text,
  ].join(' · ');
}

// Tile composers: a tile and its provenance are built together, so no tile can
// reach a StatsStrip without one.

// acquisitionStats is the Acquisition group's tile row — New people is the
// one served acquisition metric; the ranked lists below it are BarRows, not
// tiles, so they are not in this strip.
export function acquisitionStats(res: OverviewResult) {
  return [metricStat(res, 'New people', res.metrics.new_users)];
}
function metricStat(res: OverviewResult, label: string, m: OverviewMetric) {
  return { ...metricTile(label, m), provenance: tileProvenance(res, { kind: 'metric', metric: m }) };
}

function revenueStat(res: OverviewResult) {
  const metric = res.metrics.revenue;
  const detail = res.metrics.revenue_detail;
  return { ...revenueTile(metric, detail), provenance: tileProvenance(res, { kind: 'money', metric, detail }) };
}

function retentionStat(res: OverviewResult, label: string, day: 1 | 7 | 30, p: { state: string; rate: number; returned: number; eligible: number }) {
  return { ...retentionTile(label, p), provenance: tileProvenance(res, { kind: 'retention', day, point: p }) };
}

function unservedStat(res: OverviewResult, label: string) {
  return { label, value: 'Not available', provenance: tileProvenance(res, { kind: 'unserved' }) };
}

// A local composition, not a shared primitive: every dashboard category needs
// a real destination or explicit state explanation, while its contents reuse
// the shipped Panel, StatsStrip, Chart, and BarRows primitives.
function MetricGroup({ title, action, children }: { title: string; action?: ReactNode; children: ReactNode }) {
  return <section aria-label={title}><Panel title={title} action={action}>{children}</Panel></section>;
}

// A money cell is unit-less on purpose: the currency is the row's own heading,
// so no reader can add one row's number to another's. At narrow widths the row
// stacks and each value carries its column name, keeping the table inside the
// panel instead of pushing the page sideways.
const MONEY_CELL = '[@media(min-width:701px)]:px-3 [@media(min-width:701px)]:py-2 text-right tabular-nums [@media(max-width:700px)]:flex [@media(max-width:700px)]:justify-between [@media(max-width:700px)]:before:content-[attr(data-label)] [@media(max-width:700px)]:before:text-[var(--color-text-secondary)]';
const MONEY_HEAD_CELL = `border-b border-[var(--color-border)] ${MONEY_CELL}`;

// The per-currency arithmetic behind the Net revenue tile, plus the served
// notes that explain the headline and name what was excluded. It is page-local
// markup because no shared primitive shows three signed measures per row.
function RevenueBreakdown({ metric, detail }: { metric: OverviewMetric; detail?: OverviewRevenueDetail | null }) {
  const rows = revenueBreakdownRows(detail);
  const notes = metric.notes ?? [];
  return (
    <div className="flex flex-col gap-2">
      {rows.length > 0 ? (
        // Desktop: one bordered table. ≤700px: the wrap drops its frame and each
        // row becomes its own card, so nothing pushes the page sideways — the
        // same treatment the approved prototype uses.
        <div className="rounded-[var(--radius-md)] [@media(min-width:701px)]:border [@media(min-width:701px)]:border-[var(--color-border)]">
          <table className="w-full text-sm">
            <caption className="px-3 pt-3 text-left text-xs text-[var(--color-text-secondary)]">Net revenue by declared currency</caption>
            <thead className="text-xs text-[var(--color-text-secondary)] [@media(max-width:700px)]:sr-only">
              <tr>
                <th scope="col" className="border-b border-[var(--color-border)] px-3 py-2 text-left font-medium">Currency</th>
                <th scope="col" className={MONEY_HEAD_CELL}>Gross</th>
                <th scope="col" className={MONEY_HEAD_CELL}>Reversed</th>
                <th scope="col" className={MONEY_HEAD_CELL}>Net</th>
              </tr>
            </thead>
            <tbody className="[@media(min-width:701px)]:divide-y [@media(min-width:701px)]:divide-[var(--color-border)]">
              {rows.map((row) => (
                <tr key={row.currency} className="[@media(max-width:700px)]:mb-2 [@media(max-width:700px)]:block [@media(max-width:700px)]:rounded-[var(--radius-md)] [@media(max-width:700px)]:border [@media(max-width:700px)]:border-[var(--color-border)] [@media(max-width:700px)]:p-3">
                  <th scope="row" data-label="Currency" className={`${MONEY_CELL} text-left font-medium`}>
                    {/* One flex child: on the stacked layout the label pairs
                        with the whole reading ("VND · headline"), never with
                        half of it. */}
                    <span>
                      {row.currency}
                      {row.headline ? <span className="text-[var(--color-text-secondary)]"> · headline</span> : null}
                    </span>
                  </th>
                  <td data-label="Gross" className={MONEY_CELL}>{row.gross}</td>
                  <td data-label="Reversed" className={MONEY_CELL}>{row.reversed}</td>
                  <td data-label="Net" className={MONEY_CELL}>{row.net}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {/* Served notes, verbatim: they name the headline currency, state that no
          FX is applied, and say which rows were excluded and why. */}
      {notes.length > 0 ? (
        <ul className="flex flex-col gap-1 text-xs text-[var(--color-text-secondary)]">
          {notes.map((note) => <li key={note}>{note}</li>)}
        </ul>
      ) : null}
    </div>
  );
}

// trendMeaning distinguishes "the chart is flat because nothing qualified"
// from "the chart is flat because nothing arrived" — a padded zero series
// must never read as a populated trend.
function trendMeaning(res: OverviewResult): 'data' | 'receipt_only' | 'empty' {
  if (res.data_status.qualifying_in_range > 0) return 'data';
  if (res.data_status.events_in_range > 0) return 'receipt_only';
  return 'empty';
}

// freshnessLabel ages capture receipt time, not client occurrence time: an
// offline event that arrives late proves the source is currently reachable.
// The stale trigger is the SERVED verdict — data_status.state == 'quiet' —
// with the client-side age check kept only as the fallback for a cached packet
// whose served state predates the threshold. `word` is the StatusPill's label
// word; `now` is injectable so staleness is testable.
export function freshnessLabel(res: OverviewResult, now = Date.now()): { text: string; word: string; stale: boolean } {
  const received = res.data_status.last_received_at;
  if (!received) return { text: 'No capture receipts yet', word: 'No capture receipts yet', stale: true };
  const at = new Date(received);
  const text = `Last received ${at.toISOString().slice(0, 16).replace('T', ' ')} UTC`;
  const stale = res.data_status.state !== 'fresh' || now - at.getTime() > 24 * 60 * 60 * 1000;
  return { text, word: stale ? 'Quiet' : 'Data fresh', stale };
}

// sourcePill maps a served connector-sync state to the StatusPill's word +
// tone — status is word + dot, never color alone (§Responsive and
// accessibility). An unknown future state degrades to a neutral pill rather
// than a bare word.
export function sourcePill(state: OverviewSourceStatus['state']): { status: string; label: string } {
  switch (state) {
    case 'healthy': return { status: 'healthy', label: 'Healthy' };
    case 'partial': return { status: 'attention', label: 'Partial data' };
    case 'error': return { status: 'attention', label: 'Needs attention' };
    case 'paused': return { status: 'paused', label: 'Paused' };
    case 'not_ready': return { status: 'idle', label: 'Not run yet' };
    default: return { status: 'idle', label: 'Set up a table' };
  }
}

// One mutually exclusive view state per the redesign state contract
// (docs/redesign/design.md): loading keeps the layout, a 403 names the missing
// access, other failures offer retry, a project that has never received (or
// whose catalog is verification-only) is first-run, and the data states split
// "nothing arrived" from "arrived but nothing qualified" from "the active
// filter excluded everything". `stale` is a modifier on the data states, not
// a state of its own — last-known figures stay on screen with a timestamp.
export type OverviewViewState =
  | 'loading'
  | 'no_access'
  | 'error'
  | 'first_run'
  | 'filtered_empty'
  | 'receipt_only'
  | 'empty'
  | 'data';

export function overviewViewState(input: {
  projectID: string | undefined;
  isLoading: boolean;
  error: unknown;
  res: OverviewResult | null;
  showFirstEvent: boolean;
  platform: string;
  period: string;
}): OverviewViewState {
  const { projectID, isLoading, error, res, showFirstEvent, platform, period } = input;
  if (!projectID || (isLoading && !res)) return 'loading';
  if (error && !res) {
    return error instanceof APIError && error.status === 403 ? 'no_access' : 'error';
  }
  if (!res) return 'loading';
  if (res.data_status.qualifying_in_range > 0) return 'data';
  if (!res.data_status.ever_received || showFirstEvent) return 'first_run';
  if (res.data_status.events_in_range > 0) return 'receipt_only';
  if (platform || period !== '7d') return 'filtered_empty';
  return 'empty';
}

// The value-first panel has exactly two honest branches (§Value-first story).
// A finding is shown only when it is display-complete — an open row with a
// title, a rationale (where the comparison lives) and a parseable evidence
// envelope — because AgentRecommendation carries no typed comparison or
// next-action field, and inferring one from prose or a bare number would
// present a guess as evidence. Everything else falls back to a capability
// explanation, never a fabricated live number.
export type NextStep =
  | { kind: 'finding'; title: string; observation: string; evidence: string }
  | { kind: 'capability'; reason: 'no_finding' | 'incomplete_finding' | 'unavailable' };

function isEvidenceBackedFinding(candidate: AgentRecommendation): boolean {
  return candidate.status === 'open'
    && !!candidate.title?.trim()
    && !!candidate.rationale?.trim()
    && evidenceAvailable(candidate);
}

export function bestNextStep(findings: readonly AgentRecommendation[] | undefined, unavailable = false): NextStep {
  if (unavailable) return { kind: 'capability', reason: 'unavailable' };
  // list_findings is open-first and impact-ranked. A malformed legacy row must
  // not hide the next real, evidence-backed action.
  const finding = findings?.find(isEvidenceBackedFinding);
  if (finding) {
    return { kind: 'finding', title: finding.title.trim(), observation: finding.rationale.trim(), evidence: evidenceLine(finding) };
  }
  // Rows arrived but none is display-complete is a different fact from "no
  // findings yet" — the capability copy names which one happened.
  return { kind: 'capability', reason: findings && findings.length > 0 ? 'incomplete_finding' : 'no_finding' };
}
// list_findings is keyset-paginated and open-first. Stop as soon as its ranking
// yields the first display-complete open finding or the settled-history
// partition; the repeated-cursor guard prevents a broken response from turning
// the overview read into an infinite client loop.
export async function firstEvidenceBackedFinding(
  page: (cursor?: string) => Promise<ListFindingsResult>,
): Promise<AgentRecommendation | null> {
  let cursor: string | undefined;
  const cursors = new Set<string>();
  for (;;) {
    const result = await page(cursor);
    for (const candidate of result.findings ?? []) {
      if (candidate.status !== 'open') return null;
      if (isEvidenceBackedFinding(candidate)) return candidate;
    }
    if (!result.next_cursor) return null;
    if (cursors.has(result.next_cursor)) throw new Error('Findings pagination cursor repeated');
    cursors.add(result.next_cursor);
    cursor = result.next_cursor;
  }
}

// The 44px hit-area contract is flow-scoped: shared controls stay compact
// elsewhere, so the overview wraps its controls and raises the interactive
// descendants rather than resizing every consumer of Button/Segment/Selector.
const TARGET_44 = '[&_button]:min-h-[44px] [&_[role=radio]]:min-h-[44px] [&_[role=combobox]]:min-h-[44px]';

export function OverviewPage() {
  const projectID = useAuthStore((s) => s.project?.id);
  const [period, setPeriod] = useState('7d');
  const [platform, setPlatform] = useState('');

  const { names: eventNames, loading: catalogLoading, error: catalogError } = useEventNames();
  // A failed catalog fetch is not an empty catalog — gating on success keeps
  // a transient error from forcing the first-run setup state.
  const catalogReady = !catalogLoading && !catalogError && !!projectID;
  const firstValue = firstValuePath({ eventNames, catalogReady });

  const query = useQuery({
    queryKey: ['overview', projectID, period, platform],
    queryFn: () => new AgentRayAPI(projectID!).overview(period, platform),
    enabled: !!projectID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
  const res = query.data ?? null;

  const viewState = overviewViewState({
    projectID,
    isLoading: query.isLoading,
    error: query.error,
    res,
    showFirstEvent: firstValue.showFirstEvent,
    platform,
    period,
  });

  // The value-first panel belongs to the grouped state, so the findings read is
  // gated on it: a project with no access or no qualifying activity should not
  // spend a request on a panel it will not show. A Plans failure degrades this
  // panel alone — the numbers above come from a separate read.
  const showNextStep = viewState === 'data';
  const findingsQuery = useQuery({
    queryKey: ['overview-findings', projectID],
    queryFn: () => {
      const api = new AgentRayAPI(projectID!);
      return firstEvidenceBackedFinding((cursor) => api.listFindings({ cursor, limit: 50 }));
    },
    enabled: !!projectID && showNextStep,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
  const nextStep = bestNextStep(findingsQuery.data ? [findingsQuery.data] : undefined, findingsQuery.isError);

  const freshness = res ? freshnessLabel(res) : null;
  const occurredAt = res?.data_status.last_event_at
    ? new Date(res.data_status.last_event_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC'
    : 'not available';
  const receivedAt = res?.data_status.last_received_at
    ? new Date(res.data_status.last_received_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC'
    : 'not available';
  const stats = res
    ? [
        metricStat(res, 'Active people', res.metrics.active_users),
        metricStat(res, 'Sessions', res.metrics.sessions),
        metricStat(res, 'New people', res.metrics.new_users),
      ]
    : [];
  const monetizationStats = res
    ? [
      revenueStat(res),
      unservedStat(res, 'Purchases'),
      unservedStat(res, 'Subscriptions'),
    ]
    : [];
  const usageStats = res
    ? [
        metricStat(res, 'Activation', res.metrics.activation),
        retentionStat(res, 'D1 retention', 1, res.retention.d1),
        retentionStat(res, 'D7 retention', 7, res.retention.d7),
        retentionStat(res, 'D30 retention', 30, res.retention.d30),
        unservedStat(res, 'Crashes'),
      ]
    : [];

  // The Acquisition group's tile row: New people leads it (§Metric groups
  // names it first under Acquisition), with the ranked pageview lists below.
  const acquisition = res ? acquisitionStats(res) : [];

  const trendSpec = useMemo(() => {
    if (!res || res.trend.length === 0 || trendMeaning(res) !== 'data') return null;
    return {
      type: 'area' as const,
      x: res.trend.map((p) => p.day),
      series: [{ name: 'Active people', data: res.trend.map((p) => p.active_users) }],
      smooth: false,
      integerY: true,
      height: 220,
    };
  }, [res]);
  const trend = res ? trendMeaning(res) : 'empty';

  const headerRange = res ? rangeLabel(res) : '';

  const dataStatusPanel = res ? (
    <Panel title="Data status">
      <div className="flex flex-wrap gap-x-8 gap-y-2 text-sm">
        <span>{formatCompact(res.data_status.events_in_range)} events in range</span>
        <span>{formatCompact(res.data_status.qualifying_in_range)} qualifying (human product activity)</span>
        <span>Last occurred: {occurredAt}</span>
        <span>Last received: {receivedAt}</span>
        <span className="text-[var(--color-text-secondary)]">
          Pipeline lag: {res.data_status.pipeline_lag === 'unavailable' ? 'not measured yet' : res.data_status.pipeline_lag}
        </span>
        <span className="text-[var(--color-text-secondary)]">
          Schema health: {res.data_status.schema_status === 'unavailable' ? 'not measured yet' : res.data_status.schema_status}
        </span>
      </div>

      {res.data_status.sources.length === 0 ? (
        <p className="mt-3 text-sm text-[var(--color-text-secondary)]">No connected data sources.</p>
      ) : (
        <div className="mt-3 flex flex-col gap-2">
          {res.data_status.sources.map((source) => (
            <div key={source.sync_id || source.connector_id} className="border-t border-[var(--color-border)] pt-2 text-sm">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span className="font-medium">{source.connector_name}</span>
                <span className="text-[var(--color-text-secondary)]">{source.source_table || 'No table configured'}</span>
                <StatusPill status={sourcePill(source.state).status} label={sourcePill(source.state).label} grow={false} />
              </div>
              {source.sync_configured ? (
                <p className="mt-1 text-xs text-[var(--color-text-secondary)]">
                  Last success {source.last_success_at ? new Date(source.last_success_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : 'never'} · last attempt {source.last_run_at ? new Date(source.last_run_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : 'never'} · resume cursor <span className="font-mono">{source.cursor || '—'}</span>{source.cursor_key ? <> (<span className="font-mono">{source.cursor_key}</span>)</> : null}
                </p>
              ) : null}
              {source.last_error ? <p className="mt-1 text-xs text-[var(--color-text-secondary)]">Last error: {source.last_error}</p> : null}
            </div>
          ))}
          {res.data_status.sources_truncated ? <p className="text-xs text-[var(--color-text-secondary)]">Showing the first 20 connected sources.</p> : null}
        </div>
      )}
    </Panel>
  ) : null;

  // A refetch that fails while earlier data is cached is not an error state —
  // the read keeps the last good figures — but it must not be invisible either.
  // The figures stay, stamped with the moment they were computed, and the
  // failure (with the missing role on a 403) is named with a retry.
  const refetchError = res ? query.error : null;
  const refetchStamp = res ? new Date(res.context.generated_at).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : '';
  const refetchForbidden = refetchError instanceof APIError && refetchError.status === 403;
  const refetchCallout = refetchError ? (
    <Callout
      tone="warn"
      icon={<AlertTriangle size={16} />}
      label="Refresh failed"
      title={refetchForbidden ? 'You no longer have analytics-read access' : 'Could not refresh the overview'}
      detail={
        refetchForbidden
          ? `The latest request was refused because this account is missing the analytics-read role on this workspace. The figures below are the last successful read, as of ${refetchStamp}.`
          : `${refetchError instanceof Error ? refetchError.message : 'The overview request failed'}. The figures below are the last successful read, as of ${refetchStamp}.`
      }
      action={<Button variant="outline" size="sm" className="min-h-[44px]" icon={<RefreshCw size={14} />} onClick={() => void query.refetch()}>Retry</Button>}
    />
  ) : null;

  const freshnessCallout = freshness?.stale ? (
    <Callout
      tone="warn"
      icon={<Clock size={16} />}
      label="Data freshness"
      title={freshness.text}
      detail="Numbers below cover the selected range but the source has gone quiet — check that events are still being sent."
    />
  ) : null;

  // Trust metadata: every metric's definition and notes stay one disclosure
  // away — the numbers are only as honest as what they exclude.
  const definitions = res ? (
    <details className="text-xs text-[var(--color-text-secondary)]">
      <summary className="cursor-pointer select-none py-3">How these numbers are computed</summary>
      <dl className="mt-2 flex flex-col gap-2">
        {([
          ['Active people', res.metrics.active_users],
          ['New people', res.metrics.new_users],
          ['Sessions', res.metrics.sessions],
          ['Activation', res.metrics.activation],
          ['Revenue', res.metrics.revenue],
        ] as Array<[string, OverviewMetric]>).map(([label, m]) => (
          <div key={label}>
            <dt className="font-medium text-[var(--color-text-primary)]">{label}</dt>
            <dd>{m.definition}</dd>
            {/* Meaningful text: --color-text-secondary, never --faint
                (--color-text-disabled fails WCAG AA on the card surface). */}
            {m.notes?.map((n) => <dd key={n} className="text-[var(--color-text-secondary)]">· {n}</dd>)}
          </div>
        ))}
      </dl>
    </details>
  ) : null;

  // The capability branch of the value-first panel (§Value-first story): when
  // no complete finding exists the page explains what the connected data makes
  // possible, with a clearly labeled example — never a fabricated live number,
  // an upgrade CTA, a price, or an ROI claim.
  const capabilityExplanation = (reason: 'no_finding' | 'incomplete_finding' | 'unavailable') => (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-[var(--color-text-secondary)]">
        {reason === 'unavailable'
          ? 'Findings are unavailable right now. The numbers above are unaffected.'
          : reason === 'incomplete_finding'
            ? 'A finding exists but its evidence is not readable yet. Once your agent has read enough of this project it files a complete one here — the observation, the comparison behind it, and the evidence line.'
            : 'No complete finding yet. Once your agent has read enough of this project it files one here — the observation, the comparison behind it, and the evidence line.'}
      </p>
      {/* A labeled example, never a live number: the panel explains what a
          finding looks like without claiming this project has one. */}
      <div className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-background-muted)] p-3">
        <p className="text-2xs uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">Example — not your data</p>
        <p className="mt-1 text-sm text-[var(--color-text-secondary)]">“Activation fell 12% week over week, driven by the signup → first-project step.”</p>
      </div>
      <div className={`flex flex-wrap items-center gap-3 ${TARGET_44}`}>
        <Button variant="outline" size="sm" icon={<ArrowUpRight size={14} />} onClick={() => { window.location.href = settingsPath('ai'); }}>Connect your agent (MCP)</Button>
        <Button variant="outline" size="sm" onClick={() => { window.location.href = '/chat'; }}>Ask in chat</Button>
      </div>
    </div>
  );

  // The unified empty the receipt_only/empty states share: the chart slot says
  // why it is flat without pretending a group of state tiles is a measurement.
  const noQualifyingActivity = (
    <EmptyState
      title="Connected — no qualifying activity yet"
      detail="Events are arriving, but none count as human product activity in this range (verification pings, bots, and agent events are excluded). The trend draws once real usage lands."
    />
  );

  return (
    <AppShell>
      <PageShell
        title="Overview"
        sub={headerRange || 'The last complete days, at a glance.'}
        actions={
          <div className={`flex flex-wrap items-center gap-2 ${TARGET_44}`}>
            {/* Segment, not Selector: the Astryx Selector trigger renders
                tabindex=-1, so a keyboard user can never reach it. The
                segmented control is a real radio group — Tab reaches it,
                arrows move between options — and it wraps on mobile. */}
            <Segment
              options={[{ value: 'all', label: 'All platforms' }, ...PLATFORM_OPTIONS]}
              value={platform || 'all'}
              onChange={(v) => setPlatform(v === 'all' ? '' : v)}
              label="Platform"
            />
            <Segment options={PERIODS} value={period} onChange={setPeriod} label="Time range" />
          </div>
        }
      >
        {/* §Layout 2: the freshness line is a StatusPill — word + dot, never
            color alone. It sits under the header whenever a result exists. */}
        {freshness ? (
          <div className="flex">
            <StatusPill
              status={!res?.data_status.ever_received ? 'idle' : freshness.stale ? 'attention' : 'healthy'}
              label={freshness.word === freshness.text ? freshness.word : `${freshness.word} · ${freshness.text}`}
            />
          </div>
        ) : null}

        {viewState === 'loading' ? (
          <div role="status" aria-label="Loading overview" className="flex flex-col gap-4">
            <Loading label="Loading overview…" />
            <Panel title="Active people per day"><Loading label="" /></Panel>
            <Panel title="Retention"><Loading label="" /></Panel>
          </div>
        ) : null}

        {viewState === 'no_access' ? (
          <Callout
            tone="warn"
            icon={<Lock size={16} />}
            label="No access"
            title="You cannot read this project's analytics"
            detail="The overview needs analytics-read access on this workspace. Ask a workspace owner or admin to grant it, then reload."
          />
        ) : null}

        {viewState === 'error' ? (
          <>
            <Callout
              tone="warn"
              icon={<AlertTriangle size={16} />}
              label="Overview unavailable"
              title="Could not load the overview"
              detail={query.error instanceof Error ? query.error.message : 'The overview request failed.'}
              action={<Button variant="outline" size="sm" className="min-h-[44px]" icon={<RefreshCw size={14} />} onClick={() => void query.refetch()}>Retry</Button>}
            />
            {/* §States: the data status panel notes its own unavailability
                rather than disappearing with the rest of the page. */}
            <Panel title="Data status">
              <p className="text-sm text-[var(--color-text-secondary)]">Source health is unavailable while the overview cannot load.</p>
            </Panel>
          </>
        ) : null}

        {refetchCallout}

        {viewState === 'first_run' ? (
          <>
            <FirstEventQuickstart />
            {/* §Value-first story names the first-run state explicitly: the
                capability explanation renders here too, with the labeled
                example — never a fabricated number. */}
            <Panel title="What this overview will show you">
              {capabilityExplanation('no_finding')}
            </Panel>
            {/* A first-run project with a result still gets its data status:
                "nothing has arrived" is a receipt fact worth showing, not a
                panel to hide behind the quickstart. */}
            {dataStatusPanel}
          </>
        ) : null}

        {viewState === 'filtered_empty' && res ? (
          <>
            <EmptyState
              title="No events match this filter"
              detail={platform ? `Nothing arrived for ${platformLabel(platform)} in the selected range.` : 'Nothing arrived in the selected range.'}
              action={
                <Button variant="outline" size="sm" className="min-h-[44px]" onClick={() => { setPlatform(''); setPeriod('7d'); }}>
                  Reset to all platforms, 7 days
                </Button>
              }
            />
            {dataStatusPanel}
          </>
        ) : null}

        {/* The three metric groups are the `grouped_metrics` modifier: they
            render only when qualifying activity exists. receipt_only keeps its
            "no qualifying activity" panel and empty keeps an honest empty,
            rather than three groups of state tiles that look like metrics. */}
        {viewState === 'receipt_only' && res ? (
          <>
            {freshnessCallout}
            <StatsStrip stats={stats} />
            {definitions}
            <Panel title="Active people per day">{noQualifyingActivity}</Panel>
            {dataStatusPanel}
          </>
        ) : null}

        {viewState === 'empty' && res ? (
          <>
            <EmptyState title="No activity in this range" detail="Qualifying human events will draw the trend once they arrive." />
            {dataStatusPanel}
          </>
        ) : null}

        {viewState === 'data' && res ? (
          <>
            {freshnessCallout}

            <StatsStrip stats={stats} />

            {definitions}

            {/* §Layout 5: the trend + retention grid sits above the groups —
                the order both prototypes and this doc's item list share. */}
            <div className="grid grid-cols-3 gap-4 [@media(max-width:980px)]:grid-cols-1">
              <div className="col-span-2 [@media(max-width:980px)]:col-span-1">
                <Panel title="Active people per day">
                  {trendSpec ? (
                    <>
                      <Chart spec={trendSpec} />
                      {/* Textual equivalent: the chart is the shape, this is the data. */}
                      <p className="mt-2 text-xs text-[var(--color-text-secondary)]">
                        {res.trend.map((p) => `${p.day.slice(5)}: ${p.active_users}`).join(' · ')}
                      </p>
                    </>
                  ) : trend === 'receipt_only' ? (
                    noQualifyingActivity
                  ) : (
                    <EmptyState title="No activity in this range" detail="Qualifying human events will draw the trend once they arrive." />
                  )}
                </Panel>
              </div>
              <Panel title="Retention">
                <div className="flex flex-col gap-2 text-sm">
                  <p>{retentionLine('Day 1', res.retention.d1)}</p>
                  <p>{retentionLine('Day 7', res.retention.d7)}</p>
                  <p>{retentionLine('Day 30', res.retention.d30)}</p>
                  <p className="text-xs text-[var(--color-text-secondary)]">
                    {res.retention.cohort_window === 'lifetime' ? 'Lifetime cohorts — a person counts from their first-ever event, not the selected range.' : `Cohort window: ${res.retention.cohort_window}`}
                  </p>
                </div>
              </Panel>
            </div>

            {/* §Metric groups order: Acquisition → Monetization → Usage. */}
            <div className="grid grid-cols-2 gap-4 [@media(max-width:980px)]:grid-cols-1">
              <MetricGroup
                title="Acquisition"
                action={<Button variant="outline" size="sm" className="min-h-[44px]" onClick={() => { window.location.href = '/acquisition'; }}>See more</Button>}
              >
                <div className="flex flex-col gap-4">
                  <StatsStrip stats={acquisition} />
                  <p className="text-xs text-[var(--color-text-secondary)]">New people is the Overview’s first-observed metric. These ranked pageview lists add its real acquisition context; direct / unknown remains visible.</p>
                  <div className="grid grid-cols-2 gap-4 [@media(max-width:700px)]:grid-cols-1">
                    <div>
                      <h3 className="mb-2 text-sm font-medium">Top pages</h3>
                      <BarRows
                        rows={res.content.top_pages.rows}
                        valueHead="Page"
                        countHead={res.content.top_pages.unit}
                        mono
                        empty="No pageviews in this range"
                      />
                    </div>
                    <div>
                      <h3 className="mb-2 text-sm font-medium">Top sources</h3>
                      <BarRows
                        rows={res.content.top_sources.rows}
                        valueHead="Source"
                        countHead={res.content.top_sources.unit}
                        empty="No attributed sources in this range"
                      />
                    </div>
                  </div>
                  <p className="text-xs text-[var(--color-text-secondary)]">Unverified acquisition metrics are Not available: AgentRay shows only project-scoped pageviews and attributed sources.</p>
                </div>
              </MetricGroup>

              <MetricGroup
                title="Monetization"
                action={<Button variant="outline" size="sm" className="min-h-[44px]" onClick={() => { window.location.href = '/monetization'; }}>See more</Button>}
              >
                <div className="flex flex-col gap-3">
                  <StatsStrip stats={monetizationStats} />
                  {/* The required instrumentation is named on the group, not
                      implied by a bare Set up: revenue needs a trusted,
                      deduplicated billing source; purchases and subscriptions
                      need their own verified contracts. */}
                  <p className="text-xs text-[var(--color-text-secondary)]">Instrumented revenue requires a trusted, deduplicated server or billing source with a declared currency. Purchases and subscriptions need their own verified project-scoped metric contracts; no SDK event total is shown as money.</p>
                  <RevenueBreakdown metric={res.metrics.revenue} detail={res.metrics.revenue_detail} />
                </div>
              </MetricGroup>

              <MetricGroup
                title="Usage"
                action={<Button variant="outline" size="sm" className="min-h-[44px]" onClick={() => { window.location.href = '/usage'; }}>See more</Button>}
              >
                <div className="flex flex-col gap-4">
                  <StatsStrip stats={usageStats} />
                  <p className="text-xs text-[var(--color-text-secondary)]">Crashes are Not available until AgentRay receives a verified crash event with a normalized app-version contract.</p>
                </div>
              </MetricGroup>

              {/* §Value-first story: one panel, two honest branches — a
                  display-complete finding, or the capability explanation with
                  a labeled example. Never omitted, never a fabricated number. */}
              {findingsQuery.isLoading ? (
                <Panel title="Best next step"><Loading label="Loading the latest finding…" /></Panel>
              ) : nextStep.kind === 'finding' ? (
                <Panel title="Best next step">
                  <div className="flex flex-col gap-2">
                    <p className="text-sm font-medium">{nextStep.title}</p>
                    <p className="text-sm text-[var(--color-text-secondary)]">{nextStep.observation}</p>
                    {/* Provenance: the envelope behind the claim, rendered by the
                        same helper /plans uses so the two never drift. */}
                    <p className="font-mono text-xs text-[var(--color-text-secondary)]">{nextStep.evidence}</p>
                    <div className={`flex flex-wrap items-center gap-3 ${TARGET_44}`}>
                      <Button variant="outline" size="sm" icon={<ArrowUpRight size={14} />} onClick={() => { window.location.href = '/plans'; }}>Open the finding</Button>
                      <Button variant="outline" size="sm" onClick={() => { window.location.href = '/chat'; }}>Ask your agent to investigate</Button>
                    </div>
                  </div>
                </Panel>
              ) : (
                <Panel title="Best next step">
                  {capabilityExplanation(nextStep.reason)}
                </Panel>
              )}
            </div>

            {dataStatusPanel}
          </>
        ) : null}
      </PageShell>
    </AppShell>
  );
}
