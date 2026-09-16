'use client';

// PROTOTYPE — AI Provider rebuild (omp model-hub structure on web).
// Throwaway. implement rebuilds from the spec. Not linked from navigation.
// Open: http://localhost:3200/prototype/ai-provider
//
// Stolen structure (can1357/oh-my-pi model-hub):
//   scope sidebar (providers) → model browser (live list) → role rows (tiers).
// Web adaptation: tier chips on top are the assignment target; the left rail
// scopes the model list to one provider; clicking a model assigns it.

import { useMemo, useState } from 'react';
import { AlertTriangle, Check, KeyRound, Plus, Search, X } from 'lucide-react';
import { Button, EmptyState, Panel, StatusPill } from '@/modules/shared/components/signal-primitives';
import { formatCompact } from '@/lib/format';

// ---- fixtures ---------------------------------------------------------------

type Provider = {
  id: string;
  vendor: 'openai' | 'google' | 'custom';
  name: string;
  base_url: string;
  has_key: boolean;
  error?: string;
};

type Model = { id: string; context_window: number };

const PROVIDERS: Provider[] = [
  { id: 'p1', vendor: 'openai', name: 'OpenAI', base_url: '', has_key: true },
  { id: 'p2', vendor: 'google', name: 'Google Gemini', base_url: '', has_key: true },
  { id: 'p3', vendor: 'custom', name: 'ai.lohi2.com', base_url: 'https://ai.lohi2.com/v1', has_key: true,
    error: 'tls: failed to verify certificate: x509: certificate signed by unknown authority' },
];

const MODELS: Record<string, Model[]> = {
  p1: [
    { id: 'gpt-5.2', context_window: 400_000 },
    { id: 'gpt-5.2-mini', context_window: 400_000 },
    { id: 'gpt-5.1', context_window: 400_000 },
    { id: 'gpt-5-mini', context_window: 400_000 },
    { id: 'gpt-4.1', context_window: 1_047_576 },
    { id: 'gpt-4.1-mini', context_window: 1_047_576 },
    { id: 'o4-mini', context_window: 200_000 },
  ],
  p2: [
    { id: 'gemini-3-pro', context_window: 1_048_576 },
    { id: 'gemini-3-flash', context_window: 1_048_576 },
    { id: 'gemini-2.5-pro', context_window: 1_048_576 },
    { id: 'gemini-2.5-flash', context_window: 1_048_576 },
    { id: 'gemini-2.5-flash-lite', context_window: 1_048_576 },
  ],
  p3: [],
};

const VENDOR_LABEL: Record<Provider['vendor'], string> = {
  openai: 'OpenAI',
  google: 'Google Gemini',
  custom: 'Custom endpoint',
};

const TIERS = [
  { key: 'flash', title: 'Default', does: 'The main analysis and answers.', required: true },
  { key: 'lite', title: 'Lite', does: 'Quick routing and history compression.', required: false },
  { key: 'pro', title: 'Pro', does: 'Learning and review. Costs the most.', required: false },
] as const;
type TierKey = (typeof TIERS)[number]['key'];

type Assignment = { providerId: string; model: string } | null; // null = inherit Default

type Fixture = 'configured' | 'empty' | 'error';

// ---- prototype --------------------------------------------------------------

export default function AIProviderPrototype() {
  const [fixture, setFixture] = useState<Fixture>('configured');
  return (
    <div className="min-h-dvh bg-[var(--color-background-body)] p-6">
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <span className="text-xs text-[var(--color-text-secondary)]">Prototype fixtures — not production chrome</span>
        {(['configured', 'empty', 'error'] as const).map((id) => (
          <Button key={id} size="sm" variant={fixture === id ? 'primary' : 'outline'} onClick={() => setFixture(id)}>
            {id}
          </Button>
        ))}
      </div>
      <div className="mx-auto max-w-[960px]">
        {fixture === 'configured' ? <Hub key="c" providers={PROVIDERS} /> : null}
        {fixture === 'empty' ? <Hub key="e" providers={[]} /> : null}
        {fixture === 'error' ? <Hub key="x" providers={PROVIDERS} /> : null}
      </div>
    </div>
  );
}

function Hub({ providers }: { providers: Provider[] }) {
  const [selectedProvider, setSelectedProvider] = useState<string>(providers[0]?.id ?? '');
  const [activeTier, setActiveTier] = useState<TierKey>('flash');
  const [query, setQuery] = useState('');
  const [assign, setAssign] = useState<Record<TierKey, Assignment>>(() =>
    providers.length
      ? { flash: { providerId: 'p1', model: 'gpt-5.2' }, lite: { providerId: 'p2', model: 'gemini-2.5-flash-lite' }, pro: null }
      : { flash: null, lite: null, pro: null },
  );

  const provider = providers.find((p) => p.id === selectedProvider) ?? providers[0];
  const models = useMemo(() => {
    const list = provider ? (MODELS[provider.id] ?? []) : [];
    const q = query.trim().toLowerCase();
    return q ? list.filter((m) => m.id.toLowerCase().includes(q)) : list;
  }, [provider, query]);

  const assignModel = (modelId: string) => {
    if (!provider) return;
    setAssign((a) => ({ ...a, [activeTier]: { providerId: provider.id, model: modelId } }));
  };

  const modelLabel = (tier: TierKey, a: Assignment) => {
    if (!a) return tier === 'flash' ? 'Not set — pick a model' : 'Same as Default';
    const p = providers.find((x) => x.id === a.providerId);
    return `${a.model} · ${p?.name ?? '?'}`;
  };

  return (
    <div className="flex flex-col gap-4">
      <p className="max-w-[640px] text-sm text-[var(--color-text-primary)]">
        Your agents think with these keys. Pick a provider on the left, then click a model to assign it to the selected
        tier.
      </p>

      {/* Roles view — the three tiers as the assignment target */}
      <Panel title="Which model does what">
        <div className="flex flex-wrap gap-2">
          {TIERS.map((t) => {
            const a = assign[t.key];
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
                <span className={`mt-1 font-mono text-xs ${a ? 'text-[var(--color-text-primary)]' : 'text-[var(--color-faint)]'}`}>
                  {modelLabel(t.key, a)}
                </span>
              </button>
            );
          })}
        </div>
      </Panel>

      {/* Hub: provider rail + model browser */}
      <div className="grid grid-cols-1 gap-4 md:grid-cols-[280px_minmax(0,1fr)]">
        {/* Scope sidebar — one row per provider */}
        <Panel
          title="Providers"
          action={
            providers.length ? (
              <Button variant="ghost" size="sm" icon={<Plus size={14} />} onClick={() => {}}>
                Add
              </Button>
            ) : undefined
          }
        >
          {!providers.length ? (
            <EmptyState
              icon={<KeyRound size={20} />}
              title="No provider yet"
              detail="Paste an API key from OpenAI, Google Gemini, or a custom OpenAI-compatible endpoint."
              action={
                <Button variant="primary" size="sm" icon={<Plus size={15} />} onClick={() => {}}>
                  Add provider
                </Button>
              }
            />
          ) : (
            <div className="flex flex-col gap-1">
              {providers.map((p) => {
                const active = provider?.id === p.id;
                const count = MODELS[p.id]?.length ?? 0;
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
                      <span className="truncate text-sm text-[var(--color-text-primary)]">{p.name}</span>
                      {p.error ? (
                        <StatusPill grow={false} status="attention" label="Key rejected" />
                      ) : p.has_key ? (
                        <StatusPill grow={false} status="healthy" label="Connected" />
                      ) : (
                        <StatusPill grow={false} status="paused" label="Needs a key" />
                      )}
                    </span>
                    <span className="text-xs text-[var(--color-text-secondary)]">
                      {VENDOR_LABEL[p.vendor]}
                      {p.base_url ? ` · ${p.base_url.replace(/^https?:\/\//, '')}` : ''}
                      {count ? ` · ${count} models` : ''}
                    </span>
                    {p.error ? (
                      <span role="alert" className="mt-1 flex items-start gap-1 text-xs text-danger">
                        <AlertTriangle size={12} className="mt-0.5 flex-none" />
                        <span className="line-clamp-2">{p.error}</span>
                      </span>
                    ) : null}
                  </button>
                );
              })}
            </div>
          )}
        </Panel>

        {/* Model browser — scoped to the selected provider */}
        <Panel
          title={provider ? `${provider.name} models` : 'Models'}
          action={
            provider ? (
              <div className="flex gap-1">
                <Button variant="ghost" size="sm" onClick={() => {}}>Replace key</Button>
                <Button variant="ghost" size="sm" onClick={() => {}}>Remove</Button>
              </div>
            ) : undefined
          }
        >
          {!provider ? (
            <EmptyState title="Add a provider first" detail="Once a key is saved, its models show up here." />
          ) : provider.error ? (
            <EmptyState
              icon={<AlertTriangle size={20} />}
              title="Can't reach this provider"
              detail={provider.error}
              action={<Button variant="outline" size="sm" onClick={() => {}}>Replace key</Button>}
            />
          ) : (
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3">
                <Search size={14} className="flex-none text-[var(--color-text-secondary)]" />
                <input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder={`Search ${provider.name} models…`}
                  aria-label="Search models"
                  className="h-10 w-full bg-transparent text-sm text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-faint)]"
                />
                {query ? (
                  <button onClick={() => setQuery('')} aria-label="Clear search" className="text-[var(--color-text-secondary)]">
                    <X size={14} />
                  </button>
                ) : null}
              </div>
              <div className="flex flex-col">
                {models.length ? (
                  models.map((m) => {
                    const assigned = TIERS.filter((t) => assign[t.key]?.model === m.id && assign[t.key]?.providerId === provider.id);
                    return (
                      <button
                        key={m.id}
                        onClick={() => assignModel(m.id)}
                        className="flex min-h-[40px] w-full items-center justify-between gap-3 rounded-[var(--radius-md)] px-3 py-1.5 text-left transition-colors hover:bg-[var(--color-surface-2)]"
                      >
                        <span className="flex items-center gap-2">
                          <span className="font-mono text-sm text-[var(--color-text-primary)]">{m.id}</span>
                          {assigned.map((t) => (
                            <span key={t.key} className="rounded-[var(--radius-sm)] bg-[var(--color-surface-3)] px-1.5 py-0.5 text-xs text-[var(--color-primary)]">
                              {t.title}
                            </span>
                          ))}
                        </span>
                        <span className="text-xs text-[var(--color-text-secondary)]">
                          {m.context_window ? `${formatCompact(m.context_window)} ctx` : 'ctx unknown'}
                        </span>
                      </button>
                    );
                  })
                ) : (
                  <EmptyState title="No model matches" detail="Try a different search, or check the provider's model list." />
                )}
              </div>
              <p className="text-xs text-[var(--color-text-secondary)]">
                Click a model to assign it to <strong>{TIERS.find((t) => t.key === activeTier)?.title}</strong>.
              </p>
            </div>
          )}
        </Panel>
      </div>

      {/* Footer actions */}
      {providers.length ? (
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="primary" size="sm" onClick={() => {}}>
            <Check size={14} className="mr-1 inline" /> Save changes
          </Button>
          <Button variant="outline" size="sm" onClick={() => {}}>
            Check it works
          </Button>
          <span className="text-xs text-[var(--color-text-secondary)]">
            Checking sends one real message per model — it can take up to a minute.
          </span>
        </div>
      ) : null}
    </div>
  );
}
