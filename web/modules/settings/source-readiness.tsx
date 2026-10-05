import type { SourceReadiness } from '@/lib/api';
import { StatusPill } from '@/modules/shared/components/lohi-evidence-primitives';

export type SourceReadinessView = {
  state: 'ready' | 'syncing' | 'empty' | 'stale' | 'error' | 'incomplete' | 'denied' | 'unknown';
  label: string;
  detail: string;
  captureInterval?: string;
};

export function evidenceTime(value: string | null | undefined): string {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function reasonLabel(reason: string | null | undefined): string {
  if (!reason) return '';
  const safe = reason.replace(/[_-]+/g, ' ').trim();
  return safe ? safe[0].toUpperCase() + safe.slice(1) : '';
}

export function sourceReadinessView(readiness: SourceReadiness | null | undefined, denied = false): SourceReadinessView {
  if (denied) return { state: 'denied', label: 'Read-only access', detail: 'Source readiness is not available with this credential.' };
  if (!readiness) return { state: 'unknown', label: 'Coverage not verified', detail: 'No queryability evidence was returned.' };
  const lastComplete = evidenceTime(readiness.last_complete_at);
  const started = evidenceTime(readiness.capture_started_at);
  const finished = evidenceTime(readiness.capture_finished_at);
  const captureInterval = started || finished ? `${started || 'Start unknown'} – ${finished || 'In progress'}` : undefined;
  switch (readiness.state) {
    case 'ready':
      return { state: 'ready', label: 'Ready to query', detail: lastComplete ? `Last complete ${lastComplete}` : 'Queryable; completion time unavailable.', captureInterval };
    case 'syncing':
      return { state: 'syncing', label: 'Syncing', detail: lastComplete ? `Previous complete sync ${lastComplete}` : 'No complete queryable sync yet.', captureInterval };
    case 'not_configured':
      return { state: 'empty', label: 'Not configured', detail: 'Configure and run this source before querying it.', captureInterval };
    case 'stale':
      return { state: 'stale', label: 'Stale', detail: lastComplete ? `Previous complete sync ${lastComplete}` : 'No timestamped complete result is available.', captureInterval };
    case 'incomplete':
      return { state: 'incomplete', label: 'Not fully queryable', detail: `Sync accepted; some rows are not available yet${readiness.reason ? ` — ${reasonLabel(readiness.reason)}` : ''}.`, captureInterval };
    case 'error':
      return { state: 'error', label: 'Readiness error', detail: reasonLabel(readiness.reason) || 'Queryable state could not be verified.', captureInterval };
    default:
      return { state: 'unknown', label: 'Coverage not verified', detail: 'The source returned an unknown readiness state.', captureInterval };
  }
}

export function SourceReadinessCell({ readiness, denied = false }: { readiness?: SourceReadiness | null; denied?: boolean }) {
  const view = sourceReadinessView(readiness, denied);
  const pillState = view.state === 'ready' ? 'ready' : view.state === 'syncing' ? 'working' : view.state === 'stale' || view.state === 'incomplete' ? 'attention' : view.state === 'error' ? 'error' : view.state === 'denied' ? 'denied' : 'idle';
  return <div className="lohi-source-readiness" aria-live="polite"><StatusPill status={pillState} label={view.label} grow={false} pulse={view.state === 'syncing'} /><span className="lohi-source-readiness__meta">{view.detail}</span>{view.captureInterval ? <span className="lohi-source-readiness__meta">Capture interval {view.captureInterval}</span> : null}</div>;
}

export function oldestCompleteAt(readiness: readonly SourceReadiness[]): string | null {
  return readiness.map((item) => item.last_complete_at).filter((item): item is string => !!item).sort().at(0) ?? null;
}

export function ConnectorReadinessSummary({ readiness, loading, denied = false, error = false }: { readiness: Array<SourceReadiness | null | undefined>; loading?: boolean; denied?: boolean; error?: boolean }) {
  if (denied) return <SourceReadinessCell denied />;
  if (error) return <SourceReadinessCell readiness={{ state: 'error', published_at: null, landed_at: null, queryable_at: null, generation: null, capture_started_at: null, capture_finished_at: null, reason: 'readiness request failed', last_complete_at: null }} />;
  if (loading && readiness.length === 0) return <div className="lohi-source-readiness" role="status" aria-live="polite"><StatusPill status="working" label="Checking readiness" grow={false} /></div>;
  if (readiness.length === 0) return <SourceReadinessCell readiness={{ state: 'not_configured', published_at: null, landed_at: null, queryable_at: null, generation: null, capture_started_at: null, capture_finished_at: null, reason: null, last_complete_at: null }} />;
  const present = readiness.filter((item): item is SourceReadiness => !!item);
  if (present.length !== readiness.length) return <SourceReadinessCell />;
  const states = new Set(present.map((item) => item.state));
  const state: SourceReadiness['state'] = states.has('error') ? 'error' : states.has('incomplete') ? 'incomplete' : states.has('stale') ? 'stale' : states.has('syncing') ? 'syncing' : present.every((item) => item.state === 'ready') ? 'ready' : 'not_configured';
  // The aggregate is only as fresh as its oldest dependency. Showing the
  // newest completion would hide a stale table behind a fresh sibling sync.
  const lastComplete = oldestCompleteAt(present);
  const captureStarted = present.map((item) => item.capture_started_at).filter((item): item is string => !!item).sort().at(0) ?? null;
  const captureFinished = present.map((item) => item.capture_finished_at).filter((item): item is string => !!item).sort().at(-1) ?? null;
  return <SourceReadinessCell readiness={{ state, published_at: null, landed_at: null, queryable_at: null, generation: null, capture_started_at: captureStarted, capture_finished_at: captureFinished, reason: present.find((item) => item.reason)?.reason ?? null, last_complete_at: lastComplete }} />;
}
