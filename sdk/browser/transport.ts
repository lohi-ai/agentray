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

export class BatchTransport {
  private readonly host: string;
  private readonly apiKey: string;
  private readonly batchSize: number;
  private readonly flushIntervalMs: number;
  private readonly maxRetries: number;
  private readonly fetchImpl: typeof fetch;

  private queue: BatchEvent[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor(opts: TransportOptions) {
    this.host = opts.host.replace(/\/$/, '');
    this.apiKey = opts.apiKey;
    this.batchSize = opts.batchSize ?? 20;
    this.flushIntervalMs = opts.flushIntervalMs ?? 3000;
    this.maxRetries = opts.maxRetries ?? 3;
    this.fetchImpl = opts.fetchImpl ?? ((...a) => fetch(...a));
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
      const backoff = Math.min(1000 * 2 ** attempt, 8000);
      await new Promise((r) => setTimeout(r, backoff));
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
