'use client';

import { useMemo, useState } from 'react';
import { useRouter } from 'next/navigation';
import { useQueryClient } from '@tanstack/react-query';
import { Check, Copy, Globe, KeyRound, Plug, RefreshCw, Smartphone, Warehouse } from 'lucide-react';
import { apiBase } from '@/lib/api';
import { isSampleProject, settingsPath, shouldShowFirstEventGuide } from '@/lib/ia';
import { useAuthStore } from '@/lib/app-state';
import { useCurrentProject, useEventNames } from '@/modules/app/hooks';
import { Button, Segment } from '@/modules/shared/components/signal-primitives';

type Source = 'website' | 'app' | 'warehouse';
type Lang = 'curl' | 'js' | 'python';

const SOURCES: Array<{ value: Source; label: string }> = [
  { value: 'website', label: 'Website' },
  { value: 'app', label: 'App / API' },
  { value: 'warehouse', label: 'Warehouse' },
];

const LANGS: Array<{ value: Lang; label: string }> = [
  { value: 'curl', label: 'cURL' },
  { value: 'js', label: 'JavaScript' },
  { value: 'python', label: 'Python' },
];

function websiteSnippet(base: string, key: string): string {
  return [
    `<script>`,
    `(function () {`,
    `  var id = localStorage.getItem("ar_id") || (crypto.randomUUID && crypto.randomUUID());`,
    `  if (id) localStorage.setItem("ar_id", id);`,
    `  fetch("${base}/capture", {`,
    `    method: "POST",`,
    `    headers: { "Content-Type": "application/json" },`,
    `    body: JSON.stringify({`,
    `      api_key: "${key}",`,
    `      event: "pageview",`,
    `      distinct_id: id || "anon",`,
    `      properties: { path: location.pathname }`,
    `    })`,
    `  });`,
    `})();`,
    `</script>`,
  ].join('\n');
}

function appSnippet(lang: Lang, base: string, key: string): string {
  const url = `${base}/capture`;
  if (lang === 'curl') {
    return [
      `curl -X POST ${url} \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -d '{`,
      `    "api_key": "${key}",`,
      `    "event": "signup",`,
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
      `    api_key: "${key}",`,
      `    event: "signup",`,
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
    `    "api_key": "${key}",`,
    `    "event": "signup",`,
    `    "distinct_id": "user_123",`,
    `    "properties": {"plan": "free"},`,
    `})`,
  ].join('\n');
}

// FirstEventQuickstart is the activation surface: empty projects and the
// published Demo workspace both need a path to the caller's own data.
export function FirstEventQuickstart() {
  const router = useRouter();
  const { names, loading } = useEventNames();
  const { project } = useCurrentProject();
  const projectID = useAuthStore((s) => s.project?.id);
  const queryClient = useQueryClient();
  const sample = isSampleProject(project);

  const [source, setSource] = useState<Source>('website');
  const [lang, setLang] = useState<Lang>('js');
  const [copied, setCopied] = useState<'key' | 'code' | null>(null);

  const key = project?.api_key ?? '';
  const base = apiBase();
  const code = useMemo(
    () => (source === 'website' ? websiteSnippet(base, key) : appSnippet(lang, base, key)),
    [source, lang, base, key],
  );

  if (!shouldShowFirstEventGuide({
    eventNames: names,
    catalogReady: !loading && !!project,
    sample,
  })) return null;

  function copy(text: string, what: 'key' | 'code') {
    void navigator.clipboard?.writeText(text);
    setCopied(what);
    setTimeout(() => setCopied(null), 1500);
  }

  function checkNow() {
    void queryClient.invalidateQueries({ queryKey: ['event-names', projectID] });
    void queryClient.invalidateQueries({ queryKey: ['console', projectID] });
  }

  return (
    <div className="mb-4 overflow-hidden rounded-xl bg-[var(--color-background-card)]">
      <div className="flex items-start gap-[13px] border-b border-[var(--color-border)] px-4 py-3.5">
        <span className="grid h-[34px] w-[34px] flex-none place-items-center rounded-[10px] bg-[color-mix(in_srgb,var(--primary)_16%,transparent)] text-primary"><Plug size={16} /></span>
        <div className="min-w-0">
          <div className="mb-0.5 text-[11px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">
            {sample ? 'Connect your product · ~2 min' : 'Get started · ~2 min'}
          </div>
          <div className="text-sm font-semibold">
            {sample ? 'This funnel is a sample. Bring yours.' : 'Send your first event'}
          </div>
          <div className="text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">
            {sample
              ? 'You are looking at published demo data so you can see the product work. Connect a website, app, or warehouse to replace it with your drop.'
              : 'No data yet. Drop a snippet on your site, in your app, or open a warehouse connector.'}
          </div>
        </div>
      </div>

      <div className="flex flex-col gap-3.5 p-4">
        <div>
          <div className="mb-1.5 flex items-center gap-1.5 text-[12.5px] font-medium"><KeyRound size={14} className="text-[var(--color-text-secondary)]" /> Your project API key</div>
          <div className="flex max-w-[560px] items-center gap-[10px] rounded-md bg-[var(--color-background-muted)] px-3 py-[10px] text-[12.5px]">
            <span className="min-w-0 flex-1 truncate font-mono tabular-nums">{key || '—'}</span>
            <button
              className="inline-flex flex-none items-center gap-1 rounded-sm border border-[var(--color-border)] bg-transparent px-2 py-1 text-[11.5px] text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-surface)] hover:text-[var(--color-text-primary)]"
              onClick={() => copy(key, 'key')}
              disabled={!key}
            >
              {copied === 'key' ? <><Check size={13} /> Copied</> : <><Copy size={13} /> Copy</>}
            </button>
          </div>
        </div>

        <div>
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <span className="text-[12.5px] font-medium">Source</span>
            <span className="ms-auto"><Segment options={SOURCES} value={source} onChange={(v) => setSource(v as Source)} /></span>
          </div>

          {source === 'warehouse' ? (
            <div className="rounded-md bg-[var(--color-background-muted)] px-3.5 py-3 text-[12.5px] leading-[1.55] text-[var(--color-text-secondary)]">
              <p className="mb-2 flex items-center gap-1.5 text-[var(--color-text-primary)]">
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
              ) : (
                <p className="mb-2 flex items-center gap-1.5 text-[12px] text-[var(--color-text-secondary)]">
                  <Globe size={14} /> Paste this on every page. It sends <code className="font-mono">pageview</code>.
                </p>
              )}
              <div className="relative">
                <pre className="m-0 overflow-x-auto rounded-md bg-[var(--color-background-muted)] p-3.5 font-mono text-[12px] leading-[1.55] text-[var(--color-text-primary)]"><code>{code}</code></pre>
                <button
                  className="absolute end-2.5 top-2.5 inline-flex items-center gap-1 rounded-sm border border-[var(--color-border)] bg-[var(--color-background-card)] px-2 py-1 text-[11.5px] text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-surface)] hover:text-[var(--color-text-primary)]"
                  onClick={() => copy(code, 'code')}
                >
                  {copied === 'code' ? <><Check size={13} /> Copied</> : <><Copy size={13} /> Copy</>}
                </button>
              </div>
            </>
          )}
        </div>

        <div className="flex items-center gap-2">
          <Button variant="primary" size="sm" icon={<RefreshCw size={14} />} onClick={checkNow}>I&apos;ve sent it — check now</Button>
          <span className="text-[11.5px] text-[var(--color-text-disabled)]">
            {sample ? 'Your Production project stays empty until a real event lands.' : 'This card disappears once your first event lands.'}
          </span>
        </div>
      </div>
    </div>
  );
}
