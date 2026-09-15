import type { AgentRecommendation, MeasuredTest, TestOutcomeEntry } from '@/lib/api';
import { formatDate } from '@/lib/format';

// plans.ts — the rules /plans reads from. Pure, so the list, the detail page
// and any test can call exactly what the UI calls.
//
// A PLAN is one of two rows: a finding (agent_recommendations — something the
// agent noticed and wrote down) or an experiment (validation_tests — a
// hypothesis with a kill/keep number agreed before the data arrives). The
// experiment state machine itself lives in modules/prototypes/lib/prototype —
// this file adds only what the resumable-experiment fields need.

// outcomeEntries parses the append-only outcome list. Entries are immutable —
// a correction is a new entry — so the UI renders them oldest-first and never
// offers an edit affordance.
export function outcomeEntries(json: string | undefined): TestOutcomeEntry[] {
  if (!json) return [];
  try {
    const parsed = JSON.parse(json);
    return Array.isArray(parsed) ? (parsed as TestOutcomeEntry[]) : [];
  } catch {
    return [];
  }
}

// queryRefPart renders one query_ref — the reproducible spec
// {kind: saved_query|metric|sql, id_or_definition, version} (store/validation.go)
// — as the identity a reader can re-run. A bare id that can rot is not
// provenance, and neither is a truncated definition: the identifier or SQL is
// printed verbatim, untruncated, so "which query produced this number" has an
// answer that still resolves later.
function queryRefPart(ref: unknown): string | null {
  if (typeof ref === 'string') return ref || null;
  if (!ref || typeof ref !== 'object' || Array.isArray(ref)) return null;
  const { kind, id_or_definition: definition, version } = ref as Record<string, unknown>;
  const identity: string[] = [];
  if (typeof kind === 'string' && kind) identity.push(kind.replace(/_/g, ' '));
  if (typeof definition === 'string' && definition) identity.push(definition);
  else if (typeof definition === 'number') identity.push(String(definition));
  if (typeof version === 'string' && version) identity.push(`v${version}`);
  else if (typeof version === 'number') identity.push(`v${version}`);
  return identity.length ? identity.join(' ') : null;
}

// evidenceParts parses a finding's evidence envelope into the provenance
// fields the reader can check. The envelope is the typed contract the ops
// document — {query_ref, metric_version, dataset_version, range, filters,
// timezone, watermark, warnings}. A row that predates the envelope, or whose
// envelope carries none of those fields, yields no parts.
function evidenceParts(rec: { evidence_json?: string }): string[] {
  const raw = rec.evidence_json?.trim();
  if (!raw) return [];
  try {
    const env = JSON.parse(raw) as Record<string, unknown>;
    if (!env || typeof env !== 'object' || Array.isArray(env)) return [];
    const parts: string[] = [];
    const ref = queryRefPart(env.query_ref ?? env.query_id);
    if (ref) parts.push(ref);
    if (typeof env.range === 'string' && env.range) parts.push(env.range);
    if (typeof env.metric_version === 'string' && env.metric_version) parts.push(`metric ${env.metric_version}`);
    if (typeof env.dataset_version === 'string' && env.dataset_version) parts.push(`dataset ${env.dataset_version}`);
    if (typeof env.watermark === 'string' && env.watermark) parts.push(`watermark ${env.watermark}`);
    if (typeof env.timezone === 'string' && env.timezone) parts.push(env.timezone);
    if (env.filters !== undefined && env.filters !== null && env.filters !== '') {
      parts.push(`filters: ${typeof env.filters === 'string' ? env.filters : JSON.stringify(env.filters)}`);
    }
    if (Array.isArray(env.warnings)) {
      for (const w of env.warnings) {
        if (typeof w === 'string' && w) parts.push(`warning: ${w}`);
      }
    }
    return parts;
  } catch {
    return [];
  }
}

// evidenceAvailable reports whether a finding carries provenance a reader can
// check. It is the same parse evidenceLine renders, so a caller deciding
// whether a finding is display-complete can never disagree with the line the
// panel would print: an envelope full of unrelated keys is not evidence.
export function evidenceAvailable(rec: { evidence_json?: string }): boolean {
  return evidenceParts(rec).length > 0;
}

// evidenceLine renders a finding's evidence envelope in one line. Every field
// it carries renders here, because a provenance field that disappears is a
// claim the reader cannot check. Rows written before the envelope existed
// carry no evidence_json — the honest read is "evidence unavailable" with the
// recording date, never an invented summary.
export function evidenceLine(rec: { evidence_json?: string; created_at: string }): string {
  const parts = evidenceParts(rec);
  return parts.length ? parts.join(' · ') : `evidence unavailable — recorded ${formatDate(rec.created_at) || 'earlier'}`;
}

// baselineLine is the "baseline → target" readout for one experiment: the
// measured baseline when the row carries one, else the denominator event.
export function baselineLine(t: MeasuredTest): string {
  const target = `${t.target_count} ${t.metric_event} in ${t.window_days}d`;
  if (typeof t.baseline_value === 'number') {
    const unit = t.baseline_unit ? ` ${t.baseline_unit}` : '';
    const window = t.baseline_window ? ` (${t.baseline_window})` : '';
    return `${t.baseline_value}${unit}${window} → ${target}`;
  }
  if (t.baseline_event) return `${t.baseline_event} → ${target}`;
  return target;
}

// Finding status vocabulary. StatusPill's four states, mapped so a finding's
// pill reads like every other pill in the product.
export const FINDING_PILL: Record<string, string> = {
  open: 'attention',
  accepted: 'healthy',
  dismissed: 'paused',
};

export const FINDING_LABEL: Record<string, string> = {
  open: 'Open',
  accepted: 'Accepted',
  dismissed: 'Dismissed',
};

// Experiment status label for the list — the same words the prototypes surface
// uses, plus the proposed state's owner-facing meaning.
export const EXPERIMENT_LABEL: Record<string, string> = {
  proposed: 'Awaiting owner agreement',
  committed: 'Committed',
  passed: 'Passed',
  failed: 'Failed',
  abandoned: 'Abandoned',
  inconclusive: 'Inconclusive',
};

export const EXPERIMENT_PILL: Record<string, string> = {
  proposed: 'working',
  committed: 'idle',
  passed: 'healthy',
  failed: 'attention',
  abandoned: 'paused',
  inconclusive: 'paused',
};

// ---- Goal-seeded first experiment (007) ------------------------------------
// When the owner answered the onboarding goal prompt but no validation_tests
// row exists yet, /plans renders a suggestion card — deliberately NOT a
// propose_test row: a proposed test needs a real metric_event + target_count
// the owner agreed to, and fabricating one writes a threshold nobody chose.

export type GoalSuggestion = {
  goal: string;
  title: string;
  body: string;
  metricLine: string;
  prompt: string;
};

// goalSuggestion maps the stored project goal to the first experiment worth
// shaping with the agent. 'skipped' and unknown values yield null — a declined
// goal seeds nothing.
export function goalSuggestion(project: { goal?: string; activation_event?: string } | null | undefined): GoalSuggestion | null {
  const goal = project?.goal;
  if (!goal || goal === 'skipped') return null;
  const act = project?.activation_event?.trim();
  switch (goal) {
    case 'activation':
      return {
        goal,
        title: 'Lift activation',
        body: act
          ? `More new signups firing ${act} within their first week.`
          : 'More new signups reaching the moment the product proves itself within their first week.',
        metricLine: act
          ? `Metric: share of the weekly signup cohort reaching ${act} · Baseline: measured on commit`
          : 'Metric: share of the weekly signup cohort reaching the activation event · Baseline: measured on commit',
        prompt: act
          ? `My goal is activation. Design the cheapest test that lifts the share of new signups firing ${act} within their first week.`
          : 'My goal is activation. Design the cheapest test that lifts the share of new signups reaching first value within their first week.',
      };
    case 'retention':
      return {
        goal,
        title: 'Lift retention',
        body: 'More new users coming back on day 1 and day 7 after their first session.',
        metricLine: 'Metric: D1/D7 return rate of the weekly first-activity cohort · Baseline: measured on commit',
        prompt: 'My goal is retention. Design the cheapest test that lifts day-1 and day-7 return rates for new users.',
      };
    case 'revenue':
      return {
        goal,
        title: 'Lift revenue',
        body: 'More checkouts, upgrades, and net revenue from the same traffic.',
        metricLine: 'Metric: deduplicated net revenue per declared currency · Baseline: measured on commit',
        prompt: 'My goal is revenue. Design the cheapest test that lifts net revenue or paying conversion.',
      };
    case 'traffic':
      return {
        goal,
        title: 'Grow the right traffic',
        body: 'More sessions from the referrer channels that actually convert.',
        metricLine: 'Metric: sessions and new people by referrer channel · Baseline: measured on commit',
        prompt: 'My goal is traffic. Design the cheapest test that grows sessions from the channels that convert.',
      };
    default:
      return null;
  }
}
