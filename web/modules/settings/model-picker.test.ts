import { describe, expect, it } from 'vitest';
import {
  decodeTierValue,
  encodeTierValue,
  friendlyProviderError,
  listedModelsToItems,
  listedModelsToPickerOptions,
  savedModelItem,
  searchModelItems,
  type ListedModel,
} from './model-picker';

describe('listedModelsToPickerOptions', () => {
  it('maps listed models of two providers onto picker options and invents none', () => {
    const listed: ListedModel[] = [
      { provider_id: 'prov-a', provider_name: 'OpenAI work', provider_vendor: 'openai', id: 'stub-from-a' },
      { provider_id: 'prov-a', provider_name: 'OpenAI work', provider_vendor: 'openai', id: 'other-from-a' },
      { provider_id: 'prov-b', provider_name: 'Anthropic work', provider_vendor: 'anthropic', id: 'stub-from-b' },
    ];
    const options = listedModelsToPickerOptions(listed);
    const ids = options.map((o) => o.modelId);
    expect(ids).toEqual(['stub-from-a', 'other-from-a', 'stub-from-b']);
    expect(options.every((o) => listed.some((m) => m.id === o.modelId && m.provider_id === o.providerId))).toBe(true);
    expect(options.find((o) => o.modelId === 'stub-from-a')?.label).toContain('OpenAI work');
    expect(options.find((o) => o.modelId === 'stub-from-b')?.label).toContain('Anthropic work');
    // Must not invent IDs that were not listed.
    expect(ids).not.toContain('gpt-4o');
    expect(ids).not.toContain('claude-opus');
    expect(options).toHaveLength(listed.length);
  });

  it('drops incomplete rows rather than fabricating an id', () => {
    const options = listedModelsToPickerOptions([
      { provider_id: '', provider_name: 'X', provider_vendor: 'openai', id: 'orphan' },
      { provider_id: 'p', provider_name: 'X', provider_vendor: 'openai', id: '' },
      { provider_id: 'p', provider_name: 'X', provider_vendor: 'openai', id: 'kept' },
    ]);
    expect(options.map((o) => o.modelId)).toEqual(['kept']);
  });

  it('round-trips the encoded tier value', () => {
    const value = encodeTierValue('prov-a', 'stub-from-a');
    expect(decodeTierValue(value)).toEqual({ providerId: 'prov-a', modelId: 'stub-from-a' });
  });
});

describe('listedModelsToItems', () => {
  // Two providers listing the same model id is the normal case behind a
  // gateway — the Typeahead item id has to stay unique or selecting one picks
  // the other's provider.
  const listed: ListedModel[] = [
    { provider_id: 'prov-a', provider_name: 'Main key', provider_vendor: 'openai', id: 'shared-model' },
    { provider_id: 'prov-b', provider_name: 'Team gateway', provider_vendor: 'openai-compat', id: 'shared-model' },
  ];

  it('keys items by provider and model, labelling with the bare model id', () => {
    const items = listedModelsToItems(listed);
    expect(items.map((i) => i.id)).toEqual(['prov-a::shared-model', 'prov-b::shared-model']);
    expect(items.every((i) => i.label === 'shared-model')).toBe(true);
    expect(items.map((i) => i.auxiliaryData.provider)).toEqual(['Main key', 'Team gateway']);
  });

  it('decodes back to the provider that listed the model', () => {
    const items = listedModelsToItems(listed);
    expect(decodeTierValue(items[1].id)).toEqual({ providerId: 'prov-b', modelId: 'shared-model' });
  });

  it('falls back to the vendor when the provider has no name', () => {
    const items = listedModelsToItems([
      { provider_id: 'p', provider_name: '', provider_vendor: 'anthropic', id: 'm' },
    ]);
    expect(items[0].auxiliaryData.provider).toBe('anthropic');
  });
});

describe('searchModelItems', () => {
  const items = listedModelsToItems([
    { provider_id: 'p1', provider_name: 'Main key', provider_vendor: 'openai', id: 'gpt-4o' },
    { provider_id: 'p1', provider_name: 'Main key', provider_vendor: 'openai', id: 'o3-mini' },
    { provider_id: 'p2', provider_name: 'Team gateway', provider_vendor: 'openai-compat', id: 'flash' },
  ]);

  it('matches on the model id', () => {
    expect(searchModelItems(items, 'gpt').map((i) => i.label)).toEqual(['gpt-4o']);
  });

  it('matches on the provider name, so a user can narrow by key', () => {
    expect(searchModelItems(items, 'gateway').map((i) => i.label)).toEqual(['flash']);
  });

  it('tolerates the punctuation and typos nobody gets right in a model id', () => {
    // Model ids are hyphen-heavy; a user types what they remember.
    expect(searchModelItems(items, 'gpt4o').map((i) => i.label)).toEqual(['gpt-4o']);
    expect(searchModelItems(items, 'o3mini').map((i) => i.label)).toEqual(['o3-mini']);
    // A dropped character still finds it, and contiguous matches rank first.
    expect(searchModelItems(items, 'gpt4').map((i) => i.label)).toEqual(['gpt-4o']);
    expect(searchModelItems(items, 'gt4o')[0].label).toBe('gpt-4o');
  });

  it('is case-insensitive and returns everything for a blank query', () => {
    expect(searchModelItems(items, '  O3-MINI ').map((i) => i.label)).toEqual(['o3-mini']);
    expect(searchModelItems(items, '')).toHaveLength(3);
  });

  it('caps the result set so a 111-model workspace never floods the menu', () => {
    const many = listedModelsToItems(
      Array.from({ length: 111 }, (_, i) => ({
        provider_id: 'p',
        provider_name: 'Gateway',
        provider_vendor: 'openai-compat',
        id: `model-${i}`,
      })),
    );
    expect(searchModelItems(many, '')).toHaveLength(50);
    expect(searchModelItems(many, '', 8)).toHaveLength(8);
  });
});

describe('savedModelItem', () => {
  it('keeps a saved-but-unlisted selection visible instead of blanking the field', () => {
    const item = savedModelItem('prov-a', 'retired-model', 'Main key');
    expect(item.id).toBe(encodeTierValue('prov-a', 'retired-model'));
    expect(item.label).toBe('retired-model');
    expect(item.auxiliaryData.provider).toBe('Main key · saved');
  });

  it('still renders when the provider name is unknown', () => {
    expect(savedModelItem('prov-a', 'm', '').auxiliaryData.provider).toBe('saved');
  });
});

describe('friendlyProviderError', () => {
  it('lifts the vendor message out of the raw list-models envelope', () => {
    const raw =
      'list models: status 401 {"error":{"message":"Incorrect API key provided: sk-abc.","type":"invalid_request_error"}}';
    expect(friendlyProviderError(raw)).toBe('Incorrect API key provided: sk-abc.');
  });

  it('handles a flat message body and a string error field', () => {
    expect(friendlyProviderError('list models: status 500 {"message":"upstream down"}')).toBe('upstream down');
    expect(friendlyProviderError('list models: status 400 {"error":"bad request"}')).toBe('bad request');
  });

  it('falls back to the envelope when the body is not parseable JSON', () => {
    expect(friendlyProviderError('list models: status 502 {not json')).toBe('list models: status 502');
  });

  it('passes plain text through and never renders an empty alert', () => {
    expect(friendlyProviderError('dial tcp: connection refused')).toBe('dial tcp: connection refused');
    expect(friendlyProviderError('')).toBe('Could not reach this provider.');
  });
});
