// Pure mapping from listed models of active providers → 3-tier picker options.
// Options are exactly the IDs the list-models API returned (labeled with the
// provider). This module must not invent model IDs.

export type ListedModel = {
  provider_id: string;
  provider_name: string;
  provider_vendor: string;
  id: string;
  /**
   * Input window in tokens, as resolved server-side: the provider's own figure
   * when it reported one, else the ai package's fallback, else 0 for unknown.
   * 0 is a real answer and must stay distinguishable from a small window.
   */
  context_window?: number;
};

export type TierPickerOption = {
  value: string;
  label: string;
  providerId: string;
  modelId: string;
  contextWindow: number;
};

export function encodeTierValue(providerId: string, modelId: string): string {
  return `${providerId}::${modelId}`;
}

export function decodeTierValue(value: string): { providerId: string; modelId: string } {
  const i = value.indexOf('::');
  if (i < 0) return { providerId: '', modelId: value };
  return { providerId: value.slice(0, i), modelId: value.slice(i + 2) };
}

export function listedModelsToPickerOptions(models: ListedModel[]): TierPickerOption[] {
  return models
    .filter((m) => m && m.id && m.provider_id)
    .map((m) => ({
      value: encodeTierValue(m.provider_id, m.id),
      label: `${m.id} · ${m.provider_name || m.provider_vendor || m.provider_id}`,
      providerId: m.provider_id,
      modelId: m.id,
      contextWindow: m.context_window || 0,
    }));
}

// --- Typeahead shape -------------------------------------------------------
// The tier picker is an Astryx <Typeahead>, so the same listed models are also
// exposed as SearchableItem: `id` is the encoded provider::model pair (two
// providers routinely list the same model id, so the model alone is not
// unique), `label` is the bare model id, and the provider name rides along in
// auxiliaryData for the second line of the row.

export type ModelPickerItem = {
  id: string;
  label: string;
  auxiliaryData: { provider: string; contextWindow: number };
};

export function listedModelsToItems(models: ListedModel[]): ModelPickerItem[] {
  return listedModelsToPickerOptions(models).map((o) => ({
    id: o.value,
    label: o.modelId,
    auxiliaryData: { provider: providerLabelFor(models, o.providerId), contextWindow: o.contextWindow },
  }));
}

// How much transcript a tier may accumulate before the agent compacts it. The
// operator's override wins when they set one; otherwise it is whatever the
// provider or the model catalog reported; 0 means nobody knows, and the run
// falls back to the workspace-wide ceiling.
export function effectiveTierWindow(detected: number, override: number): number {
  return override > 0 ? override : detected > 0 ? detected : 0;
}

// Token counts are read as magnitudes, not exact figures — "200K" is what an
// operator is checking for, and 200,000 makes them count digits.
export function formatTokens(n: number): string {
  if (n <= 0) return '';
  if (n >= 1_000_000) {
    // One decimal, and drop it when it rounds away: a 1,047,576-token window is
    // "1M", not "1.0M".
    return `${n / 1_000_000 >= 10 ? Math.round(n / 1_000_000) : Number((n / 1_000_000).toFixed(1))}M`;
  }
  if (n >= 1_000) return `${Math.round(n / 1_000)}K`;
  return String(n);
}

function providerLabelFor(models: ListedModel[], providerId: string): string {
  const m = models.find((x) => x.provider_id === providerId);
  return m?.provider_name || m?.provider_vendor || providerId;
}

// A tier can point at a model the provider no longer lists (key revoked, model
// retired, list call failing). Keep the saved choice visible instead of
// silently blanking the field.
export function savedModelItem(providerId: string, modelId: string, providerName: string): ModelPickerItem {
  return {
    id: encodeTierValue(providerId, modelId),
    label: modelId,
    // No window: the provider is not listing this model, so there is nothing to
    // detect. 0 reads as "not known", which is the honest state and is exactly
    // when an operator would want to set one by hand.
    auxiliaryData: { provider: providerName ? `${providerName} · saved` : 'saved', contextWindow: 0 },
  };
}

// Fold a haystack or a needle to a comparable key: accent-stripped, lowercased,
// separators dropped. Dropping separators is what makes `gpt4o` find `gpt-4o`
// and `claude35` find `claude-3-5-haiku` — model ids are punctuation-heavy and
// nobody types the hyphens in the right places.
function foldSearchKey(s: string): string {
  return s
    .normalize('NFD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '');
}

// Does every character of `needle` appear in `hay`, in order? A subsequence
// match is the tolerance that survives a dropped or transposed character, which
// a bare substring test does not.
function subsequenceOf(hay: string, needle: string): boolean {
  let i = 0;
  for (let j = 0; j < hay.length && i < needle.length; j += 1) {
    if (hay[j] === needle[i]) i += 1;
  }
  return i === needle.length;
}

// Synchronous in-memory filter — the whole list already arrived from
// /api/workspace/models/listed, so there is nothing to debounce and the set is
// bounded by the providers the workspace configured.
//
// The match is deliberately not a bare `.includes()`: the root CLAUDE.md
// requires user-facing text search to tolerate typos and be accent-insensitive.
// Contiguous matches rank ahead of subsequence ones so the exact thing you
// typed stays at the top.
export function searchModelItems(items: ModelPickerItem[], query: string, limit = 50): ModelPickerItem[] {
  const needle = foldSearchKey(query);
  if (!needle) return items.slice(0, limit);

  const exact: ModelPickerItem[] = [];
  const loose: ModelPickerItem[] = [];
  for (const i of items) {
    const hay = foldSearchKey(i.label) + foldSearchKey(i.auxiliaryData.provider);
    if (hay.includes(needle)) exact.push(i);
    else if (subsequenceOf(hay, needle)) loose.push(i);
    if (exact.length >= limit) break;
  }
  return exact.concat(loose).slice(0, limit);
}

// A failed list-models call arrives as `list models: status 401 {…raw vendor
// JSON…}`. Show the vendor's own sentence, not the envelope.
export function friendlyProviderError(raw: string): string {
  const text = (raw || '').trim();
  if (!text) return 'Could not reach this provider.';
  const start = text.indexOf('{');
  if (start < 0) return text;
  const end = text.lastIndexOf('}');
  if (end > start) {
    try {
      const body = JSON.parse(text.slice(start, end + 1)) as Record<string, unknown>;
      const message = extractMessage(body);
      if (message) return message;
    } catch {
      // Not JSON after all — fall through to the envelope.
    }
  }
  // Unparseable or message-less body: keep the envelope ("list models: status
  // 401") rather than dumping a raw blob on someone who just pasted a key.
  const prefix = text.slice(0, start).trim().replace(/[:\s]+$/, '');
  return prefix || text;
}

function extractMessage(body: Record<string, unknown>): string {
  if (typeof body.message === 'string' && body.message.trim()) return body.message.trim();
  const err = body.error;
  if (typeof err === 'string' && err.trim()) return err.trim();
  if (err && typeof err === 'object') {
    const nested = (err as Record<string, unknown>).message;
    if (typeof nested === 'string' && nested.trim()) return nested.trim();
  }
  return '';
}
