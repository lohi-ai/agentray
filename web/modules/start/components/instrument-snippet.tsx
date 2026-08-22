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
  const [tab, setTab] = useState<'track' | 'ios' | 'waitlist'>('track');
  const key = apiKey || 'YOUR_PROJECT_API_KEY';
  const code = tab === 'track' ? trackSnippet(host, key) : tab === 'ios' ? swiftSnippet(host, key) : waitlistSnippet(host, key);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex gap-1">
        <TabButton active={tab === 'track'} onClick={() => setTab('track')}>
          1 · Track the page
        </TabButton>
        <TabButton active={tab === 'ios'} onClick={() => setTab('ios')}>
          2 · iOS app
        </TabButton>
        <TabButton active={tab === 'waitlist'} onClick={() => setTab('waitlist')}>
          3 · Collect emails
        </TabButton>
      </div>
      <p className="text-sm leading-[1.55] text-[var(--color-text-secondary)]">
        {tab === 'track' ? (
          <>
            Paste this before <code>&lt;/body&gt;</code> on your landing page. It sends{' '}
            <code>user.pageview</code> and a click event — no build step, no npm. Works on Framer, Carrd, Webflow, or a
            plain HTML file.
          </>
        ) : tab === 'ios' ? (
          <>
            One Swift file, no package. Screens land as <code>user.pageview</code> so the same charts read them, every
            event carries <code>platform: ios</code> so your app stays separable from your site, and{' '}
            <code>identify</code> links a person&apos;s app and web history instead of counting them twice.
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
      {tab === 'ios' ? (
        <p className="text-xs leading-[1.5] text-[var(--color-text-secondary)]">
          Prefer a package? The <code>AgentRay</code> Swift package is the same contract with batching, offline retry,
          and a flush when the app backgrounds — add it with Swift Package Manager instead of pasting this.
        </p>
      ) : null}
      {tab === 'waitlist' ? (
        <p className="text-xs leading-[1.5] text-[var(--color-text-secondary)]">
          The consent checkbox is required — the request is refused without it. Addresses are yours: export or delete
          them any time, and every submitter gets an unsubscribe link back.
        </p>
      ) : null}
      <PackagesNote />
    </div>
  );
}

// PackagesNote exists because the snippets above are the on-ramp, not the
// ceiling, and nothing in the product ever said so. The published SDKs — batching,
// retry, typed events, automatic `platform` — were discoverable only by reading
// the repo, so a team that already has a build step pasted the no-build snippet
// and never learned there was a better answer.
function PackagesNote() {
  return (
    <details className="rounded-[var(--radius-md)] border border-[var(--color-border)] px-3 py-2">
      <summary className="cursor-pointer text-sm text-[var(--color-text-secondary)]">
        Have a build step? Install the SDK instead
      </summary>
      <ul className="mt-2 flex flex-col gap-2 text-xs leading-[1.5] text-[var(--color-text-secondary)]">
        {PACKAGES.map((pkg) => (
          <li key={pkg.install} className="flex flex-col gap-1">
            <code className="break-all">{pkg.install}</code>
            <span>
              {pkg.blurb}
              {pkg.short ? <> — becomes <code>{pkg.short}</code> once it is on the registry</> : null}
            </span>
          </li>
        ))}
      </ul>
      <p className="mt-2 text-xs leading-[1.5] text-[var(--color-text-secondary)]">
        Each one batches, retries a failed flush, and stamps <code>platform</code> for you — so the split on Traffic and
        People is right without you sending the property by hand.
      </p>
    </details>
  );
}

// The SDKs are published to GitHub Releases first, so `install` is a command
// that works today with no registry account. `short` is the name the same
// package takes once its registry entry exists — it is shown as the preferred
// form, but it is not what this list tells someone to run, because a copied
// command that resolves to nothing is worse than a longer one that works.
// See docs/RELEASING-SDK.md.
const SDK_RELEASES = 'https://github.com/lohi-ai/agentray/releases/download';

const PACKAGES: Array<{ install: string; short?: string; blurb: string }> = [
  {
    install: `npm i ${SDK_RELEASES}/browser-v0.1.0/agentray-browser-0.1.0.tgz`,
    short: '@agentray/browser',
    blurb: 'websites and SPAs — pageviews, autocapture, identify',
  },
  {
    install: `npm i ${SDK_RELEASES}/server-v0.1.0/agentray-server-0.1.0.tgz`,
    short: '@agentray/server',
    blurb: 'Node, Deno, Bun — backend and webhook events',
  },
  {
    install: `pip install ${SDK_RELEASES}/python-v0.1.0/agentray-0.1.0-py3-none-any.whl`,
    short: 'agentray',
    blurb: 'Python services and jobs',
  },
  {
    install: 'https://github.com/lohi-ai/agentray-swift.git (SwiftPM)',
    blurb: 'iOS and macOS apps',
  },
];

function TabButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={`min-h-9 rounded-[var(--radius-md)] border px-3 text-sm ${
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
      <pre className="max-h-[320px] overflow-auto rounded-[var(--radius-md)] bg-[var(--color-background-muted)] p-3 text-xs leading-[1.6]">
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

// The iOS tracker. Exported because the dashboard's first-event card offers the
// same snippet under its own "iOS app" source — one Swift contract, not two.
//
// The three things a native app needs and a cURL example does not teach: an id
// that survives a relaunch (UserDefaults, not a fresh UUID per launch), an alias
// on login so the person who used the website and then the app is one person,
// and a platform tag so both audiences stay separable afterwards.
export function swiftSnippet(host: string, key: string) {
  return `import Foundation

// AgentRay — drop this file into your app. No package, no build step.
enum AgentRay {
    static let host = ${JSON.stringify(host)}
    static let apiKey = ${JSON.stringify(key)}

    // One stable id per install, so a screen view and a signup are the same
    // person. Survives relaunches; replaced by your user id at identify().
    private static let anonKey = "agentray_anon_id"
    private static let idKey = "agentray_distinct_id"

    private static var anonID: String {
        if let id = UserDefaults.standard.string(forKey: anonKey) { return id }
        let id = "a-" + UUID().uuidString.lowercased()
        UserDefaults.standard.set(id, forKey: anonKey)
        return id
    }

    private static var distinctID: String {
        UserDefaults.standard.string(forKey: idKey) ?? anonID
    }

    static func capture(_ event: String, _ properties: [String: Any] = [:]) {
        var props = properties
        props["platform"] = "ios"   // keeps your app separable from your website
        post("/capture", ["api_key": apiKey, "event": event,
                          "distinct_id": distinctID, "properties": props])
    }

    /// Call from .onAppear. Screens land as user.pageview — the event the
    /// Traffic and Product charts already read.
    static func screen(_ name: String) {
        capture("user.pageview", ["screen": name, "path": "/" + name])
    }

    /// Call on login. Links everything this install did anonymously to the user,
    /// so someone who used your site and then your app is one person, not two.
    static func identify(_ userID: String, traits: [String: Any] = [:]) {
        let previous = distinctID
        UserDefaults.standard.set(userID, forKey: idKey)
        if previous != userID {
            post("/alias", ["api_key": apiKey, "anonymous_id": previous, "distinct_id": userID])
        }
        post("/identify", ["api_key": apiKey, "distinct_id": userID, "$set": traits])
    }

    /// Call on logout, so the next person on this device starts fresh.
    static func reset() {
        UserDefaults.standard.removeObject(forKey: idKey)
        UserDefaults.standard.removeObject(forKey: anonKey)
    }

    private static func post(_ path: String, _ body: [String: Any]) {
        guard let url = URL(string: host + path),
              let data = try? JSONSerialization.data(withJSONObject: body) else { return }
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = data
        URLSession.shared.dataTask(with: request).resume()
    }
}

// Usage
//   .onAppear { AgentRay.screen("Home") }
//   AgentRay.capture("user.signup", ["plan": "free"])
//   AgentRay.identify("user_123", traits: ["email": "alice@example.com"])
`;
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
