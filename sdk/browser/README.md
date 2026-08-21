# @agentray/browser

The browser SDK for [AgentRay](https://agentray.lohi2.com). One `init()` wires up
identity (anonymous ↔ identified), batched delivery, retries, and `sendBeacon`
flush on page unload.

## Install

```bash
npm install @agentray/browser
```

No bundler? The same bundle loads from a `<script>` tag and exposes
`window.AgentRay`:

```html
<script src="https://unpkg.com/@agentray/browser/dist/index.global.js"></script>
<script>
  AgentRay.init({ host: 'https://agentray.example.com', apiKey: 'phc_your_project_key', autocapture: true });
</script>
```

## Quick start

```ts
import { init } from '@agentray/browser';

const ar = init({
  host: 'https://agentray.example.com',
  apiKey: 'phc_your_project_key',
  autocapture: true, // delegated click + pageview capture
});

// Manual events
ar.capture('checkout_started', { plan: 'pro' });

// On login — links the prior anonymous session to the user
ar.identify('user-123', { email: 'alice@example.com' });

// On logout
ar.reset();
```

## API

| Method | Purpose |
| --- | --- |
| `init(opts)` | Create the client. `opts`: `host`, `apiKey`, optional `autocapture`, `batching`, `platform`. |
| `capture(event, props?)` | Queue an event (flushed in batches). |
| `identify(userId, traits?)` | Switch to an identified user; aliases the anonymous history. |
| `alias(anon, canonical)` | Manually link two IDs (advanced). |
| `reset()` | Start a fresh anonymous session. |
| `flush()` | Force-send buffered events now. |
| `autocapture(opts?)` | Turn on delegated capture; returns an uninstall fn. |

## Delivery semantics

Events are buffered and sent to `POST /batch` when the buffer reaches
`batchSize` (default 20) or after `flushIntervalMs` (default 3000). Transient
5xx/network failures retry with exponential backoff up to `maxRetries` (default
3); 4xx responses are not retried. On `visibilitychange→hidden` and `pagehide`
the buffer is flushed via `navigator.sendBeacon` so the tail of a session is not
lost when the tab closes.

## Build & test

```bash
npm run typecheck
npm test           # vitest, jsdom
npm run build      # tsup → dist/ (ESM + CJS + <script> global + d.ts)
```

## Which app an event came from

Every event carries `platform: "web"`. That property is what lets Traffic's
platform split, the per-platform funnel, and the filter bar tell your website's
audience from your app's, in a product that has both — instead of reporting one
blended number that describes neither.

It is stated rather than left to a user-agent guess on the server: the guess is
right for a plain tab and wrong the moment the bundle runs somewhere else — an
Electron renderer, a prerender bot, a jsdom test — where it lands in "unknown".

Set it once when this bundle genuinely is not the website. A Capacitor or Cordova
build shipped inside the iOS app should say so, or its users are counted as web
traffic:

```ts
init({ host, apiKey, platform: 'ios' });
```

Per-call properties cannot override it — the platform is applied after your own
keys, so a stray `platform` in a `capture()` call can't mislabel a surface.
