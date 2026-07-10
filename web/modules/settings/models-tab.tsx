'use client';

import { useState } from 'react';
import { Grid } from '@astryxdesign/core/Grid';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import type { WorkspaceModelTiers, WorkspaceModelTiersInput } from '@/lib/api';
import { useWorkspaceModels } from '@/modules/agent/hooks';
import { Button, Loading, Panel } from '@/modules/shared/components/signal-primitives';

// The three workspace tiers. flash is the always-present default every project
// and agent inherits; lite/pro are additive overrides — a blank field falls back
// to flash, so the common "one key, different model per tier" setup needs the key
// entered only once (on flash).
const TIERS = [
  { key: 'flash', label: 'Default', hint: 'Balanced model every agent draws from. Required.' },
  { key: 'lite', label: 'Lite', hint: 'Cheaper model for mechanical steps. Blank fields inherit the default.' },
  { key: 'pro', label: 'Pro', hint: 'Stronger model for deep reasoning. Blank fields inherit the default.' },
] as const;

type TierKey = (typeof TIERS)[number]['key'];

// Providers AgentRay constructs by name (internal/agentcore/registry.go). Any
// OpenAI-compatible vendor is reached by picking "openai" + a custom Base URL.
const PROVIDERS = ['openai', 'anthropic'];

type TierDraft = { provider: string; model: string; base_url: string };
type Draft = {
  flash: TierDraft;
  lite: TierDraft;
  pro: TierDraft;
  keys: Record<TierKey, string>; // '' = keep stored key; any value = replace
  clear: Record<TierKey, boolean>; // true = clear the stored key ('-')
  model_fallback: boolean;
};

function draftFromConfig(c: WorkspaceModelTiers): Draft {
  return {
    flash: { provider: c.provider || 'openai', model: c.model, base_url: c.base_url },
    lite: { provider: c.lite_provider, model: c.lite_model, base_url: c.lite_base_url },
    pro: { provider: c.pro_provider, model: c.pro_model, base_url: c.pro_base_url },
    keys: { flash: '', lite: '', pro: '' },
    clear: { flash: false, lite: false, pro: false },
    model_fallback: c.model_fallback,
  };
}

function apiKeyField(key: string, clear: boolean): string {
  if (clear) return '-';
  return key; // '' leaves the stored key untouched server-side
}

function draftToInput(d: Draft): WorkspaceModelTiersInput {
  return {
    provider: d.flash.provider,
    model: d.flash.model,
    base_url: d.flash.base_url,
    api_key: apiKeyField(d.keys.flash, d.clear.flash),
    lite_provider: d.lite.provider,
    lite_model: d.lite.model,
    lite_base_url: d.lite.base_url,
    lite_api_key: apiKeyField(d.keys.lite, d.clear.lite),
    pro_provider: d.pro.provider,
    pro_model: d.pro.model,
    pro_base_url: d.pro.base_url,
    pro_api_key: apiKeyField(d.keys.pro, d.clear.pro),
    model_fallback: d.model_fallback,
  };
}

const labelCls = 'mb-1.5 block text-[12.5px] text-[var(--color-text-secondary)]';

function hasKeyFor(config: WorkspaceModelTiers, tier: TierKey): boolean {
  if (tier === 'flash') return config.has_key;
  if (tier === 'lite') return config.lite_has_key;
  return config.pro_has_key;
}

export function ModelsTab() {
  const { models, modelsLoading, saveModels, testModels } = useWorkspaceModels();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [seededFrom, setSeededFrom] = useState<WorkspaceModelTiers | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);

  // Reseed the form whenever the loaded config object changes (initial load +
  // post-save invalidation), discarding unsaved local edits in favour of server
  // truth. Adjusting state during render off a changed source is React's
  // recommended pattern — no effect needed.
  if (models && models !== seededFrom) {
    setSeededFrom(models);
    setDraft(draftFromConfig(models));
  }

  if (modelsLoading && !draft) return <Panel title="AI Provider"><Loading label="Loading model pool…" /></Panel>;
  if (!models || !draft) return <Panel title="AI Provider"><Loading label="Loading model pool…" /></Panel>;

  const patch = (tier: TierKey, field: keyof TierDraft, value: string) =>
    setDraft((d) => (d ? { ...d, [tier]: { ...d[tier], [field]: value } } : d));
  const setKey = (tier: TierKey, value: string) =>
    setDraft((d) => (d ? { ...d, keys: { ...d.keys, [tier]: value }, clear: { ...d.clear, [tier]: false } } : d));
  const toggleClear = (tier: TierKey) =>
    setDraft((d) => (d ? { ...d, clear: { ...d.clear, [tier]: !d.clear[tier] }, keys: { ...d.keys, [tier]: '' } } : d));

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

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[12.5px] text-[var(--color-text-secondary)]">
        The model pool is shared by every project and agent in this workspace. Configure it once: bring your own
        provider key per tier, and pick which model serves each tier. Keys are encrypted at rest and never returned.
        Only workspace owners and admins can change these.
      </p>

      {TIERS.map(({ key, label, hint }) => {
        const tier = draft[key];
        const keySet = hasKeyFor(models, key);
        const cleared = draft.clear[key];
        return (
          <Panel key={key} title={`${label} tier`}>
            <p className="mb-3 max-w-[560px] text-[12px] text-[var(--color-text-secondary)]">{hint}</p>
            <Grid columns={{ minWidth: 340, max: 2 }} gap={3}>
              <div>
                <label className={labelCls}>Provider</label>
                <Selector
                  label="Provider"
                  isLabelHidden
                  options={[
                    ...(key !== 'flash' ? [{ value: '', label: 'Inherit default' }] : []),
                    ...PROVIDERS.map((p) => ({ value: p, label: p })),
                    // Surface an already-stored OpenAI-compatible vendor name so the
                    // controlled selector always has a matching option.
                    ...(tier.provider && !PROVIDERS.includes(tier.provider) ? [{ value: tier.provider, label: tier.provider }] : []),
                  ]}
                  value={tier.provider}
                  onChange={(v) => patch(key, 'provider', v)}
                  width="100%"
                />
              </div>
              <div>
                <label className={labelCls}>Model</label>
                <TextInput
                  label="Model"
                  isLabelHidden
                  value={tier.model}
                  placeholder={key === 'flash' ? 'e.g. gpt-4o' : 'Inherit default'}
                  onChange={(v) => patch(key, 'model', v)}
                  width="100%"
                />
              </div>
              <div>
                <label className={labelCls}>Base URL <span className="text-[var(--color-text-disabled)]">(optional)</span></label>
                <TextInput
                  label="Base URL"
                  isLabelHidden
                  value={tier.base_url}
                  placeholder="OpenAI-compatible endpoint"
                  onChange={(v) => patch(key, 'base_url', v)}
                  width="100%"
                />
              </div>
              <div>
                <label className={labelCls}>
                  API key
                  <span className="ms-2 text-[var(--color-text-disabled)]">
                    {cleared ? 'will be cleared' : keySet ? 'key set' : key === 'flash' ? 'not set' : 'inherits default'}
                  </span>
                </label>
                <TextInput
                  label="API key"
                  isLabelHidden
                  type="password"
                  value={draft.keys[key]}
                  isDisabled={cleared}
                  placeholder={keySet ? '•••••••• (unchanged)' : 'Paste provider key'}
                  onChange={(v) => setKey(key, v)}
                  width="100%"
                />
                {keySet ? (
                  <button
                    type="button"
                    className="mt-1.5 text-[11.5px] text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]"
                    onClick={() => toggleClear(key)}
                  >
                    {cleared ? 'Keep stored key' : 'Clear stored key'}
                  </button>
                ) : null}
              </div>
            </Grid>
          </Panel>
        );
      })}

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
