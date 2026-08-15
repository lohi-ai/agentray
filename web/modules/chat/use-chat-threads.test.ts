import { describe, expect, it } from 'vitest';
import type { AgentConversationEntry } from '@/lib/api';
import type { ChatMsg } from './chat-parts';
import { entriesToMessages, mergeDelta, renderEntries, syncSeq } from './use-chat-threads';

let seq = 0;
function entry(over: Partial<AgentConversationEntry>): AgentConversationEntry {
  seq += 1;
  return {
    id: `e${seq}`, conversation_id: 'c1', seq, kind: 'message', role: 'user',
    turn: 1, payload_json: '{}', created_at: '2026-08-15T00:00:00Z',
    ...over,
  } as AgentConversationEntry;
}
const msg = (role: string, text: string, over: Partial<AgentConversationEntry> = {}) =>
  entry({ kind: 'message', role, payload_json: JSON.stringify({ text }), ...over });
const trace = (tool: string, over: Record<string, unknown> = {}) =>
  entry({ kind: 'tool_trace', role: 'assistant', payload_json: JSON.stringify({ tool, allowed: true, ...over }) });

describe('entriesToMessages', () => {
  it('renders one message per entry, in log order', () => {
    seq = 0;
    const out = entriesToMessages([msg('user', 'hi'), msg('assistant', 'hello'), msg('user', 'again')]);
    expect(out.map((m) => [m.role, m.text])).toEqual([
      ['user', 'hi'], ['assistant', 'hello'], ['user', 'again'],
    ]);
  });

  // The bug the whole rewrite exists for: a message steered into a running turn
  // is its own user entry, and the old pair-folding model rendered the ask above
  // it with a permanently empty answer.
  it('keeps a steered message and its answer as separate messages', () => {
    seq = 0;
    const out = entriesToMessages([
      msg('user', 'analyse signups'),
      msg('user', 'actually, just last week'),
      msg('assistant', 'Last week: 412 signups.'),
    ]);
    expect(out).toHaveLength(3);
    expect(out.every((m) => m.role === 'user' || m.text.length > 0)).toBe(true);
  });

  // A stopped turn appends no assistant entry (the runner returns before
  // persistAssistantTurn), so the user message must not be left half-rendered.
  it('does not invent an answer for a stopped turn', () => {
    seq = 0;
    const out = entriesToMessages([msg('user', 'long job'), msg('user', 'next question')]);
    expect(out.map((m) => m.role)).toEqual(['user', 'user']);
  });

  it('attaches tool traces to the answer they precede', () => {
    seq = 0;
    const out = entriesToMessages([
      msg('user', 'q'),
      trace('run_sql', { call_id: 'tc_1', target: 'events', result_meta: '12 rows' }),
      msg('assistant', 'a'),
    ]);
    expect(out).toHaveLength(2);
    expect(out[1].steps).toEqual([
      { kind: 'tool', callID: 'tc_1', tool: 'run_sql', target: 'events', status: 'done', detail: '12 rows' },
    ]);
  });

  it('shows the work of a turn that was stopped before it answered', () => {
    seq = 0;
    const out = entriesToMessages([msg('user', 'q'), trace('run_sql', { call_id: 'tc_1' })]);
    expect(out).toHaveLength(2);
    expect(out[1].outcome).toBe('stopped');
    expect(out[1].provisional).toBe(true);
  });
});

describe('renderEntries cursor', () => {
  it('advances past every settled entry', () => {
    seq = 0;
    const entries = [msg('user', 'q'), trace('run_sql'), msg('assistant', 'a')];
    expect(renderEntries(entries).seq).toBe(entries[entries.length - 1].seq);
  });

  // Trailing traces belong to an answer that hasn't been written yet. Advancing
  // the cursor past them would drop that work from the timeline for good.
  it('stops short of trailing traces so they are re-read with their answer', () => {
    seq = 0;
    const ask = msg('user', 'q');
    const out = renderEntries([ask, trace('run_sql')]);
    expect(out.seq).toBe(ask.seq);
  });
});

describe('mergeDelta', () => {
  const local = (over: Partial<ChatMsg>): ChatMsg => ({ id: 'l1', role: 'user', text: 't', done: true, ...over });

  it('updates a message this tab already holds', () => {
    const out = mergeDelta(
      [local({ id: 'e9', role: 'assistant', text: 'partial', seq: 9, card: null })],
      [local({ id: 'e9', role: 'assistant', text: 'final', seq: 9 })],
    );
    expect(out).toHaveLength(1);
    expect(out[0].text).toBe('final');
  });

  // Our own turn comes back from the server as a delta. It must adopt the server
  // id rather than appear twice — and must keep the card/tools the lean server
  // projection doesn't carry.
  it('adopts the server id onto an unreconciled local message', () => {
    const card = { kind: 'stat' as const, title: 'Signups' };
    const out = mergeDelta(
      [local({ id: 'local:1', role: 'assistant', text: 'answer', card, tools: [] })],
      [local({ id: 'e4', role: 'assistant', text: 'answer', seq: 4 })],
    );
    expect(out).toHaveLength(1);
    expect(out[0].id).toBe('e4');
    expect(out[0].seq).toBe(4);
    expect(out[0].card).toBe(card);
  });

  it('appends work that arrived from another tab', () => {
    const out = mergeDelta(
      [local({ id: 'e1', text: 'mine', seq: 1 })],
      [local({ id: 'e2', role: 'assistant', text: 'theirs', seq: 2 })],
    );
    expect(out.map((m) => m.id)).toEqual(['e1', 'e2']);
  });

  // The provisional message is re-derived from the log on every poll, so the
  // stale copy has to go or a watched thread grows a duplicate every four seconds.
  it('replaces the provisional message instead of stacking copies', () => {
    const out = mergeDelta(
      [local({ id: 'e1', text: 'q', seq: 1 }), local({ id: 'open:1', role: 'assistant', text: '', provisional: true })],
      [local({ id: 'open:1', role: 'assistant', text: '', provisional: true })],
    );
    expect(out.filter((m) => m.provisional)).toHaveLength(1);
  });

  it('is a no-op for an empty delta', () => {
    const cur = [local({ id: 'e1', seq: 1 })];
    expect(mergeDelta(cur, [])).toBe(cur);
  });
});

describe('syncSeq', () => {
  it('is the highest server cursor held', () => {
    expect(syncSeq([
      { id: 'e1', role: 'user', text: 'a', seq: 3 },
      { id: 'local:1', role: 'assistant', text: 'b' },
      { id: 'e2', role: 'user', text: 'c', seq: 7 },
    ])).toBe(7);
  });

  it('is zero when nothing has been synced', () => {
    expect(syncSeq([{ id: 'local:1', role: 'user', text: 'a' }])).toBe(0);
  });
});
