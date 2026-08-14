// Pure mapping from listed models of active providers → 3-tier picker options.
// Options are exactly the IDs the list-models API returned (labeled with the
// provider). This module must not invent model IDs.

export type ListedModel = {
  provider_id: string;
  provider_name: string;
  provider_vendor: string;
  id: string;
};

export type TierPickerOption = {
  value: string;
  label: string;
  providerId: string;
  modelId: string;
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
    }));
}
