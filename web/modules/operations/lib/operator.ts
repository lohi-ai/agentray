import type { Operator } from '@/lib/api';
import { cronToWords } from './cron-words';

// operator.ts — the rules /operations reads from. Pure, so the list, the mobile
// card and the detail page cannot drift into three different answers about the
// same row, and so the rules are testable without a browser.

export type OperatorStatus = {
  // Matches StatusPill's vocabulary and the agent fleet's four states, so the
  // two fleet screens speak one language (modules/agents/monitor).
  status: 'attention' | 'working' | 'healthy' | 'paused';
  label: string;
};

/**
 * operatorStatus is the single rule the whole screen reads from.
 *
 * The order matters. A trigger the owner switched off is paused whatever the
 * history says. An armed trigger on a PAUSED AGENT is the state the product
 * most easily lies about: the trigger fires and nothing runs, so reporting it
 * as "Armed" tells the owner work is happening when none is.
 */
export function operatorStatus(op: Operator): OperatorStatus {
  if (!op.enabled) return { status: 'paused', label: 'Paused' };
  if (!op.agent_enabled) return { status: 'paused', label: 'Teammate paused' };
  if (op.consecutive_failures > 0) return { status: 'attention', label: 'Attention' };
  if (op.running_count > 0) return { status: 'working', label: 'Working' };
  return { status: 'healthy', label: 'Armed' };
}

// rank floats failures and active work to the top. Same shape as rank() on the
// agent fleet screen, so the two lists order alike.
export function rank(op: Operator): number {
  const { status } = operatorStatus(op);
  if (status === 'attention') return 0;
  if (status === 'working') return 1;
  if (status === 'healthy') return 2;
  return 3;
}

// operatorTitle is what the row is called. A cron expression is never a title:
// an unnamed trigger borrows the name of whoever answers it, which is at least
// something the owner recognises.
export function operatorTitle(op: Operator): string {
  const named = (op.name ?? '').trim();
  if (named) return named;
  const runner = runnerLabel(op);
  return op.kind === 'webhook' ? `${runner} webhook` : `${runner} schedule`;
}

// runnerLabel names who actually does the work. A trigger whose agent is a
// team's lead runs the whole team through the runtime's team lead, so the team
// is the honest answer — naming the lead agent would credit one member for
// work the group does.
export function runnerLabel(op: Operator): string {
  return (op.team_name ?? '').trim() || op.agent_name || 'Unassigned';
}

export function isTeamRun(op: Operator): boolean {
  return !!(op.team_name ?? '').trim();
}

/**
 * startsOn is the "Starts on" cell: when a schedule fires, or the ingress path
 * a webhook listens on. Manual-only operators do not exist yet — every row here
 * has a trigger by definition — so there is no "on demand" case to invent.
 */
export function startsOn(op: Operator): string {
  if (op.kind === 'webhook') return webhookPath(op.webhook_token);
  return cronToWords(op.cron) || 'No schedule set';
}

// webhookPath shortens the ingress address for a table cell. The full URL is on
// the detail page with a copy control — a truncated URL must never be
// copy-pasteable, or someone will paste a broken one and debug it for an hour.
export function webhookPath(token: string): string {
  const t = (token ?? '').trim();
  if (!t) return 'No address yet';
  return `POST /hook/…${t.slice(-4)}`;
}

// lastRunLine states the outcome in words. `shared_history` is surfaced rather
// than hidden: a run records the CHANNEL that started it, not which trigger, so
// when one agent has two schedules these numbers describe both of them.
export function lastOutcome(op: Operator): { text: string; failed: boolean } {
  if (!op.last_status) return { text: 'Never run', failed: false };
  if (op.last_status === 'running') return { text: 'Working now', failed: false };
  if (op.last_status === 'error') {
    const streak = op.consecutive_failures;
    const detail = (op.last_summary ?? '').trim();
    const prefix = streak > 1 ? `Failed ${streak} runs in a row` : 'Failed';
    return { text: detail ? `${prefix} — ${detail}` : prefix, failed: true };
  }
  const summary = (op.last_summary ?? '').trim();
  return { text: summary || 'Completed', failed: false };
}

// A config-sourced row is the legacy project schedule. It is listed so the page
// is honest about everything that runs, but its cron and its on/off live on the
// project's agent config — rewriting that from here would change how every
// unattended run in the project is gated.
export function isEditableHere(op: Operator): boolean {
  return op.source !== 'config';
}
