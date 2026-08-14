import { describe, expect, it } from 'vitest';
import {
  decodeTierValue,
  encodeTierValue,
  listedModelsToPickerOptions,
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
