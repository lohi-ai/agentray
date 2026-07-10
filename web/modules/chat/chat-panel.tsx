'use client';

import { TabList, Tab } from '@astryxdesign/core/TabList';
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { List, ListItem } from '@astryxdesign/core/List';
import { Text } from '@astryxdesign/core/Text';
import type { AgentRecommendation, AgentRun } from '@/lib/api';
import { formatCompact, formatCost, formatLatency, formatRelative } from '@/lib/format';

export type PanelTab = 'recs' | 'activity' | 'runs';

function runLatency(run: AgentRun): string {
  if (!run.finished_at) return '—';
  const ms = new Date(run.finished_at).getTime() - new Date(run.started_at).getTime();
  return formatLatency(ms);
}

export function WorkPanel({
  tab, onTab, recommendations, runs, onAck, bare,
}: {
  tab: PanelTab;
  onTab: (tab: PanelTab) => void;
  recommendations: AgentRecommendation[];
  runs: AgentRun[];
  onAck: (id: string, status: 'accepted' | 'dismissed') => void;
  // When hosted inside a StackSheet panel (narrow viewport) drop the grid
  // placement and left border — the sheet card supplies its own framing.
  bare?: boolean;
}) {
  const open = recommendations.filter((r) => r.status === 'open');
  const body = (
    <>
      <div className="px-3 pt-2.5">
        <TabList value={tab} onChange={(v) => onTab(v as PanelTab)} size="sm" layout="fill">
          <Tab value="recs" label="Recommendations" endContent={open.length ? <Badge variant="purple" label={open.length} /> : undefined} />
          <Tab value="activity" label="Activity" />
          <Tab value="runs" label="Runs" />
        </TabList>
      </div>
      <div className="flex-1 overflow-auto p-3">
        {tab === 'recs' ? <RecsPane recs={open} onAck={onAck} /> : null}
        {tab === 'activity' ? <ActivityPane runs={runs} /> : null}
        {tab === 'runs' ? <RunsPane runs={runs} /> : null}
      </div>
    </>
  );
  if (bare) return <div className="flex min-h-0 flex-1 flex-col overflow-hidden">{body}</div>;
  return (
    <aside className="col-start-3 flex min-h-0 flex-col overflow-hidden border-l border-[var(--color-border)] bg-[var(--color-background-card)]">
      {body}
    </aside>
  );
}

function RecsPane({ recs, onAck }: { recs: AgentRecommendation[]; onAck: (id: string, status: 'accepted' | 'dismissed') => void }) {
  if (recs.length === 0) return <div className="px-1 py-1.5 pt-3 text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">No open recommendations</div>;
  return (
    <div>
      {recs.map((r) => (
        <Card key={r.id} padding={3} className="mb-2">
          <Text weight="semibold" className="mb-[3px] block text-[13px]">{r.title}</Text>
          <Text type="supporting" className="mb-2.5 block leading-[1.45]">{r.rationale}</Text>
          <div className="flex gap-2">
            <Button variant="primary" size="sm" label="Act" onClick={() => onAck(r.id, 'accepted')} />
            <Button variant="ghost" size="sm" label="Skip" onClick={() => onAck(r.id, 'dismissed')} />
          </div>
        </Card>
      ))}
    </div>
  );
}

// Activity dot stays a bespoke <span>: "running" is brand agent-purple, which
// StatusDot's variant enum can't express (no purple). Same exception as StatusPill.
function activityDot(status: AgentRun['status']) {
  return <span className={`mt-[5px] h-1.5 w-1.5 flex-none rounded-full bg-faint ${status === 'running' ? 'bg-warning' : status === 'failed' ? '' : 'bg-success'}`} />;
}
function runDot(status: AgentRun['status']) {
  return <span className={`relative inline-block size-2 flex-none rounded-full ${status === 'running' ? 'bg-agent text-agent after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-[\'\']' : status === 'failed' ? 'bg-warning text-warning after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-[\'\']' : 'bg-faint'}`} />;
}

function ActivityPane({ runs }: { runs: AgentRun[] }) {
  if (runs.length === 0) return <Text type="supporting" weight="medium" className="block px-1 pt-3 uppercase tracking-[0.08em]">No recent activity</Text>;
  return (
    <List header="Live activity" density="compact">
      {runs.slice(0, 8).map((run) => (
        <ListItem
          key={run.id}
          startContent={activityDot(run.status)}
          label={run.summary || `${run.trigger} run`}
          endContent={<span className="font-mono text-[11px] text-[var(--color-text-disabled)]">{run.finished_at ? formatRelative(run.finished_at) : 'now'}</span>}
        />
      ))}
    </List>
  );
}

function RunsPane({ runs }: { runs: AgentRun[] }) {
  if (runs.length === 0) return <Text type="supporting" weight="medium" className="block px-1 pt-3 uppercase tracking-[0.08em]">No runs yet</Text>;
  return (
    <List header="Recent runs" density="compact">
      {runs.slice(0, 12).map((run) => (
        <ListItem
          key={run.id}
          startContent={runDot(run.status)}
          label={run.summary || `${run.trigger} run`}
          description={
            <span className="flex gap-3 font-mono tabular-nums">
              <span>{runLatency(run)}</span>
              <span>{formatCompact(run.token_input + run.token_output)} tok</span>
              <span>{formatCost(run.cost_usd)}</span>
            </span>
          }
          endContent={<span className="font-mono tabular-nums text-[11px]" style={{ color: run.status === 'running' ? 'var(--agent)' : run.status === 'failed' ? 'var(--danger)' : 'var(--success)' }}>{run.status}</span>}
        />
      ))}
    </List>
  );
}
