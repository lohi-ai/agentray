import type { MeasuredTest } from '@/lib/api';

// prototype.ts — the rules /prototypes reads from. Pure, so the list, the card
// and the detail page cannot disagree about what a prototype's state is.
//
// A PROTOTYPE is one falsifiable bet on one idea: a hypothesis, the success
// event, the count agreed IN ADVANCE, the window, and the verdict. It is exactly
// one validation_tests row — the concept already existed, it just had room for
// one at a time.

export type PrototypeState = 'proposed' | 'running' | 'passed' | 'failed' | 'abandoned';

/**
 * stateOf reads the row's state from the SERVER's verdict, never from a local
 * recomputation.
 *
 * That is the whole discipline of this feature. The threshold is agreed before
 * the data arrives so that "did it work" is a lookup rather than an argument —
 * and a page that decides for itself whether a count clears the bar reintroduces
 * exactly the disagreement the row exists to prevent, this time between the
 * screen and the agent answering about the same test in chat.
 */
export function stateOf(t: MeasuredTest): PrototypeState {
  if (t.status === 'proposed') return 'proposed';
  if (t.status === 'passed') return 'passed';
  if (t.status === 'failed') return 'failed';
  if (t.status === 'abandoned') return 'abandoned';
  // Committed: the server's verdict says whether the window has already settled
  // it. `committed` back means still running — too early to call.
  if (t.verdict === 'passed') return 'passed';
  if (t.verdict === 'failed') return 'failed';
  return 'running';
}

// The verdict in words. Color never carries it alone.
export const STATE_LABEL: Record<PrototypeState, string> = {
  proposed: 'Waiting on you',
  running: 'Running',
  passed: 'Passed',
  failed: 'Failed',
  abandoned: 'Abandoned',
};

// StatusPill's four states, mapped so a prototype's pill reads like every other
// pill in the product rather than inventing a fifth vocabulary.
export const STATE_PILL: Record<PrototypeState, string> = {
  proposed: 'working',
  running: 'idle',
  passed: 'healthy',
  failed: 'attention',
  abandoned: 'paused',
};

// Left-rule and progress-bar color. Tokens only — never a hex.
export const STATE_TONE: Record<PrototypeState, string> = {
  proposed: 'var(--agent)',
  // --data rather than a neutral border token: a running test is a measurement
  // in progress, and its bar has to stay readable at 10% fill on the dark ramp.
  running: 'var(--data)',
  passed: 'var(--success)',
  failed: 'var(--danger)',
  abandoned: 'var(--color-text-disabled)',
};

// isRecorded is whether the OWNER has closed the test, as opposed to the number
// having settled it. The gap between the two matters: a committed test whose
// window closed short of target already reads "Failed", but until the owner
// writes down which of the three failures it was, the most useful thing the
// product can do is ask.
export function isRecorded(t: MeasuredTest): boolean {
  return t.status === 'passed' || t.status === 'failed' || t.status === 'abandoned';
}

// Exactly one action per card, and it is the next honest move: agree to the
// number, watch it, close it out, build the thing, or find out which of the
// three failures it was.
export function primaryAction(t: MeasuredTest): string {
  const state = stateOf(t);
  if (state === 'proposed') return 'Commit to this number';
  if (state === 'running') return 'Open';
  if (!isRecorded(t)) return 'Record the decision';
  if (state === 'passed') return 'Build it';
  if (state === 'failed') return 'Ask which it was';
  return 'Open';
}

/**
 * progressPct is only defined for a MEASURED test. A proposal counts nothing —
 * the window opens at commit — so it has no bar, rather than a bar pinned at 0%
 * against a number nobody agreed to.
 */
export function progressPct(t: MeasuredTest): number | null {
  if (!t.measured || t.target_count <= 0) return null;
  return Math.min(100, (t.metric_count / t.target_count) * 100);
}

// countsLine is the row of facts under the hypothesis. It reports only what is
// true of this state: an uncommitted test gets the proposal's terms, not a score.
export function countsLine(t: MeasuredTest, waitlistCount: number): string {
  if (!t.measured) {
    return `Proposes ${t.target_count} people in ${t.window_days} days`;
  }
  const parts = [`${t.metric_count} of ${t.target_count}`];
  // Days left is only true of a test still counting. A decided one keeps a
  // positive days_left (the window outlives the verdict), and printing it under
  // a "Passed" pill reads as though the number could still move.
  if (stateOf(t) === 'running' && t.days_left > 0) {
    parts.push(`${t.days_left} ${t.days_left === 1 ? 'day' : 'days'} left`);
  }
  if (t.baseline_event && t.baseline_count > 0) {
    parts.push(`${Math.round((t.metric_count / t.baseline_count) * 100)}% of ${t.baseline_count}`);
  }
  // The waitlist is the project's, not the test's — only worth showing next to a
  // test that is actually counting waitlist joins.
  if (waitlistCount > 0 && t.metric_event.startsWith('waitlist.')) parts.push(`${waitlistCount} on the waitlist`);
  return parts.join(' · ');
}

// chatHref deep-links a question into the agent with the prompt pre-filled —
// the same move modules/start/components/idea-test.tsx makes.
export function chatHref(question: string): string {
  return `/chat?q=${encodeURIComponent(question)}`;
}

export function groupTests(tests: readonly MeasuredTest[]) {
  const waiting: MeasuredTest[] = [];
  const running: MeasuredTest[] = [];
  const decided: MeasuredTest[] = [];
  for (const t of tests) {
    const state = stateOf(t);
    if (state === 'proposed') waiting.push(t);
    else if (state === 'running') running.push(t);
    else decided.push(t);
  }
  return { waiting, running, decided };
}
