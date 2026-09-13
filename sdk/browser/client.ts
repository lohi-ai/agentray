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

import { DEFAULT_PLATFORM, withPlatform } from './platform';
import { IdentityQueue, type AliasOperation } from './transport';

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
  /**
   * Value stamped on every event's `platform` property (default `"web"`).
   * Override only when this bundle is not the website — e.g. a Capacitor build
   * running inside the iOS app, which should report `"ios"`.
   */
  platform?: string;
  /**
   * Delivery lane for `identify`/`alias`. `init()` passes the transport's lane
   * so identity work shares one queue with the session; a standalone client
   * gets its own.
   */
  identity?: IdentityQueue;
}

export class AgentRayClient {
  private readonly apiUrl: string;
  private readonly apiKey: string;
  private readonly platform: string;
  private readonly identity: IdentityQueue;
  private distinctId: string;
  private anonId: string | null;

  constructor(config: AgentRayConfig) {
    this.apiUrl = config.apiUrl.replace(/\/$/, '');
    this.apiKey = config.apiKey;
    this.platform = config.platform ?? DEFAULT_PLATFORM;
    const anon = getOrCreateAnonId();
    this.anonId = anon;
    this.distinctId = anon;
    this.identity =
      config.identity ?? new IdentityQueue({ host: this.apiUrl, apiKey: this.apiKey });
    this.identity.onAliasConfirmed = (op) => this.forgetAliasedAnonymousId(op);
    // An alias that never got a 2xx is replayed before any new identity work.
    this.identity.restorePendingAlias();
  }

  /**
   * Send a single event. Uses the current distinct ID (anonymous or identified).
   */
  capture(event: string, properties: Record<string, unknown> = {}): void {
    this.post('/capture', {
      api_key: this.apiKey,
      event,
      distinct_id: this.distinctId,
      properties: withPlatform(properties, this.platform),
      timestamp: new Date().toISOString(),
    });
  }

  /**
   * Identify the current user. If a prior anonymous session existed, links it
   * to the user ID via POST /alias so history is not lost.
   *
   * Both requests go through the identity queue: a transient failure retries,
   * and anything still unconfirmed rides the unload beacon instead of being
   * lost with the tab.
   */
  identify(userId: string, traits: Record<string, unknown> = {}): void {
    if (this.anonId !== null && this.anonId !== userId) {
      this.identity.enqueue({
        kind: 'alias',
        anonymousId: this.anonId,
        distinctId: userId,
      });
    }
    this.anonId = null;
    this.distinctId = userId;
    this.identity.enqueue({
      kind: 'identify',
      distinctId: userId,
      traits,
      timestamp: new Date().toISOString(),
    });
  }

  /**
   * Manually link an anonymous ID to a canonical user ID.
   * Prefer `identify()` — this is for advanced cases where you manage IDs yourself.
   */
  alias(anonymousId: string, canonicalId: string): void {
    this.identity.enqueue({ kind: 'alias', anonymousId, distinctId: canonicalId });
  }

  /**
   * Reset to a new anonymous session. Call on logout so the next visitor on
   * this device gets a fresh ID and doesn't inherit the logged-out user's history.
   *
   * Purely local: it neither sends anything nor cancels identity work already
   * issued, because dropping a pending alias here would unlink the history of
   * the user who just left.
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

  /**
   * Drop the local anonymous id once the server has confirmed the alias. The
   * compare-and-delete matters: `reset()` may already have minted the next
   * visitor's id, and that one is not ours to erase.
   */
  private forgetAliasedAnonymousId(op: AliasOperation): void {
    let current: string | null = null;
    try {
      current = localStorage.getItem(ANON_ID_KEY);
    } catch {}
    if (current === null || current !== op.anonymousId) return;
    try {
      localStorage.removeItem(ANON_ID_KEY);
    } catch {}
  }

  private post(path: string, body: unknown): void {
    fetch(`${this.apiUrl}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).catch(() => {});
  }
}
