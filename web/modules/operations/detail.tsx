'use client';

import { useState } from 'react';
import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, CircleDot, Clock, Copy, Play, TriangleAlert, Users, Webhook } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { HStack } from '@astryxdesign/core/HStack';
import { Table } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { apiBase, type AgentRun, type Operator } from '@/lib/api';
import { formatCost, formatRelative } from '@/lib/format';
import { useUIStore } from '@/lib/app-state';
import { AppShell } from '@/modules/shared/components/app-shell';
import {
  Button,
  Callout,
  EmptyState,
  Intro,
  Loading,
  Panel,
  StatusPill,
} from '@/modules/shared/components/signal-primitives';
import { useOperator } from './hooks';
import { cronToWords } from './lib/cron-words';
import { isEditableHere, isTeamRun, lastOutcome, operatorStatus, operatorTitle, runnerLabel } from './lib/operator';

// /operations/[id] — one standing operator: what it does, what starts it, who
// runs it, and what happened. Editing writes through the same per-agent trigger
// route the setup tab uses; nothing here is a second source of truth.

const inputCls =
  'w-full rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-2.5 py-1.5 text-[13px] text-[var(--color-text)] outline-none focus:border-[var(--color-border-strong)]';
const labelCls = 'text-[12px] font-medium text-[var(--color-text-secondary)]';

const hookURL = (token: string) => `${apiBase()}/api/agent/hook/${token}`;

function RunStatusPill({ status }: { status: string }) {
  if (status === 'error') return <StatusPill status="attention" label="Failed" grow={false} />;
  if (status === 'running') return <StatusPill status="working" label="Running" grow={false} />;
  return <StatusPill status="healthy" label="Done" grow={false} />;
}

// OperatorForm owns everything editable: the name, the prompt, and the cron.
// It is mounted per row version, so its state starts from that row rather than
// being synced onto it after the fact.
function OperatorForm({
  op,
  saving,
  save,
}: {
  op: Operator;
  saving: boolean;
  save: (v: { op: Operator; name: string; cron: string; prompt: string; enabled: boolean }) => void;
}) {
  const router = useRouter();
  const setMessage = useUIStore((s) => s.setMessage);
  const [name, setName] = useState(op.name);
  const [cron, setCron] = useState(op.cron);
  const [prompt, setPrompt] = useState(op.prompt_template);

  const editable = isEditableHere(op);
  const isWebhook = op.kind === 'webhook';
  const dirty = name !== op.name || cron !== op.cron || prompt !== op.prompt_template;

  const copyHook = async () => {
    await navigator.clipboard.writeText(hookURL(op.webhook_token));
    setMessage('Webhook URL copied');
  };

  return (
    <>
      <Panel title="What it does">
        {editable ? (
          <VStack gap={3} align="stretch">
            <VStack gap={1} align="stretch">
              <label className={labelCls} htmlFor="op-name">
                Name
              </label>
              <input
                id="op-name"
                className={inputCls}
                value={name}
                placeholder={operatorTitle(op)}
                onChange={(e) => setName(e.target.value)}
              />
            </VStack>
            <VStack gap={1} align="stretch">
              <label className={labelCls} htmlFor="op-prompt">
                What the teammate is asked to do
              </label>
              <textarea
                id="op-prompt"
                className={`${inputCls} min-h-[96px]`}
                value={prompt}
                placeholder={
                  isWebhook
                    ? 'What should happen when this fires? Use {{body}} to drop in the webhook payload.'
                    : 'What should happen on each run? Leave empty for the default health check.'
                }
                onChange={(e) => setPrompt(e.target.value)}
              />
            </VStack>
            <HStack gap={2} align="center">
              <Button
                variant="primary"
                size="sm"
                disabled={saving || !dirty}
                onClick={() => save({ op, name, cron, prompt, enabled: op.enabled })}
              >
                {saving ? 'Saving…' : 'Save'}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={saving}
                onClick={() => save({ op, name, cron, prompt, enabled: !op.enabled })}
              >
                {op.enabled ? 'Pause this operator' : 'Resume this operator'}
              </Button>
            </HStack>
          </VStack>
        ) : (
          <VStack gap={2} align="stretch">
            <Text type="supporting">
              This is the project’s original schedule, set on the agent’s own configuration before operators existed. It is
              listed here so this page shows everything that runs — but its timing and its on/off switch live with the
              autonomy setting they are gated by, and changing them here would change how every unattended run in the
              project is allowed to behave.
            </Text>
            <div>
              <Button variant="outline" size="sm" onClick={() => router.push('/settings')}>
                Open the setting
              </Button>
            </div>
          </VStack>
        )}
      </Panel>

      <Panel title="What starts it">
        <VStack gap={3} align="stretch">
          <HStack gap={2} align="center">
            {isWebhook ? <Webhook size={14} aria-hidden /> : <Clock size={14} aria-hidden />}
            <Text weight="medium">{isWebhook ? 'An inbound webhook' : cronToWords(op.cron) || 'No schedule set'}</Text>
          </HStack>
          {isWebhook ? (
            <VStack gap={1} align="stretch">
              <label className={labelCls} htmlFor="op-hook">
                Address
              </label>
              <HStack gap={2} align="center">
                <input
                  id="op-hook"
                  className={`${inputCls} font-mono text-[12px]`}
                  readOnly
                  value={hookURL(op.webhook_token)}
                  onFocus={(e) => e.currentTarget.select()}
                />
                <Button variant="outline" size="sm" icon={<Copy size={13} aria-hidden />} onClick={copyHook}>
                  Copy
                </Button>
              </HStack>
              <Text type="supporting">
                {op.hmac_secret_name
                  ? `Bodies are verified with the “${op.hmac_secret_name}” secret.`
                  : 'Anyone with this address can start a run. Add an HMAC secret on the teammate’s setup page to verify bodies.'}
              </Text>
            </VStack>
          ) : editable ? (
            <VStack gap={1} align="stretch">
              <label className={labelCls} htmlFor="op-cron">
                Schedule (cron)
              </label>
              <input
                id="op-cron"
                className={`${inputCls} max-w-[320px] font-mono`}
                value={cron}
                placeholder="0 9 * * 1"
                onChange={(e) => setCron(e.target.value)}
              />
              <Text type="supporting">{cron.trim() ? cronToWords(cron) : 'Five fields: minute hour day-of-month month day-of-week.'}</Text>
            </VStack>
          ) : (
            <Text type="supporting" className="font-mono">
              {op.cron}
            </Text>
          )}
        </VStack>
      </Panel>
    </>
  );
}

export function OperationDetailPage() {
  const params = useParams<{ operatorId: string }>();
  // The legacy project schedule's id is `config:<projectID>`, so the segment is
  // percent-encoded on the way in and has to be decoded on the way back out.
  const id = decodeURIComponent(params?.operatorId ?? '');
  const router = useRouter();
  const { operator, runs, isLoading, error, save, saving, runNow, running } = useOperator(id);

  if (isLoading && !operator) {
    return (
      <AppShell active="operations">
        <Loading label="Opening the operator…" />
      </AppShell>
    );
  }

  if (error || !operator) {
    return (
      <AppShell active="operations">
        <Intro title="Operator" sub="One standing unit of unattended work." />
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Not found"
          title="We couldn’t open that operator"
          detail="It may have been deleted, or it belongs to another project. Nothing has been changed."
          action={
            <Button variant="outline" size="sm" onClick={() => router.push('/operations')}>
              Back to Operations
            </Button>
          }
        />
      </AppShell>
    );
  }

  const { status, label } = operatorStatus(operator);
  const outcome = lastOutcome(operator);
  const isWebhook = operator.kind === 'webhook';

  return (
    <AppShell active="operations">
      <Intro
        title={
          <HStack gap={2} align="center">
            <span>{operatorTitle(operator)}</span>
            <StatusPill status={status} label={label} grow={false} />
          </HStack>
        }
        sub={outcome.text === 'Never run' ? 'It has not run yet.' : `Last run: ${outcome.text}`}
        action={
          <HStack gap={1}>
            <Button variant="ghost" size="sm" icon={<ArrowLeft size={13} aria-hidden />} onClick={() => router.push('/operations')}>
              Operations
            </Button>
            <Button variant="primary" icon={<Play size={15} aria-hidden />} onClick={runNow} disabled={running || !operator.agent_enabled}>
              Run now
            </Button>
          </HStack>
        }
      />

      {!operator.agent_enabled ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Dead trigger"
          title={`${operator.agent_name} is paused, so this never fires`}
          detail="The trigger is armed and nothing happens when it does. Turn the teammate back on, or pause the trigger so the list stops promising work that isn't happening."
          action={
            <Button variant="agent" size="sm" onClick={() => router.push(`/agents/${operator.agent_id}/setup`)}>
              Open the teammate
            </Button>
          }
        />
      ) : null}

      {/* The form seeds itself from the row it is keyed to. Keying on
          `id:updated_at` means a save remounts it with the saved values, while a
          refetch that changed nothing leaves someone's half-typed cron alone. */}
      <OperatorForm key={`${operator.id}:${operator.updated_at}`} op={operator} saving={saving} save={save} />

      <Panel title="Who runs it">
        <VStack gap={2} align="stretch">
          <HStack gap={2} align="center" className="flex-wrap">
            <Badge
              variant="neutral"
              label={
                <HStack gap={1} align="center">
                  {isTeamRun(operator) ? <Users size={12} aria-hidden /> : <CircleDot size={12} aria-hidden />}
                  {runnerLabel(operator)}
                </HStack>
              }
            />
            <Button variant="ghost" size="sm" onClick={() => router.push(`/agents/${operator.agent_id}/setup`)}>
              Open setup
            </Button>
          </HStack>
          <Text type="supporting">
            {isTeamRun(operator)
              ? `${operator.agent_name} leads ${operator.team_name}, so this trigger runs the whole team.`
              : 'Moving this work to a different teammate means giving that teammate its own trigger — a trigger belongs to whoever answers it.'}
          </Text>
        </VStack>
      </Panel>

      <Panel
        title="Run history"
        action={
          <Text type="supporting">
            {operator.run_count} total · {formatCost(operator.cost_24h)} in 24h
          </Text>
        }
      >
        <VStack gap={3} align="stretch">
          {operator.shared_history ? (
            // Stated, not hidden. A run records the CHANNEL that started it, not
            // which trigger, so with two schedules on one teammate these rows
            // belong to both — and quietly presenting them as this operator's
            // alone would be the page inventing precision the data lacks.
            <Text type="supporting">
              These are {operator.agent_name}’s {isWebhook ? 'webhook' : 'scheduled'} runs. That teammate has more than one{' '}
              {isWebhook ? 'webhook' : 'schedule'}, and a run does not record which one started it — so some of these may
              belong to a sibling operator.
            </Text>
          ) : null}
          {runs.length === 0 ? (
            <EmptyState
              icon={<Clock size={18} aria-hidden />}
              title="It hasn’t run yet"
              detail={
                operator.enabled
                  ? 'Nothing has fired this operator so far. Use “Run now” to rehearse it before it fires on its own.'
                  : 'This operator is paused, so nothing will start it until you resume it.'
              }
            />
          ) : (
            <Table
              data={runs}
              idKey="id"
              density="compact"
              columns={[
                { key: 'when', header: 'Started', align: 'start', renderCell: (row: AgentRun) => formatRelative(row.started_at) },
                { key: 'status', header: 'Outcome', align: 'start', renderCell: (row: AgentRun) => <RunStatusPill status={row.status} /> },
                {
                  key: 'summary',
                  header: 'What it said',
                  align: 'start',
                  renderCell: (row: AgentRun) => (
                    <Text type="supporting" maxLines={2} className={row.status === 'error' ? '!text-danger' : undefined}>
                      {row.summary || '—'}
                    </Text>
                  ),
                },
                {
                  key: 'cost',
                  header: 'Cost',
                  align: 'end',
                  renderCell: (row: AgentRun) => <span className="font-mono tabular-nums">{formatCost(row.cost_usd)}</span>,
                },
              ]}
            />
          )}
        </VStack>
      </Panel>
    </AppShell>
  );
}
