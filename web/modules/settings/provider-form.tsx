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
  { value: 'openai-responses', label: 'OpenAI Responses (stored context)' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'google', label: 'Google Gemini' },
  { value: 'claude-code', label: 'Claude Code (subscription)' },
  { value: 'openai-codex', label: 'ChatGPT / Codex (subscription)' },
  { value: 'google-antigravity', label: 'Antigravity (subscription)' },
  { value: 'ollama', label: 'Ollama (local)' },
  { value: 'lm-studio', label: 'LM Studio (local)' },
  { value: 'llama.cpp', label: 'llama.cpp (local)' },
  { value: 'vllm', label: 'vLLM (self-hosted)' },
  { value: 'litellm', label: 'LiteLLM (self-hosted)' },
  { value: 'openai-compat', label: 'Something else (advanced)' },
] as const;

export function vendorLabel(vendor: string): string {
  return VENDOR_KINDS.find((v) => v.value === vendor)?.label ?? vendor;
}

// Subscription vendors authenticate with a pool of OAuth accounts, not a
// pasted key — the form only creates the row; accounts are added by signing in.
export function isOAuthVendor(vendor: string): boolean {
  return vendor === 'claude-code' || vendor === 'openai-codex' || vendor === 'google-antigravity';
}

// These OpenAI-compatible engines commonly run without authentication. A key
// remains optional so secured laptop/server deployments work too.
export function isAPIKeyOptionalVendor(vendor: string): boolean {
  return ['ollama', 'lm-studio', 'lmstudio', 'llama.cpp', 'llama-cpp', 'vllm', 'localai', 'local-ai', 'litellm'].includes(vendor);
}

// Anything that is not one of the three first-party vendors is reached through
// an OpenAI-compatible endpoint, and that endpoint has to be typed in. OAuth
// vendors have fixed endpoints, so they never ask for one either.
function vendorNeedsBaseURL(vendor: string): boolean {
  return !isOAuthVendor(vendor) && vendor !== 'openai' && vendor !== 'openai-responses' && vendor !== 'anthropic' && vendor !== 'google';
}

type ProviderDraft = { vendor: string; name: string; base_url: string; api_key: string };

const emptyDraft = (): ProviderDraft => ({ vendor: 'openai', name: '', base_url: '', api_key: '' });

const draftFrom = (p: WorkspaceProvider | null): ProviderDraft =>
  p ? { vendor: p.vendor, name: p.name, base_url: p.base_url, api_key: '' } : emptyDraft();

export function providerFormTitle(provider: WorkspaceProvider | null): string {
  if (!provider) return 'Add AI provider';
  return `${provider.auth_type === 'optional' ? 'Edit provider' : 'Replace key'} — ${provider.name || vendorLabel(provider.vendor)}`;
}

// One form for both adding and editing providers. Keys are required for new
// cloud providers, optional for local engines, and blank-on-edit preserves the
// stored one (the API's existing empty api_key behavior).
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

  const oauth = isOAuthVendor(draft.vendor);
  const keyOptional = isAPIKeyOptionalVendor(draft.vendor);
  const advanced = vendorNeedsBaseURL(draft.vendor);
  const showBaseURL = advanced || draft.vendor === 'google' || !!draft.base_url;
  // OAuth vendors don't use API keys — accounts are added via sign-in after
  // the provider row exists.
  const keyMissing = !oauth && !keyOptional && !editing && !draft.api_key.trim();
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
    <div className="flex flex-col gap-4 px-5 py-4">
      <Selector
        label="Who is the provider?"
        size="sm"
        options={VENDOR_KINDS.map((v) => ({ value: v.value, label: v.label }))}
        value={draft.vendor}
        onChange={(v) => patch('vendor', v)}
        width="100%"
      />
      {!oauth ? (
        <TextInput
          label={editing ? 'New API key' : 'API key'}
          type="password"
          isRequired={!editing && !keyOptional}
          isOptional={editing || keyOptional}
          status={touched && keyMissing ? { type: 'error', message: 'Paste the key from your provider.' } : undefined}
          value={draft.api_key}
          placeholder={editing ? 'Leave blank to keep the current key' : keyOptional ? 'Optional — only if your server requires one' : 'Paste the key from your provider'}
          onChange={(v) => patch('api_key', v)}
          width="100%"
        />
      ) : (
        <Text type="supporting">
          This vendor authenticates with your subscription account, not an API key. Once added, you will sign in to connect one or more accounts.
        </Text>
      )}
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
        {oauth
          ? 'You can sign in with multiple accounts to pool rate limits.'
          : draft.vendor === 'openai-responses'
            ? 'Uses OpenAI’s Responses API. Conversation chaining stores response state at OpenAI; AgentRay safely replays full context when local session state is unavailable.'
          : keyOptional
            ? 'This engine can run without a key. AgentRay uses the same OpenAI-compatible connection on a laptop or a server.'
          : advanced
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
