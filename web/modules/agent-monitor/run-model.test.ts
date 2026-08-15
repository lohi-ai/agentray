import { describe, expect, it } from 'vitest';
import type { AgentLLMCall, RunChapter } from '@/lib/api';
import { chapterAt, clampOffset, groupBySession } from './run-model';

function call(over: Partial<AgentLLMCall>): AgentLLMCall {
  return {
    id: 'c', run_id: 'run-1', session_key: '', depth: 0, seq: 0, base_seq: 0, keep_prefix: 0,
    provider: 'anthropic', model: 'claude-opus-5', messages_json: '[]', tools: [], response: '',
    tool_calls_json: '[]', stop_reason: 'end_turn', token_input: 100, token_output: 20,
    cost_usd: 0.01, latency_ms: 900, streamed: false, error: '', created_at: '2026-08-15T00:00:00Z',
    ...over,
  };
}

function chapter(index: number, first: number, last: number): RunChapter {
  return {
    index, first_step: first, last_step: last, first_turn: first + 1, last_turn: last + 1,
    title: `chapter ${index}`, steps: last - first + 1, tool_calls: 0,
    tokens_in: 0, tokens_out: 0, cost_usd: 0,
  };
}

describe('groupBySession', () => {
  it('separates a sub-agent from the parent that shares its run id', () => {
    const rows = groupBySession([
      call({ session_key: 'run-1', depth: 0 }),
      call({ session_key: 'run-1', depth: 0 }),
      call({ session_key: 'child-a', depth: 1, token_input: 50, token_output: 10, cost_usd: 0.02 }),
    ]);

    expect(rows.map((r) => r.key)).toEqual(['run-1', 'child-a']);
    expect(rows[0].calls).toBe(2);
    // The child's spend must land on the child. Folding it into the parent is
    // the exact failure this grouping exists to prevent.
    expect(rows[1].calls).toBe(1);
    expect(rows[1].tokens).toBe(60);
    expect(rows[1].cost).toBeCloseTo(0.02);
    expect(rows[0].cost).toBeCloseTo(0.02); // 2 × 0.01, not 3 × anything
  });

  it('orders by delegation depth, then by how busy the session was', () => {
    const rows = groupBySession([
      call({ session_key: 'grandchild', depth: 2 }),
      call({ session_key: 'child-quiet', depth: 1 }),
      call({ session_key: 'child-busy', depth: 1 }),
      call({ session_key: 'child-busy', depth: 1 }),
      call({ session_key: 'parent', depth: 0 }),
    ]);
    expect(rows.map((r) => r.key)).toEqual(['parent', 'child-busy', 'child-quiet', 'grandchild']);
  });

  it('falls back to the run id for rows written before attribution existed', () => {
    // Old trace rows have no session key. They must still group into one row
    // rather than collapsing into an empty-string bucket that reads as a
    // nameless agent.
    const rows = groupBySession([call({ session_key: '' }), call({ session_key: '' })]);
    expect(rows).toHaveLength(1);
    expect(rows[0].key).toBe('run-1');
    expect(rows[0].calls).toBe(2);
  });

  it('has nothing to show for a run with no calls', () => {
    expect(groupBySession([])).toEqual([]);
  });
});

describe('chapterAt', () => {
  const chapters = [chapter(0, 0, 9), chapter(1, 10, 24), chapter(2, 25, 30)];

  it('finds the chapter containing the window, including at its boundaries', () => {
    expect(chapterAt(chapters, 0)?.index).toBe(0);
    expect(chapterAt(chapters, 9)?.index).toBe(0);
    expect(chapterAt(chapters, 10)?.index).toBe(1);
    expect(chapterAt(chapters, 24)?.index).toBe(1);
    expect(chapterAt(chapters, 25)?.index).toBe(2);
  });

  it('reports nothing rather than guessing when the offset is off the run', () => {
    expect(chapterAt(chapters, 31)).toBeUndefined();
    expect(chapterAt([], 0)).toBeUndefined();
  });
});

describe('clampOffset', () => {
  it('stops at the last whole window instead of running off the end', () => {
    // 120 steps, 50 per window → windows start at 0, 50, 100.
    expect(clampOffset(150, 120, 50)).toBe(100);
    expect(clampOffset(100, 120, 50)).toBe(100);
    expect(clampOffset(50, 120, 50)).toBe(50);
  });

  it('never goes below the first step', () => {
    expect(clampOffset(-10, 120, 50)).toBe(0);
  });

  it('lands on 0 for a run that fits in one window', () => {
    expect(clampOffset(50, 30, 50)).toBe(0);
    expect(clampOffset(0, 0, 50)).toBe(0);
  });

  it('puts an exact multiple of the window on its own last page', () => {
    // 100 steps, 50 per window → the last page is steps 51-100, starting at 50.
    // Rounding this to 100 would open an empty page past the end of the run.
    expect(clampOffset(999, 100, 50)).toBe(50);
  });
});
