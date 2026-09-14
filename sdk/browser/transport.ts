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

/**
 * A batch the transport gave up on. An event batch has no durable copy — unlike
 * an alias, which waits out a page load in `localStorage` — so once the budget
 * is spent these events are gone, and this record is the only trace that they
 * existed.
 */
export interface DroppedBatch {
  /** The events that were not delivered. */
  events: BatchEvent[];
  /** Delivery attempts made before giving up. */
  attempts: number;
  /** The last failure: an HTTP status, or the network error's message. */
  reason: string;
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
  /**
   * How long a batch keeps retrying a transient failure before it is abandoned
   * (default 60000).
   *
   * A deadline rather than an attempt count, because what the budget has to
   * outlast is a *window*, not a number of tries: a container restart is
   * process start + store open, and the backoff curve is capped at 8 s, so an
   * attempt count silently means a different amount of time on every failure
   * pattern. The default covers a restart with margin — the deploy's own
   * healthcheck expects the API to answer within its 30 s `start_period`, and
   * a blue/green roll keeps the old colour serving, so a reader's outage is a
   * restart, not a deploy. Raise it if a longer window is worth holding a
   * batch in memory for; the cost of a longer budget is that `flush()` a caller
   * awaits is bounded by it, and that a page which navigates mid-outage loses
   * the batch anyway (see `onBatchDropped`).
   */
  retryBudgetMs?: number;
  /**
   * Called when a batch is abandoned, after the budget is spent or on a 4xx the
   * server will never accept. Wire it to the host's error reporting: without it
   * a dropped batch is still warned about on the console and announced as an
   * `agentray:batch_dropped` window event, but nothing durable records it.
   */
  onBatchDropped?: (drop: DroppedBatch) => void;
  /** Injected for tests; defaults to the global fetch. */
  fetchImpl?: typeof fetch;
}

/**
 * Bounded exponential backoff between delivery attempts, shared by both lanes
 * so an event batch and an alias wait the same way. `capMs` lets the batch lane
 * stop a wait at its deadline rather than sleeping past it. Deliberately the
 * promise constructor rather than `Promise.withResolvers`: this bundle targets
 * ES2018 and ships to browsers we do not get to choose.
 */
function backoffDelay(attempt: number, capMs = 8000): Promise<void> {
  return new Promise((resolve) =>
    setTimeout(resolve, Math.min(1000 * 2 ** attempt, 8000, capMs)),
  );
}

export class BatchTransport {
  private readonly host: string;
  private readonly apiKey: string;
  private readonly batchSize: number;
  private readonly flushIntervalMs: number;
  private readonly retryBudgetMs: number;
  private readonly onBatchDropped?: (drop: DroppedBatch) => void;
  private readonly fetchImpl: typeof fetch;

  /**
   * The identity lane. `identify`/`alias` are not events, so they get their own
   * queue with the same retry and unload-beacon durability as a batch, while
   * staying on the endpoints the server actually stitches identity on.
   */
  readonly identity: IdentityQueue;

  private queue: BatchEvent[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;
  /**
   * The tail of the delivery chain. A batch that is still retrying must not be
   * joined by a second loop for the events captured meanwhile: with a budget
   * measured in tens of seconds, one loop per flush interval would multiply the
   * requests an outage is already refusing and land a burst of batches the
   * moment the server answers again.
   */
  private delivering: Promise<void> | null = null;

  constructor(opts: TransportOptions) {
    this.host = opts.host.replace(/\/$/, '');
    this.apiKey = opts.apiKey;
    this.batchSize = opts.batchSize ?? 20;
    this.flushIntervalMs = opts.flushIntervalMs ?? 3000;
    this.retryBudgetMs = opts.retryBudgetMs ?? 60_000;
    this.onBatchDropped = opts.onBatchDropped;
    this.fetchImpl = opts.fetchImpl ?? ((...a) => fetch(...a));
    this.identity = new IdentityQueue({
      host: this.host,
      apiKey: this.apiKey,
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
      this.timer = setTimeout(() => {
        this.timer = null;
        void this.flush();
      }, this.flushIntervalMs);
    }
  }

  /**
   * Send everything currently buffered, behind any batch already being retried.
   * Safe to call when empty (no-op), and resolves when the delivery it started
   * has settled — delivered, or abandoned and reported.
   */
  async flush(): Promise<void> {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.queue.length === 0) return this.delivering ?? undefined;
    const batch = this.queue;
    this.queue = [];
    const run = (this.delivering ?? Promise.resolve()).then(() => this.deliver(batch));
    this.delivering = run;
    try {
      await run;
    } finally {
      if (this.delivering === run) this.delivering = null;
    }
  }

  /**
   * Deliver one batch, retrying a transient failure until the budget is spent.
   * A 4xx is terminal — resending a bad key cannot fix it — but it is still a
   * batch the product will never see, so it is reported like any other drop.
   */
  private async deliver(batch: BatchEvent[]): Promise<void> {
    const body = JSON.stringify({ api_key: this.apiKey, batch });
    const deadline = Date.now() + this.retryBudgetMs;
    let attempts = 0;
    let reason = 'unknown';
    for (;;) {
      attempts++;
      try {
        const res = await this.fetchImpl(`${this.host}/batch`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body,
          keepalive: true,
        });
        if (res.ok) return;
        reason = `status ${res.status}`;
        if (res.status >= 400 && res.status < 500) break;
      } catch (err) {
        reason = err instanceof Error ? err.message : String(err);
      }
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      await backoffDelay(attempts - 1, remaining);
    }
    this.reportDropped({ events: batch, attempts, reason });
  }

  /**
   * Say so when a batch is abandoned. A dropped batch is data the reader
   * generated and the product will never see, so it must not be silent: the
   * callback is for the host's own error reporting, the warning is for whoever
   * has the console open, and the event is for anything already listening.
   */
  private reportDropped(drop: DroppedBatch): void {
    // Nothing here may throw. `deliver` is the tail of the flush chain, so an
    // exception escaping it would reject `flush()` — and an analytics SDK must
    // never be the reason the host page breaks. The callback is isolated from
    // the rest so a bad one cannot also hide the console signal.
    try {
      this.onBatchDropped?.(drop);
    } catch {}
    try {
      if (typeof console !== 'undefined' && typeof console.warn === 'function') {
        console.warn(
          `[agentray] dropped ${drop.events.length} event(s) after ${drop.attempts} attempt(s): ${drop.reason}`,
        );
      }
      if (typeof window !== 'undefined' && typeof CustomEvent === 'function') {
        window.dispatchEvent(new CustomEvent('agentray:batch_dropped', { detail: drop }));
      }
    } catch {}
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

/** An operation with its wire form already fixed — see `IdentityQueue.encode`. */
interface QueuedOperation {
  op: IdentityOperation;
  path: string;
  body: string;
}

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

/**
 * Where unconfirmed aliases wait out a page load. One key per alias, named for
 * the project that owns it, holding ids only — never traits.
 *
 * One key per alias rather than one list per origin, for two reasons that both
 * come down to what a shared blob cannot do. `setItem` and `removeItem` are
 * atomic for a single key, so two tabs cannot lose each other's alias the way a
 * read-modify-write of one list can. And the project is part of the name, so a
 * second AgentRay project on the same origin can never replay — or clear — the
 * first project's pending link, which would send one tenant's anonymous id to
 * the other's endpoint and drop the link it was meant to save.
 */
const PENDING_ALIAS_PREFIX = 'agentray_pending_alias.';

/**
 * Most unconfirmed aliases replayed for one project. A login is one alias, and
 * `identify()` only issues one per `reset()`, so this bound is only reached by a
 * page stuck in a loop; it keeps that loop from filling the origin's storage.
 */
const MAX_PENDING_ALIASES = 8;

type DeliveryOutcome = 'ok' | 'terminal' | 'unsent';

interface PendingAlias {
  anonymousId: string;
  distinctId: string;
  /** Enqueue time, so a replay keeps the order the logins happened in. */
  at: number;
}

/** Every unconfirmed alias this project left on this origin, oldest first. */
function readPendingAliases(apiKey: string): PendingAlias[] {
  const prefix = `${PENDING_ALIAS_PREFIX}${apiKey}.`;
  const pending: PendingAlias[] = [];
  try {
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i);
      if (key === null || !key.startsWith(prefix)) continue;
      const stored = JSON.parse(localStorage.getItem(key) ?? 'null') as {
        distinctId?: unknown;
        at?: unknown;
      } | null;
      if (typeof stored?.distinctId !== 'string') continue;
      pending.push({
        anonymousId: key.slice(prefix.length),
        distinctId: stored.distinctId,
        at: typeof stored.at === 'number' ? stored.at : 0,
      });
    }
  } catch {
    return [];
  }
  return pending.sort((a, b) => a.at - b.at);
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
  private queue: QueuedOperation[] = [];
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
      // A single atomic write, so a second tab writing its own alias at the
      // same moment cannot make this one disappear.
      try {
        localStorage.setItem(
          `${PENDING_ALIAS_PREFIX}${this.apiKey}.${op.anonymousId}`,
          JSON.stringify({ distinctId: op.distinctId, at: Date.now() }),
        );
      } catch {}
    }
    this.queue.push({ op, ...this.encode(op) });
    void this.pump();
  }

  /**
   * Re-queue every alias that an earlier page load stored and never got
   * acknowledged, oldest first. Safe to replay: the server's alias write is
   * idempotent on `(project_id, anonymous_id)`.
   */
  restorePendingAlias(): void {
    const pending = readPendingAliases(this.apiKey);
    for (const stale of pending.slice(0, -MAX_PENDING_ALIASES)) {
      try {
        localStorage.removeItem(`${PENDING_ALIAS_PREFIX}${this.apiKey}.${stale.anonymousId}`);
      } catch {}
    }
    for (const alias of pending.slice(-MAX_PENDING_ALIASES)) {
      const alreadyQueued = this.queue.some(
        (queued) =>
          queued.op.kind === 'alias' && queued.op.anonymousId === alias.anonymousId,
      );
      if (alreadyQueued) continue;
      this.enqueue({
        kind: 'alias',
        anonymousId: alias.anonymousId,
        distinctId: alias.distinctId,
      });
    }
  }

  /** Deliver what is queued; resolves when every operation has settled or stalled. */
  flush(): Promise<void> {
    return this.pump();
  }

  private pump(): Promise<void> {
    if (this.pumping !== null) return this.pumping;
    const pumping = (async () => {
      // Keep draining while the queue came up empty but something arrived
      // meanwhile. An operation enqueued while the previous drain was finishing
      // finds `pumping` still set and so starts nothing of its own; this loop,
      // not that enqueue, is what delivers it — and `flush()` waits for it.
      let drained: boolean;
      do {
        drained = await this.drain();
      } while (drained && this.queue.length > 0);
    })().finally(() => {
      this.pumping = null;
    });
    this.pumping = pumping;
    return pumping;
  }

  /**
   * Deliver the queue front to back. Resolves `true` when it ran out of work
   * and `false` when the head is still unacknowledged — what tells `pump()`
   * "gone" apart from "stalled", since a stalled head would otherwise loop.
   */
  private async drain(): Promise<boolean> {
    while (this.queue.length > 0) {
      const queued = this.queue[0];
      const outcome = await this.deliver(queued, 0);
      // Still unacknowledged: keep it for the next flush or the unload beacon
      // rather than dropping the only record of how two ids are related.
      if (outcome === 'unsent') return false;
      this.queue.shift();
      const { op } = queued;
      if (op.kind !== 'alias') continue;
      // Only this project's own key, and only after it settled: a 4xx can never
      // be fixed by replaying it, and another project's marker is not ours.
      try {
        localStorage.removeItem(`${PENDING_ALIAS_PREFIX}${this.apiKey}.${op.anonymousId}`);
      } catch {}
      if (outcome === 'ok') this.onAliasConfirmed?.(op);
    }
    return true;
  }

  private async deliver(queued: QueuedOperation, attempt: number): Promise<DeliveryOutcome> {
    try {
      const res = await this.fetchImpl(`${this.host}${queued.path}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: queued.body,
        // Deliberately no `keepalive`: it caps a request body near 64 KiB and
        // `/identify` carries caller-authored traits of arbitrary size. Unload
        // durability is the beacon's job, not this flag's.
      });
      // 4xx (bad key, malformed) is not retryable — retrying can't fix it.
      if (res.ok) return 'ok';
      if (res.status >= 400 && res.status < 500) return 'terminal';
      throw new Error(`status ${res.status}`);
    } catch {
      if (attempt + 1 >= this.maxRetries) return 'unsent';
      await backoffDelay(attempt);
      return this.deliver(queued, attempt + 1);
    }
  }

  /**
   * The wire form of an operation, serialized the moment it is enqueued. Doing
   * it here rather than at send time is what makes a retry, a deferred send
   * behind a slow alias, and a beacon all carry the values the caller passed —
   * a caller that reuses a traits object can no longer change what ships.
   */
  private encode(op: IdentityOperation): { path: string; body: string } {
    if (op.kind === 'alias') {
      return {
        path: '/alias',
        body: JSON.stringify({
          api_key: this.apiKey,
          anonymous_id: op.anonymousId,
          distinct_id: op.distinctId,
        }),
      };
    }
    return {
      path: '/identify',
      body: JSON.stringify({
        api_key: this.apiKey,
        distinct_id: op.distinctId,
        $set: op.traits,
        timestamp: op.timestamp,
      }),
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
      for (const queued of this.queue) {
        navigator.sendBeacon(
          `${this.host}${queued.path}`,
          new Blob([queued.body], { type: 'application/json' }),
        );
      }
    };
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'hidden') beaconAll();
    });
    window.addEventListener('pagehide', beaconAll);
  }
}
