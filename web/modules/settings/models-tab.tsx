'use client';

import { useMemo, useState } from 'react';
import { AlertTriangle, Check, KeyRound, LogIn, Plus, RefreshCw, Search, Trash2, X } from 'lucide-react';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { Text } from '@astryxdesign/core/Text';
import type {
  AgentConfigTestResult,
  WorkspaceModelTiers,
  WorkspaceModelTiersInput,
  WorkspaceProvider,
  WorkspaceProviderAccount,
  WorkspaceProviderInput,
} from '@/lib/api';
import { useProviderAccounts, useWorkspaceModels } from '@/modules/agent/hooks';
import { ConfirmDialog } from '@/modules/shared/components/modal';
import { Button, Callout, EmptyState, Loading, Panel, StatusPill } from '@/modules/shared/components/signal-primitives';
import { formatRelative } from '@/lib/format';
import { useStackSheet } from '@/modules/shared/components/stack-sheet';
import {
  friendlyProviderError,
  formatTokens,
  listedModelsToItems,
  searchModelItems,
  type ListedModel,
} from './model-picker';
import { isOAuthVendor, ProviderForm, providerFormTitle, vendorLabel } from './provider-form';
import { OAuthSignIn } from './oauth-signin';

// contextWindow is the operator's OVERRIDE only, in tokens; 0 means "use the
// window we detected for this model". Storing the override rather than the
// effective number is what lets a later model change re-detect instead of
// inheriting a stale figure.
type TierDraft = { providerId: string; model: string; contextWindow: number; fallbackModel: string };
type Draft = {
  flash: TierDraft;
  lite: TierDraft;
  pro: TierDraft;
  model_fallback: boolean;
};

// Blank means "inherit from Default" everywhere in this file — that is exactly
// what runtime.resolve() already does with an unset tier, and what
// SaveWorkspaceTierSelection persists.
const emptyTier = (): TierDraft => ({ providerId: '', model: '', contextWindow: 0, fallbackModel: '' });

// A tier is only selectable when it names both a configured provider and a
// model. A half-set tier is treated as unset, which is what the runtime does.
const tierDraft = (providerId: string, model: string, contextWindow: number, fallbackModel = ''): TierDraft =>
  providerId && model ? { providerId, model, contextWindow, fallbackModel } : emptyTier();

function draftFromConfig(c: WorkspaceModelTiers): Draft {
  return {
    flash: tierDraft(c.flash_provider_id || '', c.model || '', c.context_window || 0, c.fallback_model || ''),
    lite: tierDraft(c.lite_provider_id || '', c.lite_model || '', c.lite_context_window || 0, c.lite_fallback_model || ''),
    pro: tierDraft(c.pro_provider_id || '', c.pro_model || '', c.pro_context_window || 0, c.pro_fallback_model || ''),
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
    context_window: d.flash.contextWindow,
    lite_context_window: d.lite.contextWindow,
    pro_context_window: d.pro.contextWindow,
    fallback_model: d.flash.fallbackModel,
    lite_fallback_model: d.lite.fallbackModel,
    pro_fallback_model: d.pro.fallbackModel,
  };
}

// `models` snapshots what each tier pointed at when the check ran. Reading the
// live draft instead would relabel a finished result with a model that was
// never called, the moment the user touches a picker.
type TestState = {
  ok: boolean;
  tiers: Record<string, { ok: boolean; error?: string }>;
  models: Record<string, string>;
};

function AccountsSection({
  provider,
  onAddAccount,
  onDeleteAccount,
  onSetStatus,
  onProbeUsage,
}: {
  provider: WorkspaceProvider;
  onAddAccount: () => void;
  onDeleteAccount: (accountID: string) => Promise<unknown>;
  onSetStatus: (accountID: string, status: 'active' | 'disabled') => Promise<unknown>;
  onProbeUsage: (accountID: string) => Promise<unknown>;
}) {
  const { data, isLoading } = useProviderAccounts(provider.id);
  const accounts = data?.accounts ?? [];
  const [busy, setBusy] = useState<string | null>(null);

  return (
    <div className="flex flex-col gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium uppercase tracking-wider text-[var(--color-text-secondary)]">
          Connected accounts ({accounts.length})
        </span>
        <Button variant="ghost" size="sm" icon={<Plus size={12} />} onClick={onAddAccount}>
          Add account
        </Button>
      </div>

      {isLoading ? (
        <Loading label="Loading accounts…" />
      ) : !accounts.length ? (
        <div className="flex items-center justify-between py-2 text-xs text-[var(--color-text-secondary)]">
          <span>No accounts signed in yet.</span>
          <Button variant="primary" size="sm" icon={<LogIn size={12} />} onClick={onAddAccount}>
            Sign in now
          </Button>
        </div>
      ) : (
        <div className="flex flex-col divide-y divide-[var(--color-border)]">
          {accounts.map((acc) => {
            const isBlocked = acc.blocked_until && new Date(acc.blocked_until) > new Date();
            const isWorking = busy === acc.id;
            return (
              <div key={acc.id} className="flex items-center justify-between gap-3 py-2 text-xs">
                <div className="flex min-w-0 flex-col gap-0.5">
                  <div className="flex items-center gap-2">
                    <span className="truncate font-mono font-medium text-[var(--color-text-primary)]">
                      {acc.email || acc.account_id}
                    </span>
                    {acc.plan ? (
                      <span className="rounded bg-[var(--color-surface-3)] px-1.5 py-0.5 text-[10px] uppercase text-[var(--color-text-secondary)]">
                        {acc.plan}
                      </span>
                    ) : null}
                    {acc.org_name ? (
                      <span className="truncate text-[var(--color-faint)]">({acc.org_name})</span>
                    ) : null}
                    <StatusPill
                      grow={false}
                      status={acc.status === 'disabled' ? 'paused' : isBlocked ? 'attention' : 'healthy'}
                      label={acc.status === 'disabled' ? 'Disabled' : isBlocked ? 'Rate limited' : 'Active'}
                    />
                  </div>
                  <span className="text-[11px] text-[var(--color-faint)]">
                    {isBlocked ? `Cooling down until ${new Date(acc.blocked_until!).toLocaleTimeString()}` : null}
                    {!isBlocked && acc.last_used_at ? `Last used ${formatRelative(acc.last_used_at)}` : null}
                    {!isBlocked && !acc.last_used_at ? `Added ${formatRelative(acc.created_at)}` : null}
                  </span>
                </div>

                <div className="flex items-center gap-1 flex-none">
                  <button
                    title="Check usage"
                    aria-label="Check usage"
                    disabled={isWorking}
                    onClick={async () => {
                      setBusy(acc.id);
                      try { await onProbeUsage(acc.id); } finally { setBusy(null); }
                    }}
                    className="rounded p-1 text-[var(--color-text-secondary)] hover:bg-[var(--color-surface-3)] hover:text-[var(--color-text-primary)]"
                  >
                    <RefreshCw size={13} className={isWorking ? 'animate-spin' : ''} />
                  </button>
                  <button
                    title={acc.status === 'active' ? 'Pause this account' : 'Resume this account'}
                    aria-label={acc.status === 'active' ? 'Pause account' : 'Resume account'}
                    disabled={isWorking}
                    onClick={async () => {
                      setBusy(acc.id);
                      try {
                        await onSetStatus(acc.id, acc.status === 'active' ? 'disabled' : 'active');
                      } finally {
                        setBusy(null);
                      }
                    }}
                    className="rounded px-1.5 py-0.5 text-[11px] text-[var(--color-text-secondary)] hover:bg-[var(--color-surface-3)]"
                  >
                    {acc.status === 'active' ? 'Pause' : 'Resume'}
                  </button>
                  <button
                    title="Remove account"
                    aria-label="Remove account"
                    disabled={isWorking}
                    onClick={async () => {
                      setBusy(acc.id);
                      try { await onDeleteAccount(acc.id); } finally { setBusy(null); }
                    }}
                    className="rounded p-1 text-[var(--color-text-secondary)] hover:bg-[var(--color-surface-3)] hover:text-danger"
                  >
                    <Trash2 size={13} />
                  </button>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

const PROVIDER_SHEET = 'ai-provider-form';
const OAUTH_SHEET = 'ai-oauth-form';

const TIERS = [
  { key: 'flash', title: 'Default', does: 'The main analysis and answers.', required: true },
  { key: 'lite', title: 'Lite', does: 'Quick routing and history compression.', required: false },
  { key: 'pro', title: 'Pro', does: 'Learning and review. Costs the most.', required: false },
] as const;
type TierKey = (typeof TIERS)[number]['key'];

export function ModelsTab() {
  const {
    models, modelsLoading, modelsError, providers, providersError, listedModels, listedErrors, listedLoading,
    saveModels, testModels, createProvider, updateProvider, deleteProvider,
    deleteAccount, setAccountStatus, probeUsage, refreshAccounts, retryLoad,
  } = useWorkspaceModels();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [seededFrom, setSeededFrom] = useState<WorkspaceModelTiers | null>(null);
  const [activeTier, setActiveTier] = useState<TierKey>('flash');
  const [selectedProvider, setSelectedProvider] = useState('');
  const [query, setQuery] = useState('');
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testState, setTestState] = useState<TestState | null>(null);
  const [deleting, setDeleting] = useState<WorkspaceProvider | null>(null);
  const [busyProvider, setBusyProvider] = useState<string | null>(null);
  const { push, closeById } = useStackSheet();

  const configured = providers as WorkspaceProvider[];
  const listed = listedModels as ListedModel[];

  if (models && models !== seededFrom) {
    setSeededFrom(models);
    setDraft(draftFromConfig(models));
  }

  // Default the rail to the provider the Default tier already uses, else the
  // first configured one.
  const provider = !configured.length
    ? null
    : (configured.find((p) => p.id === selectedProvider) ??
      configured.find((p) => p.id === draft?.flash.providerId) ??
      configured[0]);

  // The search is the same tolerant matcher the old Typeahead used — model ids
  // are punctuation-heavy and nobody types the hyphens in the right places.
  // Memoized: every keystroke and draft change would otherwise re-fold the
  // whole catalog through the NFD/regex pipeline.
  const providerItems = useMemo(
    () => (!provider ? [] : listedModelsToItems(listed.filter((m) => m.provider_id === provider.id))),
    [listed, provider],
  );
  const providerModels = useMemo(() => searchModelItems(providerItems, query), [providerItems, query]);
  const modelCount = useMemo(() => {
    const m = new Map<string, number>();
    for (const x of listed) m.set(x.provider_id, (m.get(x.provider_id) ?? 0) + 1);
    return m;
  }, [listed]);

  if (modelsError || providersError) {
    const err = modelsError ?? providersError;
    return (
      <Panel title="AI Provider">
        <Callout
          tone="warn"
          icon={<AlertTriangle size={16} />}
          label="AI Provider unavailable"
          title="Could not load the model pool"
          detail={err instanceof Error ? err.message : 'The workspace AI configuration could not be loaded.'}
          action={<Button variant="outline" size="sm" className="min-h-[44px]" onClick={retryLoad}>Retry</Button>}
        />
      </Panel>
    );
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

  const errorByProvider = new Map<string, string>();
  const orphanErrors: string[] = [];
  for (const e of listedErrors) {
    const message = friendlyProviderError(e.error);
    if (configured.some((p) => p.id === e.provider_id)) errorByProvider.set(e.provider_id, message);
    else orphanErrors.push(message);
  }

  const tierLabel = (key: TierKey) => {
    const t = draft[key];
    if (!t.providerId || !t.model) return key === 'flash' ? 'Not set — pick a model' : 'Same as Default';
    return `${t.model} · ${providerName(t.providerId)}`;
  };

  const assignModel = (modelId: string) => {
    if (!provider) return;
    setDraft((d) =>
      d
        ? {
            ...d,
            [activeTier]: {
              providerId: provider.id,
              model: modelId,
              // Changing the model drops the window override: it was a
              // statement about the model that was there.
              contextWindow: d[activeTier].model === modelId ? d[activeTier].contextWindow : 0,
              // The fallback belongs to the tier's provider — a provider switch
              // drops it, and promoting the fallback to primary clears it.
              fallbackModel:
                d[activeTier].providerId === provider.id && d[activeTier].fallbackModel !== modelId
                  ? d[activeTier].fallbackModel
                  : '',
            },
          }
        : d,
    );
  };

  // The fallback is a second model of the tier's own provider, retried when the
  // primary model's call fails. It only exists once the tier has a model on
  // this provider, and it can never be the primary itself.
  const toggleFallback = (modelId: string) => {
    if (!provider) return;
    setDraft((d) => {
      if (!d) return d;
      const t = d[activeTier];
      if (t.providerId !== provider.id || !t.model || t.model === modelId) return d;
      return { ...d, [activeTier]: { ...t, fallbackModel: t.fallbackModel === modelId ? '' : modelId } };
    });
  };

  const clearTier = (key: TierKey) => {
    setDraft((d) => (d ? { ...d, [key]: emptyTier() } : d));
  };

  const setTierWindow = (key: TierKey, tokens: number) => {
    setDraft((d) => (d ? { ...d, [key]: { ...d[key], contextWindow: tokens } } : d));
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
      setTestState({
        ok: res.ok,
        tiers: res.tiers ?? {},
        models: { flash: models.model, lite: models.lite_model, pro: models.pro_model },
      });
    } catch {
      // useWorkspaceModels already surfaced the failure as a toast.
    } finally {
      setTesting(false);
    }
  };

  const openProviderSheet = (p: WorkspaceProvider | null) => {
    push({
      id: PROVIDER_SHEET,
      title: providerFormTitle(p),
      content: (
        <ProviderForm
          provider={p}
          onSubmit={async (input) => {
            if (p) await updateProvider(p.id, input);
            else await createProvider(input);
            closeById(PROVIDER_SHEET);
          }}
          onCancel={() => closeById(PROVIDER_SHEET)}
        />
      ),
    });
  };
  const openOAuthSheet = (p: WorkspaceProvider) => {
    push({
      id: OAUTH_SHEET,
      title: `Sign in to ${p.name || vendorLabel(p.vendor)}`,
      content: (
        <OAuthSignIn
          provider={p}
          onDone={() => {
            closeById(OAUTH_SHEET);
            refreshAccounts(p.id);
          }}
          onCancel={() => closeById(OAUTH_SHEET)}
        />
      ),
    });
  };

  const onDelete = async (id: string) => {
    setBusyProvider(id);
    try {
      await deleteProvider(id);
      setDraft((d) => {
        if (!d) return d;
        const clear = (t: TierDraft): TierDraft => (t.providerId === id ? emptyTier() : t);
        return { ...d, flash: clear(d.flash), lite: clear(d.lite), pro: clear(d.pro) };
      });
      if (provider?.id === id) setSelectedProvider('');
    } finally {
      setBusyProvider(null);
    }
  };

  const hasProviders = configured.length > 0;
  const activeTierDraft = draft[activeTier];
  const activeModel = providerItems.find((m) => m.label === activeTierDraft.model && activeTierDraft.providerId === provider?.id);

  return (
    <div className="flex flex-col gap-4">
      <p className="max-w-[640px] text-sm text-[var(--color-text-primary)]">
        {hasProviders
          ? 'Your agents think with these keys. Pick a provider on the left, then click a model to assign it to the selected tier.'
          : models.hosted_default
            ? 'Your agents are running on the included hosted model. Add your own key to use a different provider — it is encrypted and never shown again.'
            : 'Your agents need an AI provider to think. Add a key once, then choose which model handles which kind of work.'}
      </p>

      {/* Tier chips — the assignment target (omp's Roles view) */}
      <Panel title="Which model does what">
        <div className="flex flex-wrap gap-2">
          {TIERS.map((t) => {
            const a = draft[t.key];
            const set = a.providerId && a.model;
            const active = activeTier === t.key;
            return (
              <button
                key={t.key}
                onClick={() => setActiveTier(t.key)}
                aria-pressed={active}
                className={`flex min-h-[44px] min-w-[200px] flex-col items-start gap-0.5 rounded-[var(--radius-md)] border px-3 py-2 text-left transition-colors ${
                  active
                    ? 'border-[var(--color-primary)] bg-[var(--color-surface-3)]'
                    : 'border-[var(--color-border)] bg-[var(--color-surface-2)] hover:bg-[var(--color-surface-3)]'
                }`}
              >
                <span className="flex items-center gap-2 text-sm text-[var(--color-text-primary)]">
                  {t.title}
                  {t.required ? <span className="text-xs text-[var(--color-text-secondary)]">Required</span> : null}
                  {active ? <span className="text-xs text-[var(--color-primary)]">← assigning</span> : null}
                </span>
                <span className="text-xs text-[var(--color-text-secondary)]">{t.does}</span>
                <span className={`mt-1 font-mono text-xs ${set ? 'text-[var(--color-text-primary)]' : 'text-[var(--color-faint)]'}`}>
                  {tierLabel(t.key)}
                </span>
                {a.fallbackModel ? (
                  <span className="font-mono text-xs text-[var(--color-text-secondary)]">fallback → {a.fallbackModel}</span>
                ) : null}
                {set && t.key !== 'flash' ? (
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      clearTier(t.key);
                    }}
                    className="mt-0.5 text-xs text-[var(--color-text-secondary)] underline-offset-2 hover:underline"
                  >
                    Reset to Default
                  </button>
                ) : null}
              </button>
            );
          })}
        </div>
      </Panel>

      {/* Hub: provider rail + scoped model browser */}
      <div className="grid grid-cols-1 gap-4 md:grid-cols-[280px_minmax(0,1fr)]">
        <Panel
          title="Providers"
          action={
            hasProviders ? (
              <Button variant="ghost" size="sm" icon={<Plus size={14} />} onClick={() => openProviderSheet(null)}>
                Add
              </Button>
            ) : undefined
          }
        >
          {!hasProviders ? (
            <EmptyState
              icon={<KeyRound size={20} />}
              title="No provider yet"
              detail="Paste an API key from OpenAI, Google Gemini, or a custom OpenAI-compatible endpoint."
              action={
                <Button variant="primary" size="sm" icon={<Plus size={15} />} onClick={() => openProviderSheet(null)}>
                  Add provider
                </Button>
              }
            />
          ) : (
            <div className="flex flex-col gap-1">
              {configured.map((p) => {
                const failure = errorByProvider.get(p.id);
                const active = provider?.id === p.id;
                const count = modelCount.get(p.id) ?? 0;
                const isOAuth = isOAuthVendor(p.vendor);
                const badge = failure
                  ? { status: 'attention' as const, label: isOAuth ? 'Auth error' : 'Key rejected' }
                  : isOAuth
                    ? (p.account_count ?? 0) > 0
                      ? { status: 'healthy' as const, label: `${p.account_count} account${p.account_count === 1 ? '' : 's'}` }
                      : { status: 'paused' as const, label: 'Needs sign-in' }
                    : p.has_key
                      ? { status: 'healthy' as const, label: 'Connected' }
                      : { status: 'paused' as const, label: 'Needs a key' };
                return (
                  <button
                    key={p.id}
                    onClick={() => setSelectedProvider(p.id)}
                    aria-pressed={active}
                    className={`flex min-h-[44px] w-full flex-col items-start gap-0.5 rounded-[var(--radius-md)] px-3 py-2 text-left transition-colors ${
                      active ? 'bg-[var(--color-surface-3)]' : 'hover:bg-[var(--color-surface-2)]'
                    }`}
                  >
                    <span className="flex w-full items-center justify-between gap-2">
                      <span className="truncate text-sm text-[var(--color-text-primary)]">{p.name || vendorLabel(p.vendor)}</span>
                      <StatusPill grow={false} status={badge.status} label={badge.label} />
                    </span>
                    <span className="text-xs text-[var(--color-text-secondary)]">
                      {vendorLabel(p.vendor)}
                      {p.base_url ? ` · ${p.base_url.replace(/^https?:\/\//, '')}` : ''}
                      {count ? ` · ${count} models` : ''}
                    </span>
                    {failure ? (
                      <span role="alert" className="mt-1 flex items-start gap-1 text-xs text-danger">
                        <AlertTriangle size={12} className="mt-0.5 flex-none" />
                        <span className="line-clamp-2">{failure}</span>
                      </span>
                    ) : null}
                  </button>
                );
              })}
            </div>
          )}
          {orphanErrors.length ? (
            <div role="alert" className="mt-2 flex items-start gap-2 text-xs text-danger">
              <AlertTriangle size={13} className="mt-0.5 flex-none" />
              <span>{orphanErrors.join(' · ')}</span>
            </div>
          ) : null}
        </Panel>

        <Panel
          title={provider ? `${provider.name || vendorLabel(provider.vendor)} models` : 'Models'}
          action={
            provider ? (
              <div className="flex gap-1">
                {isOAuthVendor(provider.vendor) ? (
                  <Button variant="ghost" size="sm" icon={<LogIn size={13} />} onClick={() => openOAuthSheet(provider)}>
                    Add account
                  </Button>
                ) : (
                  <Button variant="ghost" size="sm" onClick={() => openProviderSheet(provider)} disabled={busyProvider === provider.id}>
                    Replace key
                  </Button>
                )}
                <Button variant="ghost" size="sm" onClick={() => setDeleting(provider)} disabled={busyProvider === provider.id}>
                  Remove
                </Button>
              </div>
            ) : undefined
          }
        >
          {!provider ? (
            <EmptyState title="Add a provider first" detail="Once a key is saved, its models show up here." />
          ) : errorByProvider.get(provider.id) ? (
            <EmptyState
              icon={<AlertTriangle size={20} />}
              title="Can't reach this provider"
              detail={errorByProvider.get(provider.id)}
              action={
                <Button variant="outline" size="sm" onClick={() => openProviderSheet(provider)}>
                  Replace key
                </Button>
              }
            />
          ) : (
            <div className="flex flex-col gap-3">
              {isOAuthVendor(provider.vendor) ? (
                <AccountsSection
                  provider={provider}
                  onAddAccount={() => openOAuthSheet(provider)}
                  onDeleteAccount={(accID) => deleteAccount(provider.id, accID)}
                  onSetStatus={(accID, status) => setAccountStatus(provider.id, accID, status)}
                  onProbeUsage={(accID) => probeUsage(provider.id, accID)}
                />
              ) : null}
              <div className="flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3">
                <Search size={14} className="flex-none text-[var(--color-text-secondary)]" />
                <input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder={`Search ${provider.name || vendorLabel(provider.vendor)} models…`}
                  aria-label="Search models"
                  className="h-10 w-full bg-transparent text-sm text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-faint)]"
                />
                {query ? (
                  <button onClick={() => setQuery('')} aria-label="Clear search" className="text-[var(--color-text-secondary)]">
                    <X size={14} />
                  </button>
                ) : null}
              </div>

              {listedLoading ? (
                <Loading label="Loading the models your providers offer…" />
              ) : providerModels.length ? (
                <div className="flex flex-col">
                  {providerModels.map((m) => {
                    const assigned = TIERS.filter((t) => draft[t.key].providerId === provider.id && draft[t.key].model === m.label);
                    const isFallback = activeTierDraft.providerId === provider.id && activeTierDraft.fallbackModel === m.label;
                    const canFallback =
                      activeTierDraft.providerId === provider.id && activeTierDraft.model !== '' && activeTierDraft.model !== m.label;
                    return (
                      <button
                        key={m.id}
                        onClick={() => assignModel(m.label)}
                        className="flex min-h-[40px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] px-3 py-1.5 text-left transition-colors hover:bg-[var(--color-surface-2)]"
                      >
                        <span className="flex items-center gap-2">
                          <span className="font-mono text-sm text-[var(--color-text-primary)]">{m.label}</span>
                          {assigned.map((t) => (
                            <span key={t.key} className="rounded-[var(--radius-sm)] bg-[var(--color-surface-3)] px-1.5 py-0.5 text-xs text-[var(--color-primary)]">
                              {t.title}
                            </span>
                          ))}
                          {isFallback ? (
                            <span className="rounded-[var(--radius-sm)] bg-[var(--color-surface-3)] px-1.5 py-0.5 text-xs text-[var(--color-text-secondary)]">
                              fallback
                            </span>
                          ) : null}
                        </span>
                        <span className="flex items-center gap-2 text-xs text-[var(--color-text-secondary)]">
                          {m.auxiliaryData.contextWindow ? `${formatTokens(m.auxiliaryData.contextWindow)} ctx` : 'ctx unknown'}
                          {canFallback || isFallback ? (
                            <button
                              onClick={(e) => {
                                e.stopPropagation();
                                toggleFallback(m.label);
                              }}
                              title={isFallback ? 'Remove as fallback' : `Use as fallback for ${TIERS.find((t) => t.key === activeTier)?.title}`}
                              aria-pressed={isFallback}
                              className={`rounded-[var(--radius-sm)] p-1 transition-colors ${
                                isFallback ? 'text-[var(--color-primary)]' : 'text-[var(--color-faint)] hover:text-[var(--color-text-secondary)]'
                              }`}
                            >
                              <RefreshCw size={13} />
                            </button>
                          ) : null}
                        </span>
                      </button>
                    );
                  })}
                </div>
              ) : (
                <EmptyState
                  title={query ? 'No model matches' : 'No models listed'}
                  detail={query ? 'Try a different search.' : 'This provider did not return a model list — check the key.'}
                />
              )}

              <p className="text-xs text-[var(--color-text-secondary)]">
                Click a model to assign it to <strong>{TIERS.find((t) => t.key === activeTier)?.title}</strong>.
              </p>

              {/* Context-window override for the active tier's chosen model —
                  only when that model belongs to the provider in view, or the
                  field would show a foreign model's window and write its
                  override onto the wrong provider's draft. */}
              {activeTierDraft.providerId === provider.id && activeTierDraft.model ? (
                <ContextWindowField
                  detected={activeModel?.auxiliaryData.contextWindow ?? 0}
                  override={activeTierDraft.contextWindow}
                  onChange={(tokens) => setTierWindow(activeTier, tokens)}
                />
              ) : null}
            </div>
          )}
        </Panel>
      </div>

      {hasProviders ? (
        <div className="flex flex-col gap-3">
          <div className="border-t border-[var(--color-border)] pt-4">
            <CheckboxInput
              label="If a model call fails, retry it on the tier's fallback model"
              description="Each tier can name a second model of the same provider (the refresh icon in the list). Off means a failed call ends the run."
              value={draft.model_fallback}
              onChange={(checked) => setDraft((d) => (d ? { ...d, model_fallback: checked } : d))}
            />
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="primary" size="sm" onClick={() => void onSave()} disabled={saving}>
              {saving ? 'Saving…' : 'Save changes'}
            </Button>
            <Button variant="outline" size="sm" onClick={() => void onTest()} disabled={testing}>
              {testing ? 'Checking…' : 'Check it works'}
            </Button>
            <Text type="supporting">Checking sends one real message per model — it can take up to a minute.</Text>
          </div>

          {testing ? (
            <div className="rounded-md border border-[var(--color-border)] p-3 text-sm text-[var(--color-text-secondary)]">
              Checking your models… you can stay on this page.
            </div>
          ) : testState ? (
            <div className="rounded-md border border-[var(--color-border)] p-3">
              <div className="mb-2 text-xs text-[var(--color-text-secondary)]">
                {testState.ok ? 'Everything answered.' : 'Some models did not answer.'}
              </div>
              <div className="flex flex-col gap-2">
                {TIERS.map((tier) => {
                  const result = testState.tiers[tier.key];
                  const model = testState.models[tier.key];
                  return (
                    <div key={tier.key} className="flex flex-wrap items-baseline gap-2 text-sm">
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

      <p className="max-w-[640px] text-xs text-[var(--color-text-secondary)]">
        Only workspace owners and admins can change these.
      </p>

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

// ContextWindowField is the operator's override for the active tier's model.
// 0/blank means "use what the provider or catalog reported".
function ContextWindowField({
  detected,
  override,
  onChange,
}: {
  detected: number;
  override: number;
  onChange: (tokens: number) => void;
}) {
  return (
    <div className="mt-2 max-w-[280px]">
      <label className="mb-1 block text-xs text-[var(--color-text-secondary)]" htmlFor="tier-ctx-override">
        Context window override (tokens)
      </label>
      <input
        id="tier-ctx-override"
        type="number"
        min={0}
        value={override || ''}
        placeholder={detected ? `${detected}` : 'auto'}
        onChange={(e) => onChange(Math.max(0, parseInt(e.target.value, 10) || 0))}
        className="h-9 w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 text-sm text-[var(--color-text-primary)] outline-none"
      />
      <p className="mt-1 text-xs text-[var(--color-text-secondary)]">
        {detected ? `Detected ${formatTokens(detected)} — leave blank to use it.` : 'No detected window — set one for a self-hosted model.'}
      </p>
    </div>
  );
}
