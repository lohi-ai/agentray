'use client';

import { useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Check, Copy, KeyRound, Plug, RefreshCw } from 'lucide-react';
import { apiBase } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { useCurrentProject, useEventNames } from '@/modules/app/hooks';
import { Button, Segment } from '@/modules/shared/components/signal-primitives';

type Lang = 'curl' | 'js' | 'python';

const LANGS: Array<{ value: Lang; label: string }> = [
  { value: 'curl', label: 'cURL' },
  { value: 'js', label: 'JavaScript' },
  { value: 'python', label: 'Python' },
];

// snippet builds a copy-paste integration that POSTs one event to the ingest
// endpoint. The shape mirrors the /capture contract exactly:
//   { api_key, event, distinct_id, properties }
function snippet(lang: Lang, base: string, key: string): string {
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

// FirstEventQuickstart is the activation surface: a brand-new project has an API
// key but no obvious path to its first data point, so the dashboard would just
// look empty forever. This card turns that dead end into the analytics "aha" —
// copy a working snippet, send one event, watch the dashboard come alive. It is
// self-gating: it renders nothing once the project has ever emitted an event
// (the event-name catalog spans all history, not the active range).
export function FirstEventQuickstart() {
  const { names, loading } = useEventNames();
  const { project } = useCurrentProject();
  const projectID = useAuthStore((s) => s.project?.id);
  const queryClient = useQueryClient();

  const [lang, setLang] = useState<Lang>('curl');
  const [copied, setCopied] = useState<'key' | 'code' | null>(null);

  const key = project?.api_key ?? '';
  const base = apiBase();
  const code = useMemo(() => snippet(lang, base, key), [lang, base, key]);

  if (loading || names.length > 0 || !project) return null;

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
          <div className="mb-0.5 text-[11px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">Get started · ~2 min</div>
          <div className="text-sm font-semibold">Send your first event</div>
          <div className="text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">No data yet. Drop one of these snippets into your app and your dashboard, agents, and people views all light up.</div>
        </div>
        <span className="ms-auto hidden items-center gap-1.5 self-center rounded-[20px] bg-[var(--color-background-surface)] px-[9px] py-[3px] text-[11.5px] text-agent [@media(min-width:520px)]:inline-flex">
          <span className="relative inline-block h-2 w-2 flex-none rounded-full bg-agent after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-['']" />
          Waiting for events
        </span>
      </div>

      <div className="flex flex-col gap-3.5 p-4">
        {/* Step 1 — API key */}
        <div>
          <div className="mb-1.5 flex items-center gap-1.5 text-[12.5px] font-medium"><KeyRound size={14} className="text-[var(--color-text-secondary)]" /> Step 1 · Your project API key</div>
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

        {/* Step 2 — snippet */}
        <div>
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <span className="text-[12.5px] font-medium">Step 2 · Send an event</span>
            <span className="ms-auto"><Segment options={LANGS} value={lang} onChange={(v) => setLang(v as Lang)} /></span>
          </div>
          <div className="relative">
            <pre className="m-0 overflow-x-auto rounded-md bg-[var(--color-background-muted)] p-3.5 font-mono text-[12px] leading-[1.55] text-[var(--color-text-primary)]"><code>{code}</code></pre>
            <button
              className="absolute end-2.5 top-2.5 inline-flex items-center gap-1 rounded-sm border border-[var(--color-border)] bg-[var(--color-background-card)] px-2 py-1 text-[11.5px] text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-surface)] hover:text-[var(--color-text-primary)]"
              onClick={() => copy(code, 'code')}
            >
              {copied === 'code' ? <><Check size={13} /> Copied</> : <><Copy size={13} /> Copy</>}
            </button>
          </div>
          <p className="mt-1.5 text-[11.5px] text-[var(--color-text-secondary)]">Swap <code className="font-mono">event</code>, <code className="font-mono">distinct_id</code>, and <code className="font-mono">properties</code> for your own. Events appear within a few seconds.</p>
        </div>

        <div className="flex items-center gap-2">
          <Button variant="primary" size="sm" icon={<RefreshCw size={14} />} onClick={checkNow}>I&apos;ve sent it — check now</Button>
          <span className="text-[11.5px] text-[var(--color-text-disabled)]">This card disappears once your first event lands.</span>
        </div>
      </div>
    </div>
  );
}
