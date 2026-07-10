/**
 * AgentRay browser client — manages anonymous ↔ identified identity lifecycle.
 *
 * Usage:
 *   const ar = new AgentRayClient({ apiUrl: "https://agentray.example.com", apiKey: "..." });
 *   ar.capture("user.pageview", { path: "/home" });
 *   // On login:
 *   ar.identify("user-123", { email: "alice@example.com" });
 *   // On logout:
 *   ar.reset();
 *
 * Identity flow:
 *   1. On construction a UUID anonymous ID is created and persisted to localStorage.
 *   2. All `capture()` calls use this anonymous ID until `identify()` is called.
 *   3. `identify()` calls POST /alias to link the prior anonymous session to the
 *      identified user, then switches subsequent events to the user ID.
 *   4. `reset()` generates a fresh anonymous ID (call on logout).
 */

const ANON_ID_KEY = 'agentray_anon_id';

function getOrCreateAnonId(): string {
  let id: string | null = null;
  try {
    id = localStorage.getItem(ANON_ID_KEY);
  } catch {}
  if (!id) {
    id = generateId();
    try {
      localStorage.setItem(ANON_ID_KEY, id);
    } catch {}
  }
  return id;
}

function generateId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return Math.random().toString(36).slice(2) + Date.now().toString(36);
}

export interface AgentRayConfig {
  /** Base URL of the AgentRay server (e.g. https://agentray.example.com). */
  apiUrl: string;
  /** Project API key. */
  apiKey: string;
}

export class AgentRayClient {
  private readonly apiUrl: string;
  private readonly apiKey: string;
  private distinctId: string;
  private anonId: string | null;

  constructor(config: AgentRayConfig) {
    this.apiUrl = config.apiUrl.replace(/\/$/, '');
    this.apiKey = config.apiKey;
    const anon = getOrCreateAnonId();
    this.anonId = anon;
    this.distinctId = anon;
  }

  /**
   * Send a single event. Uses the current distinct ID (anonymous or identified).
   */
  capture(event: string, properties: Record<string, unknown> = {}): void {
    this.post('/capture', {
      api_key: this.apiKey,
      event,
      distinct_id: this.distinctId,
      properties,
      timestamp: new Date().toISOString(),
    });
  }

  /**
   * Identify the current user. If a prior anonymous session existed, links it
   * to the user ID via POST /alias so history is not lost.
   */
  identify(userId: string, traits: Record<string, unknown> = {}): void {
    if (this.anonId !== null && this.anonId !== userId) {
      this.post('/alias', {
        api_key: this.apiKey,
        anonymous_id: this.anonId,
        distinct_id: userId,
      });
      try {
        localStorage.removeItem(ANON_ID_KEY);
      } catch {}
    }
    this.anonId = null;
    this.distinctId = userId;
    this.post('/identify', {
      api_key: this.apiKey,
      distinct_id: userId,
      $set: traits,
      timestamp: new Date().toISOString(),
    });
  }

  /**
   * Manually link an anonymous ID to a canonical user ID.
   * Prefer `identify()` — this is for advanced cases where you manage IDs yourself.
   */
  alias(anonymousId: string, canonicalId: string): void {
    this.post('/alias', {
      api_key: this.apiKey,
      anonymous_id: anonymousId,
      distinct_id: canonicalId,
    });
  }

  /**
   * Reset to a new anonymous session. Call on logout so the next visitor on
   * this device gets a fresh ID and doesn't inherit the logged-out user's history.
   */
  reset(): void {
    const newAnon = generateId();
    try {
      localStorage.setItem(ANON_ID_KEY, newAnon);
    } catch {}
    this.anonId = newAnon;
    this.distinctId = newAnon;
  }

  /** Returns the current distinct ID (anonymous UUID or identified user ID). */
  getDistinctId(): string {
    return this.distinctId;
  }

  private post(path: string, body: unknown): void {
    fetch(`${this.apiUrl}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).catch(() => {});
  }
}
