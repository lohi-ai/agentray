'use client';

import { TabList, Tab } from '@astryxdesign/core/TabList';
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { List, ListItem } from '@astryxdesign/core/List';
import { Text } from '@astryxdesign/core/Text';
import { Flag } from 'lucide-react';
import type { AgentPlanItem, AgentRecommendation, AgentRun } from '@/lib/api';
import { formatCompact, formatCost, formatLatency, formatRelative } from '@/lib/format';
import { PlanList, planProgress } from './chat-parts';

export type PanelTab = 'plan' | 'recs' | 'activity' | 'runs';

function runLatency(run: AgentRun): string {
  if (!run.finished_at) return '—';
  const ms = new Date(run.finished_at).getTime() - new Date(run.started_at).getTime();
  return formatLatency(ms);
}

export function WorkPanel({
  tab, onTab, plan, goal, recommendations, runs, onAck, bare,
}: {
  tab: PanelTab;
  onTab: (tab: PanelTab) => void;
  // The thread's current plan and completion condition. These are the two pieces
  // of agent state a person needs *while* work is happening, so they get the
  // panel's first tab rather than being buried in a turn they have to scroll back
  // to find.
  plan: AgentPlanItem[];
  goal: string;
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
          <Tab value="plan" label="Plan" endContent={plan.length ? <Badge variant="blue" label={plan.filter((i) => i.status !== 'completed').length} /> : undefined} />
          <Tab value="recs" label="Recommendations" endContent={open.length ? <Badge variant="purple" label={open.length} /> : undefined} />
          <Tab value="activity" label="Activity" />
          <Tab value="runs" label="Runs" />
        </TabList>
      </div>
      <div className="flex-1 overflow-auto p-3">
        {tab === 'plan' ? <PlanPane plan={plan} goal={goal} /> : null}
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

// PlanPane is the standing view of what the agent is doing and what it is bound
// to. It says nothing rather than something vague when there is no plan: an agent
// answering a one-step question has no plan to show, and inventing a placeholder
// checklist for it would teach the user to ignore this tab.
function PlanPane({ plan, goal }: { plan: AgentPlanItem[]; goal: string }) {
  return (
    <div className="flex flex-col gap-3">
      {goal ? (
        <Card padding={3}>
          <div className="flex items-start gap-2">
            <Flag size={13} className="mt-[3px] flex-none text-[var(--color-text-secondary)]" />
            <div className="min-w-0">
              <Text type="supporting" weight="medium" className="block">Working until this is true</Text>
              <Text className="block break-words">{goal}</Text>
            </div>
          </div>
        </Card>
      ) : null}
      {plan.length ? (
        <Card padding={3}>
          <Text type="supporting" weight="medium" className="mb-2 block">Plan · {planProgress(plan)}</Text>
          <PlanList items={plan} />
        </Card>
      ) : (
        <Text type="supporting" weight="medium" className="block px-1 pt-1 uppercase tracking-[0.08em]">
          No plan yet
        </Text>
      )}
      <Text type="supporting" className="block px-1 leading-[1.5] opacity-70">
        The agent writes this itself for multi-step work, and keeps it in front of itself even after older
        messages are summarized away. Type <code>/plan</code> to have it read back the current one.
      </Text>
    </div>
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

// The runner writes done | error | stopped (runner.go). This pane keyed on
// 'failed', which nothing writes, so every errored run has been painting itself
// as a success — and a stopped run would have joined it. A stop is neither: the
// user ended the run, so it reads muted rather than green or red.
const isFailedRun = (status: AgentRun['status']) => status === 'error' || status === 'failed';
const isStoppedRun = (status: AgentRun['status']) => status === 'stopped';

// Activity dot stays a bespoke <span>: "running" is brand agent-purple, which
// StatusDot's variant enum can't express (no purple). Same exception as StatusPill.
function activityDot(status: AgentRun['status']) {
  const tone = status === 'running' ? 'bg-warning' : isFailedRun(status) ? 'bg-danger' : isStoppedRun(status) ? '' : 'bg-success';
  return <span className={`mt-[5px] h-1.5 w-1.5 flex-none rounded-full bg-faint ${tone}`} />;
}
function runDot(status: AgentRun['status']) {
  const pulse = "after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-['']";
  const tone =
    status === 'running' ? `bg-agent text-agent ${pulse}`
    : isFailedRun(status) ? `bg-danger text-danger ${pulse}`
    : 'bg-faint';
  return <span className={`relative inline-block size-2 flex-none rounded-full ${tone}`} />;
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
              <span>{formatCost(run.cost_usd, run.cost_unpriced)}</span>
            </span>
          }
          endContent={
            <span
              className="font-mono tabular-nums text-[11px]"
              style={{
                color: run.status === 'running' ? 'var(--agent)'
                  : isFailedRun(run.status) ? 'var(--danger)'
                  : isStoppedRun(run.status) ? 'var(--color-text-disabled)'
                  : 'var(--success)',
              }}
            >
              {run.status}
            </span>
          }
        />
      ))}
    </List>
  );
}
