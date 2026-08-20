# AgentRay SDK

Three SDK modules ship with AgentRay: two browser modules for client-side
behaviour, and one server module for events the browser must not be trusted to
send (payments, subscription changes, refunds).

Every SDK call needs a project API key. Fastest path (no web app required):

```bash
agentray signup --email you@example.com   # or `agentray login` on an existing account
export AGENTRAY_API_KEY=$(agentray key)
```

(Build the CLI with `make cli`; see the CLI section of the README.)

---

## Server client (`@agentray/server`, `sdk/server/`)

The sanctioned path for **revenue truth**. The browser cannot be trusted to
report money, so payment/subscription/refund events come from your backend.

```bash
npm install @agentray/server     # or: bun add @agentray/server
# vendored alternative: copy sdk/server/ into your repo and import ./client
```

```ts
import { AgentRayServerClient } from '@agentray/server';

const ar = new AgentRayServerClient({
  apiUrl: 'https://agentray.example.com',
  apiKey: process.env.AGENTRAY_API_KEY!, // server-side only
});

// In a payment webhook handler:
await ar.revenue('user-123', { amount: 19, currency: 'USD', plan: 'pro', kind: 'subscription' }, {
  idempotencyKey: webhook.id, // provider event id — see "Idempotency" below
});
```

Differences from the browser client: identity is explicit (`distinctId` on every
call), payments must be retryable, and every event carries an idempotency key.

### Idempotency (`$insert_id`)

Revenue webhooks retry, so the same payment can arrive several times. Pass the
provider's event id as `idempotencyKey`; it is sent as `$insert_id` and stored on
the event's `insert_id` column. De-duplicate at read time, e.g.:

```sql
-- one row per payment even if the webhook fired twice
SELECT sum(amount) AS revenue FROM (
  SELECT argMax(JSONExtractFloat(properties, 'amount'), timestamp) AS amount
  FROM events WHERE event_name = 'revenue' GROUP BY insert_id
)
```


## Browser client (`@agentray/browser`, `sdk/browser/`)

Manages anonymous → identified identity and sends events from the browser.
`npm install @agentray/browser` when the package is published; until then copy
`sdk/browser/` into the product repo, or paste the no-npm HTML snippet from
`web/modules/start/components/instrument-snippet.tsx`.

```ts
import { init } from '@agentray/browser';

const ar = init({
  host: 'https://agentray.example.com',
  apiKey: 'your-project-api-key',
  autocapture: true,
});
```

### Track events

```ts
ar.capture('user.pageview', { path: '/pricing' });
ar.capture('button.click',  { label: 'Start free trial' });
```

### Identify on login

Call `identify()` when the user logs in. It automatically links the prior anonymous session to the user ID so history is not lost.

```ts
ar.identify('user-123', { email: 'alice@example.com', name: 'Alice' });
// All subsequent capture() calls use 'user-123'.
```

### Reset on logout

```ts
ar.reset(); // Generates a fresh anonymous ID for the next visitor.
```

### Manual alias (advanced)

Use `alias()` when you manage IDs yourself and want to link them explicitly.

```ts
ar.alias('anon-uuid-from-cookie', 'user-123');
```

---

## Autocapture (`sdk/browser/autocapture.ts`)

Zero-config pageview, click, and element-view tracking. Pass your own `capture` function — works with the client above or any compatible sink.
Autocapture reports `user.pageview` (the event the web-analytics tab and the
seeded Product funnel read). A hand-rolled `pageview` will not light those surfaces.

```ts
import { installAutocapture } from '@/sdk/browser/autocapture';

const uninstall = installAutocapture(
  (event, props) => ar.capture(event, props),
  { pageviews: true, clicks: true, elementViews: true },
);

// Opt out a subtree:  <div data-track-ignore>...</div>
// Force-track an element: <button data-track="upgrade-cta">Upgrade</button>
// Track visibility: <section data-track-view="hero-section">...</section>

// Cleanup (SPA unmount):
uninstall();
```

---

## API reference

All SDKs talk to the same HTTP endpoints. You can also call them directly.

| Endpoint | Payload | Purpose |
|---|---|---|
| `POST /capture` | `{ api_key, event, distinct_id, properties?, session_id? }` | Single event |
| `POST /batch` | `{ api_key, batch: [...] }` | Multiple events |
| `POST /identify` | `{ api_key, distinct_id, $set?: {} }` | Set user traits |
| `POST /alias` | `{ api_key, anonymous_id, distinct_id }` | Link anonymous → identified |

All endpoints return `{ "status": 1 }` on success.

### Identity stitching flow

```
anonymous-uuid  ──captures──▶ events (distinct_id = "anonymous-uuid")
                                        │
                user logs in            │
                                        ▼
POST /alias { anonymous_id: "anonymous-uuid", distinct_id: "user-123" }
                                        │
                                        ▼
GET /api/persons  →  shows "user-123" with combined event history
```
