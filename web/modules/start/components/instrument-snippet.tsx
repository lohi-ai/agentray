'use client';

import { useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { Button } from '@/modules/shared/components/signal-primitives';

// InstrumentSnippet is the on-ramp that was missing.
//
// The SDK's README says `npm install @agentray/browser`, which is the right
// answer for a product with a build step and useless to the person this job was
// added for: an owner whose whole prototype is a Framer page, a Carrd site, or
// one HTML file. They have no npm, and telling them to get one is telling them
// to come back next week.
//
// So this is deliberately dependency-free, inline, and readable — no CDN to
// trust, no bundle to load, nothing to install. It sends exactly the two events
// the validate job's threshold is set against, under exactly the names Product
// Scout's tracking plan names.
export function InstrumentSnippet({ apiKey, host }: { apiKey: string; host: string }) {
  const [tab, setTab] = useState<'track' | 'waitlist'>('track');
  const key = apiKey || 'YOUR_PROJECT_API_KEY';
  const code = tab === 'track' ? trackSnippet(host, key) : waitlistSnippet(host, key);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex gap-1">
        <TabButton active={tab === 'track'} onClick={() => setTab('track')}>
          1 · Track the page
        </TabButton>
        <TabButton active={tab === 'waitlist'} onClick={() => setTab('waitlist')}>
          2 · Collect emails
        </TabButton>
      </div>
      <p className="text-[12.5px] leading-[1.55] text-[var(--color-text-secondary)]">
        {tab === 'track' ? (
          <>
            Paste this before <code>&lt;/body&gt;</code> on your landing page. It sends{' '}
            <code>user.pageview</code> and a click event — no build step, no npm. Works on Framer, Carrd, Webflow, or a
            plain HTML file.
          </>
        ) : (
          <>
            Give any form on your page <code>id=&quot;waitlist&quot;</code> with an email input and a consent checkbox,
            then paste this. Each address is stored once, and a{' '}
            <code>waitlist.joined</code> event lands so the test can count it.
          </>
        )}
      </p>
      <CodeBlock code={code} />
      {tab === 'waitlist' ? (
        <p className="text-[12px] leading-[1.5] text-[var(--color-text-secondary)]">
          The consent checkbox is required — the request is refused without it. Addresses are yours: export or delete
          them any time, and every submitter gets an unsubscribe link back.
        </p>
      ) : null}
    </div>
  );
}

function TabButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={`min-h-9 rounded-[var(--radius-md)] border px-2.5 text-[12.5px] ${
        active
          ? 'border-[var(--agent)] text-[var(--color-text-primary)]'
          : 'border-[var(--color-border)] text-[var(--color-text-secondary)]'
      }`}
    >
      {children}
    </button>
  );
}

function CodeBlock({ code }: { code: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    } catch {
      // A denied clipboard permission is not an error worth a banner — the code
      // is on screen and selectable, which is the fallback either way.
    }
  };
  return (
    <div className="relative">
      <pre className="max-h-[320px] overflow-auto rounded-[var(--radius-md)] bg-[var(--color-background-muted)] p-3 text-[11.5px] leading-[1.6]">
        <code className="font-mono">{code}</code>
      </pre>
      <span className="absolute end-2 top-2">
        <Button variant="outline" size="sm" icon={copied ? <Check size={13} /> : <Copy size={13} />} onClick={copy}>
          {copied ? 'Copied' : 'Copy'}
        </Button>
      </span>
    </div>
  );
}

// The tracker. A visitor id in localStorage is what lets a pageview and a
// signup be recognised as one person — without it the conversion rate is two
// unrelated counts, which is the exact mistake this job exists to prevent.
function trackSnippet(host: string, key: string) {
  return `<script>
(function () {
  var HOST = ${JSON.stringify(host)};
  var KEY  = ${JSON.stringify(key)};

  // One stable id per visitor, so a pageview and a signup are the same person.
  var id = localStorage.getItem('ar_id');
  if (!id) { id = 'a-' + Math.random().toString(36).slice(2) + Date.now().toString(36); localStorage.setItem('ar_id', id); }
  window.agentrayId = id;

  function send(event, props) {
    var body = JSON.stringify({
      api_key: KEY, event: event, distinct_id: id,
      properties: Object.assign({ '$referrer': document.referrer, '$current_url': location.href }, props || {})
    });
    // keepalive so the last event still lands if the click navigates away.
    fetch(HOST + '/capture', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body, keepalive: true });
  }
  window.agentray = send;

  // The message angle under test, carried on every event: ?variant=b
  var variant = new URLSearchParams(location.search).get('variant') || '';
  send('user.pageview', { title: document.title, path: location.pathname, variant: variant });

  document.addEventListener('click', function (e) {
    var el = e.target.closest('a,button,[data-track]');
    if (!el) return;
    send('$autocapture', { text: (el.innerText || '').trim().slice(0, 80), variant: variant });
  }, true);
})();
</script>`;
}

// The waitlist. Posts to AgentRay directly — the owner needs no backend, which
// is the point: at this phase they do not have one.
function waitlistSnippet(host: string, key: string) {
  return `<!-- Your form, anywhere on the page -->
<form id="waitlist">
  <input type="email" name="email" placeholder="you@example.com" required />
  <label><input type="checkbox" name="consent" required /> Email me when this launches</label>
  <button type="submit">Join the waitlist</button>
</form>

<script>
document.getElementById('waitlist').addEventListener('submit', async function (e) {
  e.preventDefault();
  var form = e.target;
  var res = await fetch(${JSON.stringify(host)} + '/waitlist', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      api_key: ${JSON.stringify(key)},
      email: form.email.value,
      consent: form.consent.checked,
      consent_text: form.querySelector('label').innerText.trim(),
      source: new URLSearchParams(location.search).get('utm_source') || 'direct',
      // Sent by the tracking snippet above; ties this address to the same
      // person who read the page, so the conversion rate is real.
      distinct_id: window.agentrayId || '',
      properties: { '$referrer': document.referrer }
    })
  });
  if (res.ok) { form.innerHTML = '<p>You are on the list. Thank you!</p>'; }
  else { alert('Something went wrong — please try again.'); }
});
</script>`;
}
