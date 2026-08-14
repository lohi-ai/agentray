'use client';

import { useState } from 'react';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import type { WorkspaceModelTiers, WorkspaceModelTiersInput, WorkspaceProvider, WorkspaceProviderInput } from '@/lib/api';
import { useWorkspaceModels } from '@/modules/agent/hooks';
import { Button, Loading, Panel } from '@/modules/shared/components/signal-primitives';
import {
  decodeTierValue,
  encodeTierValue,
  listedModelsToPickerOptions,
  type ListedModel,
} from './model-picker';

const TIERS = [
  { key: 'flash', label: 'Default', hint: 'Balanced model every agent draws from. Required.' },
  { key: 'lite', label: 'Lite', hint: 'Cheaper model for mechanical steps. Blank inherits the default, including its provider.' },
  { key: 'pro', label: 'Pro', hint: 'Stronger model for deep reasoning. Blank inherits the default, including its provider.' },
] as const;

type TierKey = (typeof TIERS)[number]['key'];

// Vendor kinds the user can configure — not a model catalog. Model IDs come
// from each active provider's list-models API.
const VENDOR_KINDS = [
  { value: 'openai', label: 'OpenAI' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'google', label: 'Google Gemini' },
  { value: 'openai-compat', label: 'OpenAI-compatible' },
] as const;

type ProviderDraft = { vendor: string; name: string; base_url: string; api_key: string };
const emptyProviderDraft = (): ProviderDraft => ({ vendor: 'openai', name: '', base_url: '', api_key: '' });

type TierDraft = { providerId: string; model: string };
type Draft = {
  flash: TierDraft;
  lite: TierDraft;
  pro: TierDraft;
  model_fallback: boolean;
};

function draftFromConfig(c: WorkspaceModelTiers): Draft {
  return {
    flash: { providerId: c.flash_provider_id || '', model: c.model || '' },
    lite: { providerId: c.lite_provider_id || '', model: c.lite_model || '' },
    pro: { providerId: c.pro_provider_id || '', model: c.pro_model || '' },
    model_fallback: c.model_fallback,
  };
}

function draftToInput(d: Draft): WorkspaceModelTiersInput {
  return {
    flash_provider_id: d.flash.providerId,
    model: d.flash.model,
    lite_provider_id: d.lite.providerId,
    lite_model: d.lite.model,
    pro_provider_id: d.pro.providerId,
    pro_model: d.pro.model,
    model_fallback: d.model_fallback,
  };
}

const labelCls = 'mb-1.5 block text-[12.5px] text-[var(--color-text-secondary)]';

function vendorNeedsBaseURL(vendor: string): boolean {
  return vendor === 'openai-compat' || (vendor !== 'openai' && vendor !== 'anthropic' && vendor !== 'google');
}

export function ModelsTab() {
  const {
    models, modelsLoading, providers, listedModels, listedErrors, listedLoading,
    saveModels, testModels, createProvider, updateProvider, deleteProvider,
  } = useWorkspaceModels();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [seededFrom, setSeededFrom] = useState<WorkspaceModelTiers | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [adding, setAdding] = useState(false);
  const [addDraft, setAddDraft] = useState<ProviderDraft>(emptyProviderDraft());
  const [editing, setEditing] = useState<string | null>(null);
  const [editDraft, setEditDraft] = useState<ProviderDraft>(emptyProviderDraft());
  const [busyProvider, setBusyProvider] = useState<string | null>(null);

  if (models && models !== seededFrom) {
    setSeededFrom(models);
    setDraft(draftFromConfig(models));
  }

  if (modelsLoading && !draft) return <Panel title="AI Provider"><Loading label="Loading model pool…" /></Panel>;
  if (!models || !draft) return <Panel title="AI Provider"><Loading label="Loading model pool…" /></Panel>;

  const pickerOptions = listedModelsToPickerOptions(listedModels as ListedModel[]);
  const inheritOption = { value: '', label: 'Inherit default' };

  const onSave = async () => {
    setSaving(true);
    try {
      await saveModels(draftToInput(draft));
    } finally {
      setSaving(false);
    }
  };
  const onTest = async () => {
    setTesting(true);
    try {
      await testModels();
    } finally {
      setTesting(false);
    }
  };

  const onAdd = async () => {
    const input: WorkspaceProviderInput = {
      vendor: addDraft.vendor,
      name: addDraft.name,
      base_url: addDraft.base_url,
      api_key: addDraft.api_key,
    };
    setBusyProvider('new');
    try {
      await createProvider(input);
      setAddDraft(emptyProviderDraft());
      setAdding(false);
    } finally {
      setBusyProvider(null);
    }
  };

  const onUpdate = async (id: string) => {
    setBusyProvider(id);
    try {
      await updateProvider(id, {
        vendor: editDraft.vendor,
        name: editDraft.name,
        base_url: editDraft.base_url,
        api_key: editDraft.api_key,
      });
      setEditing(null);
    } finally {
      setBusyProvider(null);
    }
  };

  const onDelete = async (id: string) => {
    setBusyProvider(id);
    try {
      await deleteProvider(id);
      setDraft((d) => {
        if (!d) return d;
        const clear = (t: TierDraft): TierDraft => (t.providerId === id ? { providerId: '', model: '' } : t);
        return { ...d, flash: clear(d.flash), lite: clear(d.lite), pro: clear(d.pro) };
      });
    } finally {
      setBusyProvider(null);
    }
  };

  const setTierValue = (tier: TierKey, value: string) => {
    if (!value) {
      setDraft((d) => (d ? { ...d, [tier]: { providerId: '', model: '' } } : d));
      return;
    }
    const { providerId, modelId } = decodeTierValue(value);
    setDraft((d) => (d ? { ...d, [tier]: { providerId, model: modelId } } : d));
  };

  const configured = providers as WorkspaceProvider[];
  const hasAnyKey = configured.some((p) => p.has_key) || models.has_key;

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[13px] text-[var(--color-text-primary)]">
        {models.hosted_default && !configured.length
          ? 'Using the hosted model. Add a provider with your own key to override — encrypted at rest, never shown again.'
          : hasAnyKey
            ? 'Configured providers are encrypted at rest. Pick a listed model for each tier.'
            : 'Add a provider and paste its API key so Growth Lead can answer. Encrypted at rest, never shown again.'}
      </p>
      <p className="max-w-[640px] text-[12.5px] text-[var(--color-text-secondary)]">
        Configure one or many providers, then pick each tier from the models those providers list. Lite and Pro inherit the default when left blank. Only workspace owners and admins can change these.
      </p>

      <Panel title="Providers">
        {configured.length === 0 && !adding ? (
          <p className="mb-3 text-[12.5px] text-[var(--color-text-secondary)]">No providers configured yet.</p>
        ) : null}
        <div className="flex flex-col gap-3">
          {configured.map((p) => (
            <div key={p.id} className="rounded-md border border-[var(--color-border)] p-3">
              {editing === p.id ? (
                <ProviderFields
                  draft={editDraft}
                  onChange={setEditDraft}
                  onSubmit={() => void onUpdate(p.id)}
                  onCancel={() => setEditing(null)}
                  submitLabel={busyProvider === p.id ? 'Saving…' : 'Save provider'}
                  disabled={busyProvider === p.id}
                  keyHint={p.has_key ? 'key set' : 'not set'}
                />
              ) : (
                <div className="flex flex-wrap items-start justify-between gap-2">
                  <div>
                    <div className="text-[13px] text-[var(--color-text-primary)]">{p.name || p.vendor}</div>
                    <div className="text-[12px] text-[var(--color-text-secondary)]">
                      {p.vendor}
                      {p.base_url ? ` · ${p.base_url}` : ''}
                      {' · '}
                      {p.has_key ? 'key set' : 'no key'}
                    </div>
                  </div>
                  <div className="flex gap-1">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => {
                        setEditing(p.id);
                        setEditDraft({ vendor: p.vendor, name: p.name, base_url: p.base_url, api_key: '' });
                      }}
                    >
                      Edit
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => void onDelete(p.id)} disabled={busyProvider === p.id}>
                      Remove
                    </Button>
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
        {adding ? (
          <div className="mt-3 rounded-md border border-[var(--color-border)] p-3">
            <ProviderFields
              draft={addDraft}
              onChange={setAddDraft}
              onSubmit={() => void onAdd()}
              onCancel={() => { setAdding(false); setAddDraft(emptyProviderDraft()); }}
              submitLabel={busyProvider === 'new' ? 'Adding…' : 'Add provider'}
              disabled={busyProvider === 'new'}
              keyHint="paste key"
            />
          </div>
        ) : (
          <div className="mt-3">
            <Button variant="outline" size="sm" onClick={() => setAdding(true)}>Add provider</Button>
          </div>
        )}
      </Panel>

      {TIERS.map(({ key, label, hint }) => {
        const tier = draft[key];
        const currentValue = tier.providerId && tier.model ? encodeTierValue(tier.providerId, tier.model) : '';
        const currentMissing = currentValue && !pickerOptions.some((o) => o.value === currentValue);
        const options = [
          ...(key !== 'flash' ? [inheritOption] : []),
          ...pickerOptions.map((o) => ({ value: o.value, label: o.label })),
          ...(currentMissing ? [{ value: currentValue, label: `${tier.model} · saved` }] : []),
        ];
        return (
          <Panel key={key} title={`${label} tier`}>
            <p className="mb-3 max-w-[560px] text-[12px] text-[var(--color-text-secondary)]">{hint}</p>
            <div>
              <label className={labelCls}>Model</label>
              <Selector
                label="Model"
                isLabelHidden
                options={options.length ? options : [{ value: '', label: listedLoading ? 'Loading models…' : 'Add a provider to list models' }]}
                value={currentValue}
                onChange={(v) => setTierValue(key, v)}
                width="100%"
              />
            </div>
          </Panel>
        );
      })}

      {listedErrors.length ? (
        <p className="max-w-[640px] text-[12.5px] text-[var(--color-danger,var(--color-text-secondary))]">
          Some providers could not list models:{' '}
          {listedErrors.map((e) => `${e.provider_id}: ${e.error}`).join('; ')}
        </p>
      ) : null}

      <Panel title="Escalation">
        <div className="max-w-[560px]">
          <CheckboxInput
            label="Escalate on failure"
            description="When a run fails at its starting tier, retry it on each higher tier before giving up."
            value={draft.model_fallback}
            onChange={(checked) => setDraft((d) => (d ? { ...d, model_fallback: checked } : d))}
          />
        </div>
      </Panel>

      <div className="flex items-center gap-2">
        <Button variant="primary" size="sm" onClick={() => void onSave()} disabled={saving}>
          {saving ? 'Saving…' : 'Save changes'}
        </Button>
        <Button variant="outline" size="sm" onClick={() => void onTest()} disabled={testing}>
          {testing ? 'Testing…' : 'Test connection'}
        </Button>
      </div>
    </div>
  );
}

function ProviderFields({
  draft,
  onChange,
  onSubmit,
  onCancel,
  submitLabel,
  disabled,
  keyHint,
}: {
  draft: ProviderDraft;
  onChange: (d: ProviderDraft) => void;
  onSubmit: () => void;
  onCancel: () => void;
  submitLabel: string;
  disabled: boolean;
  keyHint: string;
}) {
  const patch = (field: keyof ProviderDraft, value: string) => onChange({ ...draft, [field]: value });
  return (
    <div className="flex flex-col gap-3">
      <div>
        <label className={labelCls}>Vendor</label>
        <Selector
          label="Vendor"
          isLabelHidden
          options={VENDOR_KINDS.map((v) => ({ value: v.value, label: v.label }))}
          value={draft.vendor}
          onChange={(v) => patch('vendor', v)}
          width="100%"
        />
      </div>
      <div>
        <label className={labelCls}>Name <span className="text-[var(--color-text-disabled)]">(optional)</span></label>
        <TextInput label="Name" isLabelHidden value={draft.name} placeholder="Shown in the model picker" onChange={(v) => patch('name', v)} width="100%" />
      </div>
      {vendorNeedsBaseURL(draft.vendor) || draft.base_url || draft.vendor === 'google' ? (
        <div>
          <label className={labelCls}>
            Base URL
            {vendorNeedsBaseURL(draft.vendor) ? '' : <span className="text-[var(--color-text-disabled)]"> (optional)</span>}
          </label>
          <TextInput
            label="Base URL"
            isLabelHidden
            value={draft.base_url}
            placeholder={draft.vendor === 'openai-compat' ? 'https://api.example.com/v1' : 'Vendor default'}
            onChange={(v) => patch('base_url', v)}
            width="100%"
          />
        </div>
      ) : null}
      <div>
        <label className={labelCls}>API key <span className="ms-2 text-[var(--color-text-disabled)]">{keyHint}</span></label>
        <TextInput
          label="API key"
          isLabelHidden
          type="password"
          value={draft.api_key}
          placeholder={keyHint === 'key set' ? '•••••••• (unchanged)' : 'Paste provider key'}
          onChange={(v) => patch('api_key', v)}
          width="100%"
        />
      </div>
      <div className="flex gap-2">
        <Button variant="primary" size="sm" onClick={onSubmit} disabled={disabled}>{submitLabel}</Button>
        <Button variant="outline" size="sm" onClick={onCancel} disabled={disabled}>Cancel</Button>
      </div>
    </div>
  );
}
