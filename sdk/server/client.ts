/**
 * AgentRay server client — server-side event capture for events the browser
 * cannot or should not be trusted to send: payments, subscription changes,
 * refunds, webhook-driven state. This is the sanctioned path for *revenue*
 * truth, which is why it differs from the browser client in three ways:
 *
 *   1. Identity is explicit. The server already knows the user, so every call
 *      takes a `distinctId` — there is no anonymous-ID lifecycle or localStorage.
 *   2. Calls are awaitable and throw. A dropped pageview is fine; a dropped
 *      payment event is not. Callers can await, retry, and alert.
 *   3. Every event carries an idempotency key (`$insert_id`). Revenue webhooks
 *      (SePay, Stripe, …) retry, so the same payment can arrive several times;
 *      the key lets the store de-duplicate instead of double-counting MRR.
 *
 * Usage:
 *   const ar = new AgentRayServerClient({ apiUrl: "https://agentray.example.com", apiKey: "..." });
 *   await ar.identify("user-123", { email: "alice@example.com", plan: "pro" });
 *   await ar.revenue("user-123", { amount: 19, currency: "USD", plan: "pro" }, {
 *     // pass the provider's event id so a webhook retry is not counted twice:
 *     idempotencyKey: webhook.id,
 *   });
 */

export interface AgentRayServerConfig {
  /** Base URL of the AgentRay server (e.g. https://agentray.example.com). */
  apiUrl: string;
  /** Project API key. Keep this server-side only. */
  apiKey: string;
  /** Network timeout per request in ms (default 5000). */
  timeoutMs?: number;
}

export interface CaptureOptions {
  /**
   * Stable idempotency key for this event. Defaults to a random UUID. For
   * webhook-driven events pass the provider's event id so a retry of the same
   * delivery does not create a second event. Sent as `$insert_id` in properties.
   */
  idempotencyKey?: string;
  /** Override the event timestamp (ISO 8601). Defaults to now. */
  timestamp?: string;
  /** Optional session id to associate the event with. */
  sessionId?: string;
}

export interface RevenueEvent {
  /** Amount in the smallest natural unit you report on (e.g. dollars, not cents — be consistent). */
  amount: number;
  /** ISO 4217 currency code, e.g. "USD", "VND". */
  currency: string;
  /** Plan / product the payment is for, when applicable. */
  plan?: string;
  /** "subscription" | "one_time" | "renewal" | "refund" — your own taxonomy; refunds should be negative `amount`. */
  kind?: string;
  /** Any extra properties to attach (provider, invoice id, …). */
  [key: string]: unknown;
}

function generateId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return Math.random().toString(36).slice(2) + Date.now().toString(36);
}

export class AgentRayServerClient {
  private readonly apiUrl: string;
  private readonly apiKey: string;
  private readonly timeoutMs: number;

  constructor(config: AgentRayServerConfig) {
    this.apiUrl = config.apiUrl.replace(/\/$/, '');
    this.apiKey = config.apiKey;
    this.timeoutMs = config.timeoutMs ?? 5000;
  }

  /** Send a single server-side event. Resolves on success, throws on failure. */
  async capture(
    distinctId: string,
    event: string,
    properties: Record<string, unknown> = {},
    options: CaptureOptions = {},
  ): Promise<void> {
    await this.post('/capture', {
      api_key: this.apiKey,
      event,
      distinct_id: distinctId,
      session_id: options.sessionId,
      properties: { ...properties, $insert_id: options.idempotencyKey ?? generateId() },
      timestamp: options.timestamp ?? new Date().toISOString(),
    });
  }

  /**
   * Record a revenue event under the conventional `revenue` name so the analytics
   * surface and the Growth Lead can read MRR/LTV/conversion without bespoke
   * instrumentation. Always pass an `idempotencyKey` from your payment provider's
   * event id — webhook retries are the norm, and this is what keeps revenue from
   * being counted twice.
   */
  async revenue(distinctId: string, event: RevenueEvent, options: CaptureOptions = {}): Promise<void> {
    await this.capture(distinctId, 'revenue', { ...event }, options);
  }

  /** Set durable traits on a user (plan, signup source, …) without an event. */
  async identify(distinctId: string, traits: Record<string, unknown> = {}): Promise<void> {
    await this.post('/identify', {
      api_key: this.apiKey,
      distinct_id: distinctId,
      $set: traits,
      timestamp: new Date().toISOString(),
    });
  }

  /**
   * Link an anonymous browser id to the canonical user id from the server side —
   * useful when the browser SDK could not (e.g. login completed via a redirect).
   */
  async alias(anonymousId: string, canonicalId: string): Promise<void> {
    await this.post('/alias', {
      api_key: this.apiKey,
      anonymous_id: anonymousId,
      distinct_id: canonicalId,
    });
  }

  /** Send many events in one request. Each may carry its own idempotency key. */
  async batch(
    events: Array<{
      distinctId: string;
      event: string;
      properties?: Record<string, unknown>;
      options?: CaptureOptions;
    }>,
  ): Promise<void> {
    if (events.length === 0) return;
    await this.post('/batch', {
      api_key: this.apiKey,
      batch: events.map((e) => ({
        event: e.event,
        distinct_id: e.distinctId,
        session_id: e.options?.sessionId,
        properties: { ...(e.properties ?? {}), $insert_id: e.options?.idempotencyKey ?? generateId() },
        timestamp: e.options?.timestamp ?? new Date().toISOString(),
      })),
    });
  }

  private async post(path: string, body: unknown): Promise<void> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      const res = await fetch(`${this.apiUrl}${path}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        signal: controller.signal,
      });
      if (!res.ok) {
        throw new Error(`AgentRay ${path} failed: ${res.status} ${res.statusText}`);
      }
    } finally {
      clearTimeout(timer);
    }
  }
}
