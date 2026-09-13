/**
 * Batching transport for the AgentRay browser SDK.
 *
 * It coalesces captured events into a single POST /batch instead of one request
 * per event, retries transient failures with a bounded backoff, and — critically
 * for analytics accuracy — flushes the pending buffer with `navigator.sendBeacon`
 * on page hide/unload so the last events of a session are not lost when the tab
 * closes mid-flight.
 */

export interface BatchEvent {
  event: string;
  distinct_id: string;
  properties?: Record<string, unknown>;
  timestamp?: string;
}

export interface TransportOptions {
  /** Base URL of the AgentRay server (no trailing slash needed). */
  host: string;
  /** Project API key. */
  apiKey: string;
  /** Flush when this many events are buffered (default 20). */
  batchSize?: number;
  /** Flush at most this many ms after the first buffered event (default 3000). */
  flushIntervalMs?: number;
  /** Max delivery attempts per batch before dropping (default 3). */
  maxRetries?: number;
  /** Injected for tests; defaults to the global fetch. */
  fetchImpl?: typeof fetch;
}

/**
 * Bounded exponential backoff between delivery attempts, shared by both lanes
 * so an event batch and an alias wait the same way. Deliberately the promise
 * constructor rather than `Promise.withResolvers`: this bundle targets ES2018
 * and ships to browsers we do not get to choose.
 */
function backoffDelay(attempt: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, Math.min(1000 * 2 ** attempt, 8000)));
}

export class BatchTransport {
  private readonly host: string;
  private readonly apiKey: string;
  private readonly batchSize: number;
  private readonly flushIntervalMs: number;
  private readonly maxRetries: number;
  private readonly fetchImpl: typeof fetch;

  /**
   * The identity lane. `identify`/`alias` are not events, so they get their own
   * queue with the same retry and unload-beacon durability as a batch, while
   * staying on the endpoints the server actually stitches identity on.
   */
  readonly identity: IdentityQueue;

  private queue: BatchEvent[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor(opts: TransportOptions) {
    this.host = opts.host.replace(/\/$/, '');
    this.apiKey = opts.apiKey;
    this.batchSize = opts.batchSize ?? 20;
    this.flushIntervalMs = opts.flushIntervalMs ?? 3000;
    this.maxRetries = opts.maxRetries ?? 3;
    this.fetchImpl = opts.fetchImpl ?? ((...a) => fetch(...a));
    this.identity = new IdentityQueue({
      host: this.host,
      apiKey: this.apiKey,
      maxRetries: this.maxRetries,
      fetchImpl: this.fetchImpl,
    });
    this.installUnloadFlush();
  }

  /** Enqueue an event; flushes immediately when the buffer reaches batchSize. */
  enqueue(ev: BatchEvent): void {
    this.queue.push(ev);
    if (this.queue.length >= this.batchSize) {
      void this.flush();
      return;
    }
    if (this.timer === null) {
      this.timer = setTimeout(() => void this.flush(), this.flushIntervalMs);
    }
  }

  /** Send everything currently buffered. Safe to call when empty (no-op). */
  async flush(): Promise<void> {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.queue.length === 0) return;
    const batch = this.queue;
    this.queue = [];
    await this.deliver(batch, 0);
  }

  private async deliver(batch: BatchEvent[], attempt: number): Promise<void> {
    const body = JSON.stringify({ api_key: this.apiKey, batch });
    try {
      const res = await this.fetchImpl(`${this.host}/batch`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body,
        keepalive: true,
      });
      // 4xx (bad key, malformed) is not retryable — retrying can't fix it.
      if (res.ok || (res.status >= 400 && res.status < 500)) return;
      throw new Error(`status ${res.status}`);
    } catch (err) {
      if (attempt + 1 >= this.maxRetries) return; // give up; drop rather than loop
      await backoffDelay(attempt);
      await this.deliver(batch, attempt + 1);
    }
  }

  /**
   * On page hide/unload, flush the buffer synchronously via sendBeacon — a
   * regular fetch may be aborted as the document tears down, but a beacon is
   * queued by the browser and guaranteed best-effort delivery.
   */
  private installUnloadFlush(): void {
    if (typeof document === 'undefined' || typeof window === 'undefined') return;
    const beaconFlush = () => {
      if (this.queue.length === 0) return;
      const batch = this.queue;
      this.queue = [];
      const body = JSON.stringify({ api_key: this.apiKey, batch });
      if (typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
        navigator.sendBeacon(`${this.host}/batch`, new Blob([body], { type: 'application/json' }));
      }
    };
    // visibilitychange(hidden) is the reliable mobile signal; pagehide covers bfcache.
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'hidden') beaconFlush();
    });
    window.addEventListener('pagehide', beaconFlush);
  }
}

/**
 * Link an anonymous id to the account it belongs to.
 *
 * `/batch` cannot carry this. Batch items have no operation discriminator, the
 * batch handler never merges a top-level `$set` into event properties, and only
 * `POST /alias` writes the alias row that identity reads stitch through — so an
 * `$alias`/`$identify` batch item would be accepted with a 200 and stitch
 * nothing, silently orphaning the anonymous history it was meant to save.
 */
export interface AliasOperation {
  kind: 'alias';
  anonymousId: string;
  distinctId: string;
}

/** Set traits on the identified person. */
export interface IdentifyOperation {
  kind: 'identify';
  distinctId: string;
  traits: Record<string, unknown>;
  timestamp: string;
}

export type IdentityOperation = AliasOperation | IdentifyOperation;

export interface IdentityQueueOptions {
  /** Base URL of the AgentRay server (no trailing slash needed). */
  host: string;
  /** Project API key. */
  apiKey: string;
  /** Max delivery attempts per operation before it waits for the next flush (default 3). */
  maxRetries?: number;
  /** Injected for tests; defaults to the global fetch. */
  fetchImpl?: typeof fetch;
}

/** Where an unconfirmed alias waits out a page load. Holds ids only — never traits. */
const PENDING_ALIAS_KEY = 'agentray_pending_alias';

type DeliveryOutcome = 'ok' | 'terminal' | 'unsent';

interface PendingAlias {
  anonymousId: string;
  distinctId: string;
}

function readPendingAlias(): PendingAlias | null {
  let raw: string | null = null;
  try {
    raw = localStorage.getItem(PENDING_ALIAS_KEY);
  } catch {
    return null;
  }
  if (!raw) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== 'object' || parsed === null) return null;
    const { anonymousId, distinctId } = parsed as Partial<PendingAlias>;
    if (typeof anonymousId !== 'string' || typeof distinctId !== 'string') return null;
    return { anonymousId, distinctId };
  } catch {
    return null;
  }
}

function clearPendingAlias(anonymousId: string): void {
  const pending = readPendingAlias();
  // A newer identify owns the slot by now; leave its marker alone.
  if (pending === null || pending.anonymousId !== anonymousId) return;
  try {
    localStorage.removeItem(PENDING_ALIAS_KEY);
  } catch {}
}

/**
 * The identity lane of the transport: a FIFO queue that delivers `alias` and
 * `identify` operations to their own endpoints, retries transient failures, and
 * beacons whatever is still unconfirmed when the document goes away.
 *
 * It is deliberately not a generic "POST to any path" sender — the two
 * operations above are the whole surface, so a snippet that ships to someone
 * else's site cannot be pointed at another endpoint.
 */
export class IdentityQueue {
  private readonly host: string;
  private readonly apiKey: string;
  private readonly maxRetries: number;
  private readonly fetchImpl: typeof fetch;

  /**
   * The head stays queued while it is in flight, so an unload beacon still
   * carries it: `sendBeacon` returning true means the browser accepted the
   * request, not that the server answered it.
   */
  private queue: IdentityOperation[] = [];
  private pumping: Promise<void> | null = null;

  /**
   * Called once `/alias` is confirmed by a 2xx. Beacons and retries never call
   * it — acting on those would drop local identity state for a request nobody
   * has acknowledged.
   */
  onAliasConfirmed?: (op: AliasOperation) => void;

  constructor(opts: IdentityQueueOptions) {
    this.host = opts.host.replace(/\/$/, '');
    this.apiKey = opts.apiKey;
    this.maxRetries = opts.maxRetries ?? 3;
    this.fetchImpl = opts.fetchImpl ?? ((...a) => fetch(...a));
    this.installUnloadBeacon();
  }

  /** Queue an operation and start delivering it. */
  enqueue(op: IdentityOperation): void {
    if (op.kind === 'alias') {
      try {
        localStorage.setItem(
          PENDING_ALIAS_KEY,
          JSON.stringify({ anonymousId: op.anonymousId, distinctId: op.distinctId }),
        );
      } catch {}
    }
    this.queue.push(op);
    void this.pump();
  }

  /**
   * Re-queue an alias that was stored by an earlier page load and never
   * acknowledged. Safe to replay: the server's alias write is idempotent on
   * `(project_id, anonymous_id)`.
   */
  restorePendingAlias(): void {
    const pending = readPendingAlias();
    if (pending === null) return;
    if (this.queue.some((op) => op.kind === 'alias' && op.anonymousId === pending.anonymousId)) {
      return;
    }
    this.enqueue({
      kind: 'alias',
      anonymousId: pending.anonymousId,
      distinctId: pending.distinctId,
    });
  }

  /** Deliver what is queued; resolves when every operation has settled or stalled. */
  flush(): Promise<void> {
    return this.pump();
  }

  private pump(): Promise<void> {
    if (this.pumping !== null) return this.pumping;
    this.pumping = this.drain().finally(() => {
      this.pumping = null;
    });
    return this.pumping;
  }

  private async drain(): Promise<void> {
    while (this.queue.length > 0) {
      const op = this.queue[0];
      const outcome = await this.deliver(op, 0);
      // Still unacknowledged: keep it for the next flush or the unload beacon
      // rather than dropping the only record of how two ids are related.
      if (outcome === 'unsent') return;
      this.queue.shift();
      if (op.kind !== 'alias') continue;
      clearPendingAlias(op.anonymousId);
      if (outcome === 'ok') this.onAliasConfirmed?.(op);
    }
  }

  private async deliver(op: IdentityOperation, attempt: number): Promise<DeliveryOutcome> {
    const { path, body } = this.encode(op);
    try {
      const res = await this.fetchImpl(`${this.host}${path}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        keepalive: true,
      });
      // 4xx (bad key, malformed) is not retryable — retrying can't fix it.
      if (res.ok) return 'ok';
      if (res.status >= 400 && res.status < 500) return 'terminal';
      throw new Error(`status ${res.status}`);
    } catch {
      if (attempt + 1 >= this.maxRetries) return 'unsent';
      await backoffDelay(attempt);
      return this.deliver(op, attempt + 1);
    }
  }

  private encode(op: IdentityOperation): { path: string; body: Record<string, unknown> } {
    if (op.kind === 'alias') {
      return {
        path: '/alias',
        body: {
          api_key: this.apiKey,
          anonymous_id: op.anonymousId,
          distinct_id: op.distinctId,
        },
      };
    }
    return {
      path: '/identify',
      body: {
        api_key: this.apiKey,
        distinct_id: op.distinctId,
        $set: op.traits,
        timestamp: op.timestamp,
      },
    };
  }

  /**
   * On page hide/unload, beacon every unconfirmed operation to its own endpoint.
   * Nothing is cleared here: a beacon has no acknowledgement, so the page that
   * comes back from bfcache still has to deliver and confirm it.
   */
  private installUnloadBeacon(): void {
    if (typeof document === 'undefined' || typeof window === 'undefined') return;
    const beaconAll = () => {
      if (this.queue.length === 0) return;
      if (typeof navigator === 'undefined' || typeof navigator.sendBeacon !== 'function') return;
      for (const op of this.queue) {
        const { path, body } = this.encode(op);
        navigator.sendBeacon(
          `${this.host}${path}`,
          new Blob([JSON.stringify(body)], { type: 'application/json' }),
        );
      }
    };
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'hidden') beaconAll();
    });
    window.addEventListener('pagehide', beaconAll);
  }
}
