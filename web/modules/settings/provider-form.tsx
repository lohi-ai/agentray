'use client';

import { useState } from 'react';
import { Selector } from '@astryxdesign/core/Selector';
import { Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import type { WorkspaceProvider, WorkspaceProviderInput } from '@/lib/api';
import { Button } from '@/modules/shared/components/signal-primitives';

// Vendor kinds the user can configure — not a model catalog. Model IDs come
// from each active provider's list-models API. Only the escape hatch is
// renamed: `openai-compat` still goes over the wire, but a non-technical owner
// reads it as the advanced choice it is, not a fourth equal vendor.
const VENDOR_KINDS = [
  { value: 'openai', label: 'OpenAI' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'google', label: 'Google Gemini' },
  { value: 'openai-compat', label: 'Something else (advanced)' },
] as const;

export function vendorLabel(vendor: string): string {
  return VENDOR_KINDS.find((v) => v.value === vendor)?.label ?? vendor;
}

// Anything that is not one of the three first-party vendors is reached through
// an OpenAI-compatible endpoint, and that endpoint has to be typed in.
function vendorNeedsBaseURL(vendor: string): boolean {
  return vendor !== 'openai' && vendor !== 'anthropic' && vendor !== 'google';
}

type ProviderDraft = { vendor: string; name: string; base_url: string; api_key: string };

const emptyDraft = (): ProviderDraft => ({ vendor: 'openai', name: '', base_url: '', api_key: '' });

const draftFrom = (p: WorkspaceProvider | null): ProviderDraft =>
  p ? { vendor: p.vendor, name: p.name, base_url: p.base_url, api_key: '' } : emptyDraft();

export function providerFormTitle(provider: WorkspaceProvider | null): string {
  return provider ? `Replace key — ${provider.name || vendorLabel(provider.vendor)}` : 'Add AI provider';
}

// One form for both "Add provider" and "Replace key" — the only difference is
// whether a key is required (adding) or optional (editing: blank keeps the
// stored one, which is what the API already means by an empty api_key).
//
// It lives inside a StackSheet panel, which owns the title and the close
// affordance, so this renders body + footer only. The panel content is a
// ReactNode captured at push time, so the form owns its own submitting state
// rather than reading a prop that would never update.
export function ProviderForm({
  provider,
  onSubmit,
  onCancel,
}: {
  provider: WorkspaceProvider | null;
  onSubmit: (input: WorkspaceProviderInput) => Promise<void>;
  onCancel: () => void;
}) {
  const editing = !!provider;
  const [draft, setDraft] = useState<ProviderDraft>(() => draftFrom(provider));
  const [touched, setTouched] = useState(false);
  const [saving, setSaving] = useState(false);
  const patch = (field: keyof ProviderDraft, value: string) => setDraft((d) => ({ ...d, [field]: value }));

  const advanced = vendorNeedsBaseURL(draft.vendor);
  // Hidden for the three first-party vendors (criterion 4 — the advanced path
  // must not read as an equal-weight first choice), but still shown for Google,
  // which the previous UI let you point at a regional endpoint, and for any
  // provider that already has one saved.
  const showBaseURL = advanced || draft.vendor === 'google' || !!draft.base_url;
  const keyMissing = !editing && !draft.api_key.trim();
  const baseURLMissing = advanced && !draft.base_url.trim();
  const invalid = keyMissing || baseURLMissing;

  const submit = async () => {
    setTouched(true);
    if (invalid || saving) return;
    setSaving(true);
    try {
      await onSubmit({
        vendor: draft.vendor,
        name: draft.name.trim(),
        // Send the address only while the field is on screen. Switching from the
        // advanced vendor back to OpenAI hides it but keeps the typed value in
        // the draft, and the backend honors a base_url whatever the vendor is —
        // so without this gate an abandoned gateway URL silently becomes the
        // endpoint the new OpenAI key talks to.
        base_url: showBaseURL ? draft.base_url.trim() : '',
        api_key: draft.api_key.trim(),
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-col gap-3.5 px-[18px] py-4">
      <Selector
        label="Who is the provider?"
        size="sm"
        options={VENDOR_KINDS.map((v) => ({ value: v.value, label: v.label }))}
        value={draft.vendor}
        onChange={(v) => patch('vendor', v)}
        width="100%"
      />
      <TextInput
        label={editing ? 'New API key' : 'API key'}
        type="password"
        isRequired={!editing}
        isOptional={editing}
        status={touched && keyMissing ? { type: 'error', message: 'Paste the key from your provider.' } : undefined}
        value={draft.api_key}
        placeholder={editing ? 'Leave blank to keep the current key' : 'Paste the key from your provider'}
        onChange={(v) => patch('api_key', v)}
        width="100%"
      />
      <TextInput
        label="Name"
        isOptional
        value={draft.name}
        placeholder="e.g. Main key"
        onChange={(v) => patch('name', v)}
        width="100%"
      />
      {showBaseURL ? (
        <TextInput
          label="Server address"
          isRequired={advanced}
          isOptional={!advanced}
          status={
            touched && baseURLMissing
              ? { type: 'error', message: 'This provider needs the address of its server.' }
              : undefined
          }
          value={draft.base_url}
          placeholder="https://api.example.com/v1"
          onChange={(v) => patch('base_url', v)}
          width="100%"
        />
      ) : null}
      <Text type="supporting">
        {advanced
          ? 'Use this for a self-hosted or gateway endpoint that speaks the OpenAI API. Ask whoever runs it for the server address.'
          : 'Your key is encrypted and never shown again. You can replace it any time.'}
      </Text>
      <div className="mt-1 flex gap-2">
        <Button variant="primary" size="sm" onClick={() => void submit()} disabled={saving}>
          {saving ? 'Saving…' : editing ? 'Save changes' : 'Add provider'}
        </Button>
        <Button variant="ghost" size="sm" onClick={onCancel} disabled={saving}>
          Cancel
        </Button>
      </div>
    </div>
  );
}
