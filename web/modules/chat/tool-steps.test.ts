import { describe, expect, it } from 'vitest';
import type { AgentToolTrace } from '@/lib/api';
import type { ChatStep } from './chat-parts';
import { applyToolTrace, applyToolUpdate, serverToolStep, settleOrphanSteps } from './tool-steps';

const running = (callID: string, tool: string): ChatStep => ({ kind: 'tool', callID, tool, status: 'running' });
const trace = (over: Partial<AgentToolTrace> = {}): AgentToolTrace => ({ tool: 'run_sql', allowed: true, ...over });

describe('applyToolTrace', () => {
  // The bug this exists for: two calls to the same tool in one turn are
  // identical in every field but the id, so a name match settles whichever row
  // is found first and leaves the other spinning for the life of the page.
  it('settles the call the trace belongs to, not the first row with that name', () => {
    const steps = [running('tc_1', 'run_sql'), running('tc_2', 'run_sql')];
    const out = applyToolTrace(steps, trace({ call_id: 'tc_2', result_meta: '3 rows' }));
    expect(out[0].kind === 'tool' && out[0].status).toBe('running');
    expect(out[1].kind === 'tool' && out[1].status).toBe('done');
  });

  it('keeps the target the start frame gave the row', () => {
    const steps: ChatStep[] = [{ kind: 'tool', callID: 'tc_1', tool: 'run_sql', target: 'events', status: 'running' }];
    const out = applyToolTrace(steps, trace({ call_id: 'tc_1', latency_ms: 42 }));
    expect(out[0].kind === 'tool' && out[0].target).toBe('events');
    expect(out[0].kind === 'tool' && out[0].durationMS).toBe(42);
  });

  // An older server, or a call the runtime synthesized, sends no id.
  it('falls back to matching a running row by name when there is no call id', () => {
    const out = applyToolTrace([running('', 'run_sql')], trace({ result_meta: 'ok' }));
    expect(out).toHaveLength(1);
    expect(out[0].kind === 'tool' && out[0].status).toBe('done');
  });

  it('appends a trace for a call that never announced itself', () => {
    const out = applyToolTrace(undefined, trace({ call_id: 'tc_9' }));
    expect(out).toHaveLength(1);
  });

  it('reads a denied call as blocked, with the reason', () => {
    const out = applyToolTrace([running('tc_1', 'run_sql')], trace({ call_id: 'tc_1', allowed: false, reason: 'out of scope' }));
    expect(out[0].kind === 'tool' && out[0].status).toBe('blocked');
    expect(out[0].kind === 'tool' && out[0].detail).toBe('out of scope');
  });
});

describe('applyToolUpdate', () => {
  it('attaches partial output to the call that produced it', () => {
    const out = applyToolUpdate([running('tc_1', 'a'), running('tc_2', 'b')], 'tc_2', 'scanned 1k rows');
    expect(out?.[0].kind === 'tool' && out[0].detail).toBeUndefined();
    expect(out?.[1].kind === 'tool' && out[1].detail).toBe('scanned 1k rows');
  });

  it('ignores an update for a call that is not on the timeline', () => {
    const steps = [running('tc_1', 'a')];
    expect(applyToolUpdate(steps, 'tc_missing', 'note')).toBe(steps);
  });

  it('ignores an empty note rather than blanking the row', () => {
    const steps: ChatStep[] = [{ kind: 'tool', callID: 'tc_1', tool: 'a', status: 'running', detail: 'kept' }];
    expect(applyToolUpdate(steps, 'tc_1', '')).toBe(steps);
  });
});

describe('settleOrphanSteps', () => {
  // A spinner on a turn that is over is the same lie as a missing Stop marker.
  it('ends rows whose work will never finish', () => {
    const out = settleOrphanSteps([running('tc_1', 'run_sql'), { kind: 'progress', text: 'thinking' }]);
    expect(out?.[0].kind === 'tool' && out[0].status).toBe('stopped');
    expect(out?.[0].kind === 'tool' && out[0].detail).toBe('Stopped before this finished.');
    expect(out?.[1].kind).toBe('progress');
  });

  // Losing the call id here un-does the addressability the rest of the timeline
  // is built on: a trace that arrives after the stop would no longer find its
  // row and would append a second one for a call already shown.
  it('keeps the identity of the row it settles', () => {
    const steps: ChatStep[] = [{ kind: 'tool', callID: 'tc_1', tool: 'run_sql', target: 'events, 30', status: 'running' }];
    const out = settleOrphanSteps(steps)!;
    expect(out[0].kind === 'tool' && out[0].callID).toBe('tc_1');
    expect(out[0].kind === 'tool' && out[0].target).toBe('events, 30');
    const late = applyToolTrace(out, { tool: 'run_sql', allowed: true, call_id: 'tc_1', result_meta: '12 rows' });
    expect(late).toHaveLength(1);
  });

  it('leaves an already-settled timeline untouched', () => {
    const steps: ChatStep[] = [{ kind: 'tool', callID: 'tc_1', tool: 'a', status: 'done' }];
    expect(settleOrphanSteps(steps)).toBe(steps);
  });
});

describe('serverToolStep', () => {
  it('rebuilds a row from the durable trace, which has no error column', () => {
    const call = { id: 'c1', run_id: 'r1', tool: 'run_sql', allowed: false, args_json: '{}', result_meta: '', duration_ms: 0, created_at: '2026-08-15T00:00:00Z' };
    expect(serverToolStep(call)).toEqual({
      kind: 'tool', tool: 'run_sql', status: 'blocked', detail: 'blocked',
    });
  });
});
