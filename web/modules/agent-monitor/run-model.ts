import type { AgentLLMCall, RunChapter } from '@/lib/api';

// The read model behind the run view. It is here rather than inside the page
// because it is the part that can be wrong in a way a screenshot would not show:
// which chapter the open window belongs to, and which agent actually made a
// call. Both are derivations over data the server sends, so both are testable
// without a browser.

// SessionRow is one participant in a run: the run's own agent, or a sub-agent it
// delegated to.
export type SessionRow = {
  key: string;
  depth: number;
  calls: number;
  tokens: number;
  cost: number;
};

// groupBySession splits a run's model calls by the session that made them.
//
// A sub-agent shares its parent's RUN id — that is what makes the run one unit
// of accounting — so run id cannot separate them and `session_key` is what does.
// Without this the console shows one flat list of calls that reads as a single
// agent having done everything, with a delegated child's spend silently
// attributed to its parent.
//
// Ordering is by depth first, so the run's own agent leads and its children
// follow in the shape of the delegation tree; within a depth, the busiest
// session comes first. Rows with no session key fall back to the run id, which
// is what pre-attribution rows carry.
export function groupBySession(calls: AgentLLMCall[]): SessionRow[] {
  const by = new Map<string, SessionRow>();
  for (const c of calls) {
    const key = c.session_key || c.run_id;
    const row = by.get(key) ?? { key, depth: c.depth ?? 0, calls: 0, tokens: 0, cost: 0 };
    row.calls += 1;
    row.tokens += c.token_input + c.token_output;
    row.cost += c.cost_usd;
    by.set(key, row);
  }
  return [...by.values()].sort((a, b) => a.depth - b.depth || b.calls - a.calls || a.key.localeCompare(b.key));
}

// chapterAt reports which chapter contains a given step offset — the highlight
// in the table of contents. It is derived from the offset the SERVER echoed
// back, not from the offset the client asked for, so a clamped or corrected
// window can never leave the highlight pointing at a chapter that is not on
// screen.
export function chapterAt(chapters: RunChapter[], offset: number): RunChapter | undefined {
  return chapters.find((c) => offset >= c.first_step && offset <= c.last_step);
}

// clampOffset keeps a page step inside the run. `total` is the run's whole step
// count, so the last window starts at the last whole multiple of the window size
// — paging to the end lands on a full page rather than a one-step remainder,
// and paging past it is not possible at all.
export function clampOffset(offset: number, total: number, windowSize: number): number {
  if (total <= 0 || windowSize <= 0) return 0;
  const lastStart = Math.max(0, Math.floor((total - 1) / windowSize) * windowSize);
  return Math.max(0, Math.min(offset, lastStart));
}
