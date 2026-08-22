'use client';

import { useMemo, useState } from 'react';
import { useRouter } from 'next/navigation';
import { useQueryClient } from '@tanstack/react-query';
import { Apple, Check, Copy, Globe, KeyRound, Plug, RefreshCw, Smartphone, Warehouse } from 'lucide-react';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { apiBase } from '@/lib/api';
import { settingsPath, shouldShowFirstEventGuide } from '@/lib/ia';
import { useAuthStore } from '@/lib/app-state';
import { useCurrentProject, useEventNames } from '@/modules/app/hooks';
import { Button, Segment } from '@/modules/shared/components/signal-primitives';
import { InstrumentSnippet, swiftSnippet } from '@/modules/start/components/instrument-snippet';

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
      `    "event": "user.signup",`,
      `    "distinct_id": "user_123",`,
      `    "properties": { "plan": "free" }`,
      `  }'`,
    ].join('\n');
  }
  if (lang === 'js') {
    return [
      `await fetch("${url}", {`,
      `  method: "POST",`,
      `  headers: { "Content-Type": "application/json" },`,
      `  body: JSON.stringify({`,
      `    event: "user.signup",`,
      `    distinct_id: "user_123",`,
      `    properties: { plan: "free" },`,
      `  }),`,
      `});`,
    ].join('\n');
  }
  return [
    `import requests`,
    ``,
    `requests.post("${url}", json={`,
    `    "event": "user.signup",`,
    `    "distinct_id": "user_123",`,
    `    "properties": {"plan": "free"},`,
    `})`,
  ].join('\n');
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
  const [copied, setCopied] = useState<'key' | null>(null);

  const key = project?.api_key ?? '';
  const base = apiBase();
  const code = useMemo(
    () => appSnippet(lang, base, key),
    [lang, base, key],
  );
  // The website snippet is a <script> tag, so it highlights as HTML; the app
  // snippets follow the picked language.
  const codeLang = source === 'website' ? 'html' : source === 'ios' ? 'swift' : lang === 'js' ? 'javascript' : lang === 'curl' ? 'bash' : 'python';

  if (!shouldShowFirstEventGuide({
    eventNames: names,
    catalogReady: !loading && !!project,
  })) return null;

  function copy(text: string) {
    void navigator.clipboard?.writeText(text);
    setCopied('key');
    setTimeout(() => setCopied(null), 1500);
  }

  function checkNow() {
    void queryClient.invalidateQueries({ queryKey: ['event-names', projectID] });
    void queryClient.invalidateQueries({ queryKey: ['console', projectID] });
  }

  return (
    <div className="overflow-hidden rounded-xl bg-[var(--color-background-card)]">
      <div className="flex items-start gap-3 border-b border-[var(--color-border)] px-4 py-4">
        <span className="grid h-[34px] w-[34px] flex-none place-items-center rounded-[10px] bg-[color-mix(in_srgb,var(--primary)_16%,transparent)] text-primary"><Plug size={16} /></span>
        <div className="min-w-0">
          <div className="mb-0.5 text-2xs uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">
            Get started · ~2 min
          </div>
          <div className="text-sm font-semibold">Send your first event</div>
          <div className="text-sm leading-[1.5] text-[var(--color-text-secondary)]">
            No data yet. Drop a snippet on your site, in your iOS app, in your backend, or open a warehouse connector.
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
                  <Apple size={14} /> Drop this in one Swift file. It tags every event <code className="font-mono">platform: ios</code>, so
                  your app and your site stay separable.
                </p>
              ) : (
                <p className="mb-2 flex items-center gap-2 text-xs text-[var(--color-text-secondary)]">
                  <Globe size={14} /> Paste this on every page. It sends <code className="font-mono">user.pageview</code>.
                </p>
              )}
              {source === 'website' ? (
                <InstrumentSnippet apiKey={key} host={base} />
              ) : (
                <CodeBlock code={source === 'ios' ? swiftSnippet(base, key) : code} language={codeLang} size="sm" width="100%" container="section" />
              )}
            </>
          )}
        </div>

        <div className="flex items-center gap-2">
          <Button variant="primary" size="sm" icon={<RefreshCw size={14} />} onClick={checkNow}>I&apos;ve sent it — check now</Button>
          <span className="text-xs text-[var(--color-text-disabled)]">
            Events can take a few seconds to appear. This card disappears once your first event lands.
          </span>
        </div>
      </div>
    </div>
  );
}
