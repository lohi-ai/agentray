import { describe, expect, it } from 'vitest';
import type { MeasuredTest } from '@/lib/api';
import { countsLine, groupTests, isRecorded, primaryAction, progressPct, stateOf } from './prototype';

function test(over: Partial<MeasuredTest> = {}): MeasuredTest {
  return {
    id: 'v1',
    hypothesis: 'People will pay for scheduled digests',
    metric_event: 'waitlist.joined',
    baseline_event: 'landing.viewed',
    target_count: 50,
    window_days: 14,
    status: 'committed',
    decision_note: '',
    created_at: '2026-08-01T00:00:00Z',
    measured: true,
    metric_count: 12,
    baseline_count: 400,
    days_elapsed: 4,
    days_left: 10,
    ...over,
  };
}

describe('stateOf', () => {
  it('takes the verdict from the server, never from its own arithmetic', () => {
    // The whole discipline of this feature is that the threshold is agreed
    // before the data arrives. A page that re-scores the count reintroduces the
    // argument the row exists to prevent — this time between the screen and the
    // agent answering about the same test in chat.
    expect(stateOf(test({ metric_count: 999, verdict: 'committed' }))).toBe('running');
    expect(stateOf(test({ metric_count: 0, verdict: 'passed' }))).toBe('passed');
  });

  it('reads a proposal as waiting on the owner, whatever else is on the row', () => {
    expect(stateOf(test({ status: 'proposed', verdict: 'passed' }))).toBe('proposed');
  });

  it('keeps the owner’s recorded decision over any later verdict', () => {
    expect(stateOf(test({ status: 'abandoned', verdict: 'passed' }))).toBe('abandoned');
    expect(stateOf(test({ status: 'failed', verdict: 'passed' }))).toBe('failed');
  });
});

describe('progressPct', () => {
  it('gives a proposal no bar at all', () => {
    // A bar pinned at 0% against a number nobody agreed to is a score for a game
    // that has not started.
    expect(progressPct(test({ status: 'proposed', measured: false }))).toBeNull();
  });

  it('caps at 100 so an over-performing test does not overflow its track', () => {
    expect(progressPct(test({ metric_count: 500 }))).toBe(100);
    expect(progressPct(test({ metric_count: 25 }))).toBe(50);
  });

  it('does not divide by a zero target', () => {
    expect(progressPct(test({ target_count: 0 }))).toBeNull();
  });
});

describe('countsLine', () => {
  it('gives a proposal its terms, not a score', () => {
    expect(countsLine(test({ status: 'proposed', measured: false }), 0)).toBe('Proposes 50 people in 14 days');
  });

  it('reports the count against the agreed number, with the window and the rate', () => {
    expect(countsLine(test(), 0)).toBe('12 of 50 · 10 days left · 3% of 400');
  });

  it('drops the days-left clause once the window has closed', () => {
    expect(countsLine(test({ days_left: 0 }), 0)).toBe('12 of 50 · 3% of 400');
  });

  it('drops the days-left clause once the test is decided, window open or not', () => {
    // A verdict outlives the window: a test that passed on day 3 of 14 still
    // carries days_left: 11. Printing "11 days left" under a "Passed" pill reads
    // as though the number could still move.
    expect(countsLine(test({ status: 'passed' }), 0)).toBe('12 of 50 · 3% of 400');
    expect(countsLine(test({ verdict: 'failed' }), 0)).toBe('12 of 50 · 3% of 400');
  });

  it('only mentions the waitlist next to a test that counts waitlist joins', () => {
    // The waitlist is the project's, not the test's. Printing it beside a test
    // measuring checkout.completed invites reading it as that test's progress.
    expect(countsLine(test(), 37)).toContain('37 on the waitlist');
    expect(countsLine(test({ metric_event: 'checkout.completed' }), 37)).not.toContain('waitlist');
  });
});

describe('primaryAction', () => {
  it('asks for the agreement first', () => {
    expect(primaryAction(test({ status: 'proposed', measured: false }))).toBe('Commit to this number');
  });

  it('asks the owner to write down what a settled test meant', () => {
    // The window has decided the number, but nobody has said what it meant. That
    // note is the only part of the row a person can read a month later and act
    // on, so the card asks for it rather than presenting the test as closed.
    const settled = test({ status: 'committed', verdict: 'passed', days_left: 0 });
    expect(isRecorded(settled)).toBe(false);
    expect(primaryAction(settled)).toBe('Record the decision');
  });

  it('moves on once the decision is recorded', () => {
    expect(primaryAction(test({ status: 'passed', verdict: 'passed' }))).toBe('Build it');
    expect(primaryAction(test({ status: 'failed', verdict: 'failed' }))).toBe('Ask which it was');
  });

  it('never rushes a running test into a decision', () => {
    expect(primaryAction(test({ verdict: 'committed' }))).toBe('Open');
  });
});

describe('groupTests', () => {
  it('splits the board into what needs you, what is measuring, and what is over', () => {
    const rows = [
      test({ id: 'a', status: 'proposed', measured: false }),
      test({ id: 'b', verdict: 'committed' }),
      test({ id: 'c', status: 'passed', verdict: 'passed' }),
      test({ id: 'd', status: 'committed', verdict: 'failed' }),
    ];
    const { waiting, running, decided } = groupTests(rows);
    expect(waiting.map((t) => t.id)).toEqual(['a']);
    expect(running.map((t) => t.id)).toEqual(['b']);
    // 'd' is settled by the number even though the owner has not closed it — it
    // belongs with the results, and its card is the one asking for the note.
    expect(decided.map((t) => t.id)).toEqual(['c', 'd']);
  });

  it('handles an empty board without inventing groups', () => {
    expect(groupTests([])).toEqual({ waiting: [], running: [], decided: [] });
  });
});
