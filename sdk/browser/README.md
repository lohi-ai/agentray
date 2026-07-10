# @agentray/browser

The browser SDK for [AgentRay](https://agentray.lohi2.com). One `init()` wires up
identity (anonymous ↔ identified), batched delivery, retries, and `sendBeacon`
flush on page unload.

## Install

```bash
npm install @agentray/browser
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
| `init(opts)` | Create the client. `opts`: `host`, `apiKey`, optional `autocapture`, `batching`. |
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

## Build

```bash
npm run build      # tsup → dist/ (ESM + CJS + d.ts)
npm run typecheck
```
