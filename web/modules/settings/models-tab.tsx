'use client';

import { useMemo, useState } from 'react';
import { AlertTriangle, Check, KeyRound, Plus, X } from 'lucide-react';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { Text } from '@astryxdesign/core/Text';
import type {
  AgentConfigTestResult,
  WorkspaceModelTiers,
  WorkspaceModelTiersInput,
  WorkspaceProvider,
  WorkspaceProviderInput,
} from '@/lib/api';
import { useWorkspaceModels } from '@/modules/agent/hooks';
import { ConfirmDialog } from '@/modules/shared/components/modal';
import { Button, EmptyState, Loading, Panel, StatusPill } from '@/modules/shared/components/signal-primitives';
import {
  decodeTierValue,
  encodeTierValue,
  friendlyProviderError,
  listedModelsToItems,
  savedModelItem,
  searchModelItems,
  type ListedModel,
  type ModelPickerItem,
} from './model-picker';
import { ProviderDialog, vendorLabel } from './provider-dialog';
import { TIERS, TierBlock, type TierKey } from './tier-block';

type TierDraft = { providerId: string; model: string };
type Draft = {
  flash: TierDraft;
  lite: TierDraft;
  pro: TierDraft;
  model_fallback: boolean;
};

// Blank means "inherit from Default" everywhere in this file — that is exactly
// what runtime.resolve() already does with an unset tier, and what
// SaveWorkspaceTierSelection persists. The checkbox is the plain-language name
// for it; nothing new goes over the wire.
const isInherited = (t: TierDraft) => !t.providerId && !t.model;

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

type TestState = { at: number; ok: boolean; tiers: Record<string, { ok: boolean; error?: string }> };

export function ModelsTab() {
  const {
    models, modelsLoading, providers, listedModels, listedErrors, listedLoading,
    saveModels, testModels, createProvider, updateProvider, deleteProvider,
  } = useWorkspaceModels();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [seededFrom, setSeededFrom] = useState<WorkspaceModelTiers | null>(null);
  const [inherit, setInherit] = useState<Record<'lite' | 'pro', boolean>>({ lite: true, pro: true });
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testState, setTestState] = useState<TestState | null>(null);
  const [dialog, setDialog] = useState<{ provider: WorkspaceProvider | null } | null>(null);
  const [deleting, setDeleting] = useState<WorkspaceProvider | null>(null);
  const [busyProvider, setBusyProvider] = useState<string | null>(null);

  const configured = providers as WorkspaceProvider[];
  const listed = listedModels as ListedModel[];

  // The Typeahead's whole catalog is already in memory (one listed-models
  // fetch), so search is a synchronous filter — hence debounceMs={0} below.
  const items = useMemo(() => listedModelsToItems(listed), [listed]);
  const searchSource = useMemo(
    () => ({ search: (q: string) => searchModelItems(items, q), bootstrap: () => items.slice(0, 8) }),
    [items],
  );

  if (models && models !== seededFrom) {
    setSeededFrom(models);
    const next = draftFromConfig(models);
    setDraft(next);
    setInherit({ lite: isInherited(next.lite), pro: isInherited(next.pro) });
  }

  if (!models || !draft) {
    return (
      <Panel title="AI Provider">
        <Loading label={modelsLoading ? 'Loading your AI setup…' : 'Loading model pool…'} />
      </Panel>
    );
  }

  const providerName = (id: string) => {
    const p = configured.find((x) => x.id === id);
    return p ? p.name || vendorLabel(p.vendor) : '';
  };

  // ai.ListError only carries a provider UUID, so the readable name is joined
  // here — a failure has to say which key broke, not print an id.
  const errorByProvider = new Map<string, string>();
  const orphanErrors: string[] = [];
  for (const e of listedErrors) {
    const message = friendlyProviderError(e.error);
    if (configured.some((p) => p.id === e.provider_id)) errorByProvider.set(e.provider_id, message);
    else orphanErrors.push(message);
  }

  const tierItem = (key: TierKey): ModelPickerItem | null => {
    const t = draft[key];
    if (!t.providerId || !t.model) return null;
    const value = encodeTierValue(t.providerId, t.model);
    return items.find((i) => i.id === value) ?? savedModelItem(t.providerId, t.model, providerName(t.providerId));
  };

  const setTierItem = (key: TierKey, item: ModelPickerItem | null) => {
    if (!item) {
      setDraft((d) => (d ? { ...d, [key]: { providerId: '', model: '' } } : d));
      return;
    }
    const { providerId, modelId } = decodeTierValue(item.id);
    setDraft((d) => (d ? { ...d, [key]: { providerId, model: modelId } } : d));
  };

  const setInheritTier = (key: 'lite' | 'pro', checked: boolean) => {
    setInherit((s) => ({ ...s, [key]: checked }));
    if (checked) setDraft((d) => (d ? { ...d, [key]: { providerId: '', model: '' } } : d));
  };

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
    setTestState(null);
    try {
      const res: AgentConfigTestResult = await testModels();
      setTestState({ at: Date.now(), ok: res.ok, tiers: res.tiers ?? {} });
    } catch {
      // useWorkspaceModels already surfaced the failure as a toast; leaving
      // testState null keeps the last good result from being misread as new.
    } finally {
      setTesting(false);
    }
  };

  const onSubmitProvider = async (input: WorkspaceProviderInput) => {
    const target = dialog?.provider;
    setBusyProvider(target?.id ?? 'new');
    try {
      if (target) await updateProvider(target.id, input);
      else await createProvider(input);
      setDialog(null);
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

  const hasProviders = configured.length > 0;

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[13px] text-[var(--color-text-primary)]">
        {hasProviders
          ? 'Your agents think with the keys below. Choose which model handles which kind of work.'
          : models.hosted_default
            ? 'Your agents are running on the included hosted model. Add your own key to use a different provider — it is encrypted and never shown again.'
            : 'Your agents need an AI provider to think. Add a key once, then choose which model handles which kind of work.'}
      </p>

      {/* ---- Step 1 ---- */}
      <Panel
        title="1 · Your AI provider"
        action={
          hasProviders ? (
            <Button variant="outline" size="sm" icon={<Plus size={15} />} onClick={() => setDialog({ provider: null })}>
              Add provider
            </Button>
          ) : undefined
        }
      >
        {!hasProviders ? (
          <EmptyState
            icon={<KeyRound size={20} />}
            title="No provider yet"
            detail="Paste an API key from OpenAI, Anthropic, or Google and your agents can start answering. The key is encrypted and never shown again."
            action={
              <Button variant="primary" size="sm" icon={<Plus size={15} />} onClick={() => setDialog({ provider: null })}>
                Add provider
              </Button>
            }
          />
        ) : (
          <div className="flex flex-col gap-2">
            {configured.map((p) => {
              const failure = errorByProvider.get(p.id);
              return (
                <div key={p.id} className="rounded-md border border-[var(--color-border)] p-3">
                  <div className="flex flex-wrap items-start justify-between gap-2">
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="text-[13px] text-[var(--color-text-primary)]">
                          {p.name || vendorLabel(p.vendor)}
                        </span>
                        <StatusPill
                          grow={false}
                          status={failure ? 'attention' : 'healthy'}
                          label={failure ? 'Key rejected' : 'Connected'}
                        />
                      </div>
                      <div className="mt-0.5 text-[12px] text-[var(--color-text-secondary)]">
                        {vendorLabel(p.vendor)}
                        {p.base_url ? ` · ${p.base_url}` : ''}
                        {' · '}
                        {p.has_key ? 'key saved' : 'no key yet'}
                      </div>
                      {failure ? (
                        <div role="alert" className="mt-2 flex items-start gap-1.5 text-[12px] text-danger">
                          <AlertTriangle size={13} className="mt-0.5 flex-none" />
                          <span>{failure}</span>
                        </div>
                      ) : null}
                    </div>
                    <div className="flex flex-none gap-1">
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setDialog({ provider: p })}
                        disabled={busyProvider === p.id}
                      >
                        Replace key
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => setDeleting(p)} disabled={busyProvider === p.id}>
                        Remove
                      </Button>
                    </div>
                  </div>
                </div>
              );
            })}
            {orphanErrors.length ? (
              <div role="alert" className="flex items-start gap-1.5 text-[12px] text-danger">
                <AlertTriangle size={13} className="mt-0.5 flex-none" />
                <span>{orphanErrors.join(' · ')}</span>
              </div>
            ) : null}
          </div>
        )}
      </Panel>

      {/* ---- Step 2 ---- */}
      <Panel title="2 · Which model does what">
        <p className="mb-4 max-w-[600px] text-[12.5px] text-[var(--color-text-secondary)]">
          Match the brainpower to the job. A lighter model is fine for quick steps — save the strongest one for where
          depth matters. This is the simplest way to control cost.
        </p>

        {!hasProviders ? (
          <EmptyState title="Add a provider first" detail="Once a key is saved, its models show up here." />
        ) : (
          <div className="flex flex-col gap-4">
            {TIERS.map((tier) => {
              // Only Lite and Pro can inherit; Default is the tier they inherit from.
              const optional = tier.key === 'lite' || tier.key === 'pro' ? tier.key : null;
              return (
                <TierBlock
                  key={tier.key}
                  tier={tier}
                  value={tierItem(tier.key)}
                  inherit={optional ? inherit[optional] : false}
                  onInheritChange={optional ? (checked) => setInheritTier(optional, checked) : null}
                  onChange={(item) => setTierItem(tier.key, item)}
                  searchSource={searchSource}
                  isLoading={listedLoading}
                />
              );
            })}

            <div className="border-t border-[var(--color-border)] pt-4">
              <CheckboxInput
                label="If a run fails, retry it on a stronger model"
                description="Costs more on the runs that fail, but they finish instead of erroring out."
                value={draft.model_fallback}
                onChange={(checked) => setDraft((d) => (d ? { ...d, model_fallback: checked } : d))}
              />
            </div>
          </div>
        )}
      </Panel>

      {hasProviders ? (
        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="primary" size="sm" onClick={() => void onSave()} disabled={saving}>
              {saving ? 'Saving…' : 'Save changes'}
            </Button>
            <Button variant="outline" size="sm" onClick={() => void onTest()} disabled={testing}>
              {testing ? 'Checking…' : 'Check it works'}
            </Button>
            <Text type="supporting">Checking sends one real message per model — it can take up to a minute.</Text>
          </div>

          {/* The check runs for up to a minute and the toast is gone in four
              seconds, so the outcome lives here until the next check. */}
          {testing ? (
            <div className="rounded-md border border-[var(--color-border)] p-3 text-[12.5px] text-[var(--color-text-secondary)]">
              Checking your models… you can stay on this page.
            </div>
          ) : testState ? (
            <div className="rounded-md border border-[var(--color-border)] p-3">
              <div className="mb-2 text-[12px] text-[var(--color-text-secondary)]">
                {testState.ok ? 'Everything answered.' : 'Some models did not answer.'}
              </div>
              <div className="flex flex-col gap-1.5">
                {TIERS.map((tier) => {
                  const result = testState.tiers[tier.key];
                  const model = draft[tier.key].model;
                  return (
                    <div key={tier.key} className="flex flex-wrap items-baseline gap-2 text-[12.5px]">
                      {!result ? (
                        <Check size={14} className="flex-none text-[var(--color-text-disabled)]" />
                      ) : result.ok ? (
                        <Check size={14} className="flex-none text-success" />
                      ) : (
                        <X size={14} className="flex-none text-danger" />
                      )}
                      <span className="text-[var(--color-text-primary)]">{tier.title}</span>
                      <span className="text-[var(--color-text-secondary)]">
                        {result ? model || 'default model' : 'same as Default'}
                      </span>
                      {result?.error ? (
                        <span className="text-danger">— {friendlyProviderError(result.error)}</span>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            </div>
          ) : null}
        </div>
      ) : null}

      <p className="max-w-[640px] text-[12px] text-[var(--color-text-secondary)]">
        Only workspace owners and admins can change these.
      </p>

      {dialog ? (
        <ProviderDialog
          key={dialog.provider?.id ?? 'new'}
          provider={dialog.provider}
          busy={busyProvider !== null}
          onSubmit={(input) => void onSubmitProvider(input)}
          onClose={() => setDialog(null)}
        />
      ) : null}

      {deleting ? (
        <ConfirmDialog
          title={`Remove ${deleting.name || vendorLabel(deleting.vendor)}?`}
          detail="Its key is deleted and any tier using one of its models falls back to the Default. You can add it again later."
          confirmLabel="Remove"
          danger
          onConfirm={() => void onDelete(deleting.id)}
          onClose={() => setDeleting(null)}
        />
      ) : null}
    </div>
  );
}
