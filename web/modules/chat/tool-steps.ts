// The tool timeline reducers: pure functions from a message's step list and one
// stream frame to the next step list. They live outside the page because they
// are where the timeline's correctness actually is — reconciliation by call id,
// and settling rows whose work will never finish — and that is worth testing
// without mounting a chat.
import type { AgentToolCall, AgentToolTrace } from '@/lib/api';
import type { ChatStep } from './chat-parts';

// Map a streamed tool trace onto the timeline. Reconciliation is by call id: two
// concurrent calls to the same tool are identical in every other field, so
// matching on the name settles whichever row happens to be last and leaves the
// other spinning forever. Only a trace with no id (older server, synthesized
// call) falls back to the name match it used to do.
function toolStatus(t: AgentToolTrace): 'done' | 'blocked' | 'error' {
  return t.error ? 'error' : t.allowed ? 'done' : 'blocked';
}
export function applyToolTrace(steps: ChatStep[] | undefined, t: AgentToolTrace): ChatStep[] {
  const detail = t.error || t.reason || t.result_meta || (t.allowed ? '' : 'blocked');
  const list = steps ? [...steps] : [];
  const at = list.findIndex((s) =>
    s.kind === 'tool' && (t.call_id ? s.callID === t.call_id : s.status === 'running' && s.tool === t.tool),
  );
  if (at >= 0) {
    const prev = list[at];
    if (prev.kind === 'tool') {
      // Keep the target the start frame gave us — the completed trace doesn't
      // carry it, and losing it mid-run makes the row jump.
      list[at] = { ...prev, status: toolStatus(t), detail, durationMS: t.latency_ms };
      return list;
    }
  }
  list.push({ kind: 'tool', callID: t.call_id, tool: t.tool, status: toolStatus(t), detail, durationMS: t.latency_ms });
  return list;
}
// Rebuild a tool step from the persisted trace, used to restore the timeline a
// reload lost (the durable trace has no error column, so allowed drives status).
export function serverToolStep(tc: AgentToolCall): ChatStep {
  return { kind: 'tool', tool: tc.tool, status: tc.allowed ? 'done' : 'blocked', detail: tc.result_meta || (tc.allowed ? '' : 'blocked') };
}
// A turn that ends before its tools do leaves them `running` forever — a spinner
// that outlives the work it describes. Settling them is part of ending the turn.
export function settleOrphanSteps(steps: ChatStep[] | undefined): ChatStep[] | undefined {
  if (!steps?.some((s) => s.kind === 'tool' && s.status === 'running')) return steps;
  return steps.map((s) =>
    s.kind === 'tool' && s.status === 'running'
      ? { kind: 'tool' as const, tool: s.tool, status: 'error' as const, detail: 'Stopped before this finished.' }
      : s,
  );
}
// Attach a streaming tool's partial output to the call that produced it. Keyed
// by call id, so two calls to the same tool running side by side each show their
// own output instead of overwriting one another.
export function applyToolUpdate(steps: ChatStep[] | undefined, callID: string, note: string): ChatStep[] | undefined {
  if (!steps || !note) return steps;
  const at = steps.findIndex((s) => s.kind === 'tool' && (callID ? s.callID === callID : s.status === 'running'));
  if (at < 0) return steps;
  const list = [...steps];
  const prev = list[at];
  if (prev.kind !== 'tool') return steps;
  list[at] = { ...prev, detail: note };
  return list;
}
