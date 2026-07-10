'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';
import { Bell, Plus, Trash2 } from 'lucide-react';
import { Grid } from '@astryxdesign/core/Grid';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import {
  ALERT_CHANNEL_KINDS,
  ALERT_OPS,
  ALERT_SOURCE_KINDS,
  type AlertChannelKind,
  type AlertOp,
  type AlertRule,
  type AlertRuleInput,
  type AlertSourceKind,
} from '@/lib/api';
import { useAlertChannels, useAlertRules } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Intro, Loading, Panel, StatusPill } from '@/modules/shared/components/signal-primitives';

const labelCls = 'mb-1 block text-[12px] font-medium text-[var(--color-text-secondary)]';

const SOURCE_LABEL: Record<AlertSourceKind, string> = {
  insight: 'Insight / chart',
  sql: 'Saved SQL query',
  agent_ops: 'Agent ops metric',
};

const OP_LABEL: Record<AlertOp, string> = {
  gt: 'is above',
  lt: 'is below',
  z_score: 'z-score beyond (anomaly)',
};

const emptyDraft: AlertRuleInput = {
  name: '',
  source_kind: 'insight',
  source_ref: '',
  condition: { op: 'gt', value: 0 },
  schedule_cron: '*/5 * * * *',
  channels: [],
  enabled: true,
};

export function AlertsPage() {
  const { rules, loading, create, update, remove } = useAlertRules();
  const { channels } = useAlertChannels();
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState<AlertRuleInput>(emptyDraft);

  const channelName = useMemo(() => new Map(channels.map((c) => [c.id, c.name])), [channels]);

  const canSave = draft.name.trim() !== '' && draft.source_ref.trim() !== '';

  const submit = async () => {
    if (!canSave) return;
    await create.mutateAsync(draft);
    setDraft(emptyDraft);
    setCreating(false);
  };

  const toggleChannel = (id: string) =>
    setDraft((d) => ({
      ...d,
      channels: d.channels.includes(id) ? d.channels.filter((c) => c !== id) : [...d.channels, id],
    }));

  return (
    <AppShell>
      <Intro
        title="Alerts"
        sub="Notify a channel when a metric breaks its threshold or drifts into anomaly."
        action={(
          <Button variant="primary" size="sm" icon={<Plus size={14} />} onClick={() => setCreating((v) => !v)}>
            {creating ? 'Cancel' : 'New alert'}
          </Button>
        )}
      />

      {creating ? (
        <Panel title="New alert rule">
          <Grid columns={{ minWidth: 340, max: 2 }} gap={3}>
            <div>
              <label className={labelCls}>Name</label>
              <TextInput
                label="Name"
                isLabelHidden
                value={draft.name}
                placeholder="e.g. Signups dropped"
                onChange={(v: string) => setDraft((d) => ({ ...d, name: v }))}
                width="100%"
              />
            </div>
            <div>
              <label className={labelCls}>Schedule (cron)</label>
              <TextInput
                label="Schedule"
                isLabelHidden
                value={draft.schedule_cron}
                placeholder="*/5 * * * *"
                onChange={(v: string) => setDraft((d) => ({ ...d, schedule_cron: v }))}
                width="100%"
              />
            </div>
            <div>
              <label className={labelCls}>Source</label>
              <Selector
                label="Source"
                isLabelHidden
                options={ALERT_SOURCE_KINDS.map((k) => ({ value: k, label: SOURCE_LABEL[k] }))}
                value={draft.source_kind}
                onChange={(v: string) => setDraft((d) => ({ ...d, source_kind: v as AlertSourceKind }))}
                width="100%"
              />
            </div>
            <div>
              <label className={labelCls}>
                Source reference
                <span className="ms-2 text-[var(--color-text-disabled)]">chart / query id or metric name</span>
              </label>
              <TextInput
                label="Source reference"
                isLabelHidden
                value={draft.source_ref}
                placeholder="chart_… / query_… / cost_usd"
                onChange={(v: string) => setDraft((d) => ({ ...d, source_ref: v }))}
                width="100%"
              />
            </div>
            <div>
              <label className={labelCls}>Condition</label>
              <Selector
                label="Condition"
                isLabelHidden
                options={ALERT_OPS.map((o) => ({ value: o, label: OP_LABEL[o] }))}
                value={draft.condition.op}
                onChange={(v: string) => setDraft((d) => ({ ...d, condition: { ...d.condition, op: v as AlertOp } }))}
                width="100%"
              />
            </div>
            <div>
              <label className={labelCls}>Threshold value</label>
              <TextInput
                label="Threshold"
                isLabelHidden
                value={String(draft.condition.value)}
                placeholder="0"
                onChange={(v: string) => setDraft((d) => ({ ...d, condition: { ...d.condition, value: Number(v) || 0 } }))}
                width="100%"
              />
            </div>
          </Grid>

          <div className="mt-4">
            <label className={labelCls}>Notify channels</label>
            {channels.length === 0 ? (
              <p className="text-[12px] text-[var(--color-text-disabled)]">
                No channels configured yet. The alert still fires and appears here; add a Slack/email/webhook
                channel in workspace settings to get notified.
              </p>
            ) : (
              <div className="flex flex-col gap-1.5">
                {channels.map((c) => (
                  <CheckboxInput
                    key={c.id}
                    label={`${c.name} (${c.kind})`}
                    value={draft.channels.includes(c.id)}
                    onChange={() => toggleChannel(c.id)}
                  />
                ))}
              </div>
            )}
          </div>

          <div className="mt-4 flex items-center gap-2">
            <Button variant="primary" size="sm" onClick={() => void submit()} disabled={!canSave || create.isPending}>
              {create.isPending ? 'Creating…' : 'Create alert'}
            </Button>
          </div>
        </Panel>
      ) : null}

      <Panel title="Alert rules">
        {loading && rules.length === 0 ? (
          <Loading label="Loading alerts…" />
        ) : rules.length === 0 ? (
          <EmptyState
            icon={<Bell size={22} />}
            title="No alerts yet"
            detail="Create a rule to be notified when a metric crosses a threshold or drifts into anomaly."
          />
        ) : (
          <div className="flex flex-col divide-y divide-[var(--color-border)]">
            {rules.map((rule) => (
              <AlertRuleRow
                key={rule.id}
                rule={rule}
                channelName={channelName}
                onToggle={(enabled) => update.mutate({ id: rule.id, input: toInput(rule, enabled) })}
                onDelete={() => remove.mutate(rule.id)}
              />
            ))}
          </div>
        )}
      </Panel>
    </AppShell>
  );
}

function toInput(rule: AlertRule, enabled: boolean): AlertRuleInput {
  return {
    name: rule.name,
    source_kind: rule.source_kind,
    source_ref: rule.source_ref,
    condition: rule.condition,
    schedule_cron: rule.schedule_cron,
    channels: rule.channels,
    enabled,
  };
}

function AlertRuleRow({
  rule,
  channelName,
  onToggle,
  onDelete,
}: {
  rule: AlertRule;
  channelName: Map<string, string>;
  onToggle: (enabled: boolean) => void;
  onDelete: () => void;
}) {
  const firing = rule.last_state === 'firing';
  const targets = rule.channels.map((id) => channelName.get(id) ?? id).join(', ') || 'no channel';
  const why = encodeURIComponent(`Why did the alert "${rule.name}" fire? Show the metric behind it.`);
  return (
    <div className="flex items-center justify-between gap-3 py-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="truncate font-medium">{rule.name}</span>
          <StatusPill status={firing ? 'attention' : rule.enabled ? 'healthy' : 'paused'} label={firing ? 'Firing' : rule.enabled ? 'OK' : 'Paused'} grow={false} />
        </div>
        <p className="mt-0.5 truncate text-[12px] text-[var(--color-text-secondary)]">
          {SOURCE_LABEL[rule.source_kind]} · {rule.source_ref} {OP_LABEL[rule.condition.op]} {rule.condition.value} · every {rule.schedule_cron} · → {targets}
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        <Link href={`/chat?q=${why}`} className="text-[12px] text-[var(--color-primary)] hover:underline">
          Why did this fire?
        </Link>
        <CheckboxInput label="Enabled" isLabelHidden value={rule.enabled} onChange={(v: boolean) => onToggle(v)} />
        <button type="button" aria-label="Delete alert" className="text-[var(--color-text-disabled)] hover:text-[var(--color-danger)]" onClick={onDelete}>
          <Trash2 size={15} />
        </button>
      </div>
    </div>
  );
}

// Re-exported so a future channel manager can reuse the vocabulary without
// re-deriving the kinds list.
export const ALERT_CHANNEL_KIND_OPTIONS: { value: AlertChannelKind; label: string }[] = ALERT_CHANNEL_KINDS.map((k) => ({ value: k, label: k }));
