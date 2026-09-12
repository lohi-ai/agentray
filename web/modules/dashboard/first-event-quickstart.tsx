'use client';

import { useMemo, useState } from 'react';
import { useRouter } from 'next/navigation';
import { useQueryClient } from '@tanstack/react-query';
import { Apple, Check, Copy, Globe, KeyRound, Plug, RefreshCw, Smartphone, Warehouse } from 'lucide-react';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { AgentRayAPI, apiBase, type VerifySDKResult } from '@/lib/api';
import { settingsPath, shouldShowFirstEventGuide } from '@/lib/ia';
import { useAuthStore } from '@/lib/app-state';
import { useCurrentProject, useEventNames } from '@/modules/app/hooks';
import { Button, Callout, Segment } from '@/modules/shared/components/signal-primitives';
import { swiftSnippet } from '@/modules/start/components/instrument-snippet';

type Source = 'website' | 'ios' | 'app' | 'warehouse';
type Lang = 'curl' | 'js' | 'python';

// iOS is its own source, not a language under "App / API". A native app is a
// different audience arriving through the same key — it needs a device id that
// survives launches, an alias on login so its users are not counted twice
// against the website's, and a platform tag — none of which a cURL example
// teaches. Leaving it out is what made "I have a web app and an iOS app" a
// hand-rolled integration.
const SOURCES: Array<{ value: Source; label: string }> = [
  { value: 'website', label: 'Website' },
  { value: 'ios', label: 'iOS app' },
  { value: 'app', label: 'App / API' },
  { value: 'warehouse', label: 'Warehouse' },
];

const LANGS: Array<{ value: Lang; label: string }> = [
  { value: 'curl', label: 'cURL' },
  { value: 'js', label: 'JavaScript' },
  { value: 'python', label: 'Python' },
];

function appSnippet(lang: Lang, base: string, key: string): string {
  const url = `${base}/capture`;
  if (lang === 'curl') {
    return [
      `curl -X POST ${url} \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -d '{`,
      `    "api_key": "${key}",`,
      `    "event": "onboarding_verified",`,
      `    "distinct_id": "user_123",`,
      `    "properties": { "platform": "server" }`,
      `  }'`,
    ].join('\n');
  }
  if (lang === 'js') {
    return [
      `await fetch("${url}", {`,
      `  method: "POST",`,
      `  headers: { "Content-Type": "application/json" },`,
      `  body: JSON.stringify({`,
      `    api_key: "${key}",`,
      `    event: "onboarding_verified",`,
      `    distinct_id: "user_123",`,
      `    properties: { platform: "server" },`,
      `  }),`,
      `});`,
    ].join('\n');
  }
  return [
    `import requests`,
    ``,
    `requests.post("${url}", json={`,
    `    "api_key": "${key}",`,
    `    "event": "onboarding_verified",`,
    `    "distinct_id": "user_123",`,
    `    "properties": {"platform": "server"},`,
    `})`,
  ].join('\n');
}

// verificationSnippet deliberately differs from the full SDK examples: it
// sends only the excluded onboarding receipt, with an explicit platform, so a
// copied setup check cannot contaminate product metrics.
export function verificationSnippet(source: Exclude<Source, 'warehouse'>, lang: Lang, base: string, key: string): string {
  if (source === 'website') {
    return [
      `<script>`,
      `const key = ${JSON.stringify(key)};`,
      `const id = localStorage.getItem("agentray_verify_id") || crypto.randomUUID();`,
      `localStorage.setItem("agentray_verify_id", id);`,
      `fetch(${JSON.stringify(`${base}/capture`)}, {`,
      `  method: "POST",`,
      `  headers: { "Content-Type": "application/json" },`,
      `  body: JSON.stringify({`,
      `    api_key: key, event: "onboarding_verified", distinct_id: id,`,
      `    properties: { platform: "web" },`,
      `  }),`,
      `});`,
      `</script>`,
    ].join('\n');
  }
  if (source === 'ios') {
    return `${swiftSnippet(base, key)}\n\n// Verify setup once after initialization.\nAgentRay.capture("onboarding_verified")`;
  }
  return appSnippet(lang, base, key);
}

// FirstEventQuickstart is the activation surface for a project with nothing in
// it yet: the key, a snippet, and a way to check whether anything arrived. It
// used to also fire on a project literally named "Demo" — there is no such
// project now, and the shared demo is somebody else's, never a place to paste
// your own snippet.
export function FirstEventQuickstart() {
  const router = useRouter();
  const { names, loading } = useEventNames();
  const { project } = useCurrentProject();
  const projectID = useAuthStore((s) => s.project?.id);
  const queryClient = useQueryClient();

  const [source, setSource] = useState<Source>('website');
  const [lang, setLang] = useState<Lang>('js');
  const [copied, setCopied] = useState<'key' | 'task' | null>(null);
  const [verification, setVerification] = useState<VerifySDKResult | null>(null);
  const [verificationError, setVerificationError] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);

  const key = project?.api_key ?? '';
  const base = apiBase();
  const code = useMemo(
    () => verificationSnippet(source === 'warehouse' ? 'website' : source, lang, base, key),
    [source, lang, base, key],
  );
  const codeLang = source === 'website' ? 'html' : source === 'ios' ? 'swift' : lang === 'js' ? 'javascript' : lang === 'curl' ? 'bash' : 'python';

  if (!shouldShowFirstEventGuide({
    eventNames: names,
    catalogReady: !loading && !!project,
  })) return null;

  function copy(text: string, which: 'key' | 'task' = 'key') {
    void navigator.clipboard?.writeText(text);
    setCopied(which);
    setTimeout(() => setCopied(null), 1500);
  }

  // A self-contained brief for an external coding agent (Claude Code, Cursor,
  // a teammate's script). It points at the real SDK contract — init/autocapture,
  // identify, reset — instead of hand-rolled fetch calls, because identity
  // stitching lives inside the SDK and a reimplementation loses it.
  function agentTask(): string {
    return [
      'Instrument this product with AgentRay analytics using the official SDK — do not hand-roll HTTP calls.',
      '',
      `Host: ${base}`,
      `Project key: ${key || 'YOUR_PROJECT_KEY'}`,
      'NOTE: this is the project capture key. On older (pre-split) projects the same key may also read data — treat it as a secret in server code, embed it client-side only where the SDK docs say to.',
      '',
      'Web: install the browser SDK per sdk/browser/README.md — the npm scope',
      '  is not published yet, so use the GitHub release tarball or the',
      '  <script> bundle exactly as that README describes. Then:',
      '  import { init } from "@agentray/browser"',
      `  const ar = init({ host: "${base}", apiKey: "<key>", autocapture: true })`,
      '  autocapture covers pageviews (incl. SPA route changes) and clicks.',
      '  ar.capture("event_name", { ... }) for custom events.',
      '  ar.identify(userId, traits) on sign-in — it aliases the anonymous history,',
      '    so a person who browsed before signup stays one person.',
      '  ar.reset() on sign-out.',
      '  SDK source + full contract: sdk/browser/README.md in the AgentRay repo.',
      'iOS: sdk/swift/README.md — same identify-on-sign-in contract.',
      'Server/backend: sdk/server/README.md or sdk/python/README.md — different',
      '  contract: AgentRayServerClient, every capture takes an explicit',
      '  distinctId, no anonymous lifecycle and no reset(). Do not port the',
      '  browser identity flow to a server.',
      '',
      'Verification: send one event named "onboarding_verified" — the receipt',
      'check, excluded from all product metrics. Do NOT fake a user.pageview.',
      'Then tell me to press "I\'ve sent it — check now" on the AgentRay overview.',
    ].join('\n');
  }

  async function checkNow() {
    if (!projectID) return;
    setChecking(true);
    setVerificationError(null);
    try {
      const result = await new AgentRayAPI(projectID).verifySDK();
      setVerification(result);
      void Promise.all([
        queryClient.invalidateQueries({ queryKey: ['event-names', projectID] }),
        queryClient.invalidateQueries({ queryKey: ['console', projectID] }),
        queryClient.invalidateQueries({ queryKey: ['overview', projectID] }),
      ]);
    } catch (error) {
      setVerification(null);
      setVerificationError(error instanceof Error ? error.message : 'Could not check whether the verification event arrived.');
    } finally {
      setChecking(false);
    }
  }

  return (
    <div className="overflow-hidden rounded-xl bg-[var(--color-background-card)]">
      <div className="flex items-start gap-3 border-b border-[var(--color-border)] px-4 py-4">
        <span className="grid h-[34px] w-[34px] flex-none place-items-center rounded-[10px] bg-[color-mix(in_srgb,var(--primary)_16%,transparent)] text-primary"><Plug size={16} /></span>
        <div className="min-w-0">
          <div className="mb-0.5 text-2xs uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">
            Get started · ~2 min
          </div>
          <div className="text-sm font-semibold">Verify SDK capture</div>
          <div className="text-sm leading-[1.5] text-[var(--color-text-secondary)]">
            Send one verification event from your site, app, or backend. It confirms receipt without changing product metrics.
          </div>
        </div>
      </div>

      <div className="flex flex-col gap-4 p-4">
        <div>
          <div className="mb-2 flex items-center gap-2 text-sm font-medium"><KeyRound size={14} className="text-[var(--color-text-secondary)]" /> Your project API key</div>
          <div className="flex max-w-[560px] items-center gap-3 rounded-md bg-[var(--color-background-muted)] px-3 py-3 text-sm">
            <span className="min-w-0 flex-1 truncate font-mono tabular-nums">{key || '—'}</span>
            <button
              className="inline-flex flex-none items-center gap-1 rounded-sm border border-[var(--color-border)] bg-transparent px-2 py-1 text-xs text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-surface)] hover:text-[var(--color-text-primary)]"
              onClick={() => copy(key)}
              disabled={!key}
            >
              {copied === 'key' ? <><Check size={13} /> Copied</> : <><Copy size={13} /> Copy</>}
            </button>
          </div>
        </div>

        <div>
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">Source</span>
            <span className="ms-auto"><Segment options={SOURCES} value={source} onChange={(v) => setSource(v as Source)} /></span>
          </div>

          {source === 'warehouse' ? (
            <div className="rounded-md bg-[var(--color-background-muted)] px-4 py-3 text-sm leading-[1.55] text-[var(--color-text-secondary)]">
              <p className="mb-2 flex items-center gap-2 text-[var(--color-text-primary)]">
                <Warehouse size={14} /> Pull events from Postgres or an existing warehouse.
              </p>
              <Button variant="primary" size="sm" onClick={() => router.push(settingsPath('connectors'))}>Open data connectors</Button>
            </div>
          ) : (
            <>
              {source === 'app' ? (
                <div className="mb-2 flex items-center gap-2">
                  <Smartphone size={14} className="text-[var(--color-text-secondary)]" />
                  <span className="ms-auto"><Segment options={LANGS} value={lang} onChange={(v) => setLang(v as Lang)} /></span>
                </div>
              ) : source === 'ios' ? (
                <p className="mb-2 flex items-center gap-2 text-xs text-[var(--color-text-secondary)]">
                  <Apple size={14} /> The iOS snippet keeps its <code className="font-mono">platform: ios</code> stamp and adds one verification capture.
                </p>
              ) : (
                <p className="mb-2 flex items-center gap-2 text-xs text-[var(--color-text-secondary)]">
                  <Globe size={14} /> This one-time Web receipt is stamped <code className="font-mono">platform: web</code>.
                </p>
              )}
              <CodeBlock code={code} language={codeLang} size="sm" width="100%" container="section" />
            </>
          )}
        </div>

        {checking ? (
          <Callout tone="agentic" icon={<RefreshCw size={16} />} label="Verification" title="Checking for your event" detail="Looking at the most recent capture receipts for this project." />
        ) : verificationError ? (
          <Callout tone="warn" icon={<RefreshCw size={16} />} label="Verification" title="Could not check for your event" detail={verificationError} action={<Button variant="outline" size="sm" onClick={() => void checkNow()}>Retry</Button>} />
        ) : verification?.found ? (
          <Callout
            tone="growth"
            icon={<Check size={16} />}
            label="SDK verified"
            title={`${verification.event_name || 'Verification event'} received`}
            detail={`Received ${verification.received_at || 'at an unknown time'} · platform ${verification.platform || 'unknown'} · identity ${verification.identity_linked ? 'linked' : 'not linked'}.`}
          />
        ) : verification ? (
          <Callout tone="warn" icon={<RefreshCw size={16} />} label="Not received yet" title="No verification event found" detail={`Searched ${verification.searched} recent capture receipts. ${verification.warnings.join(' ')}`} action={<Button variant="outline" size="sm" onClick={() => void checkNow()}>Retry</Button>} />
        ) : null}

        {verification?.found && verification.warnings.length > 0 ? (
          <p role="status" className="text-xs text-[var(--color-text-secondary)]">{verification.warnings.join(' ')}</p>
        ) : null}

        <div className="flex items-center gap-2">
          <Button variant="primary" size="sm" icon={<RefreshCw size={14} />} onClick={() => void checkNow()} disabled={checking || !projectID}>
            {checking ? 'Checking…' : 'I’ve sent it — check now'}
          </Button>
          <Button variant="outline" size="sm" icon={copied === 'task' ? <Check size={14} /> : <Copy size={14} />} onClick={() => copy(agentTask(), 'task')}>
            {copied === 'task' ? 'Copied' : 'Copy agent task'}
          </Button>
          <span className="text-xs text-[var(--color-text-disabled)]">
            Receipts can take a few seconds. Verification stays visible until real product activity arrives.
          </span>
        </div>
      </div>
    </div>
  );
}
