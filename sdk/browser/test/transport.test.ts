import { describe, expect, it, beforeEach, vi } from 'vitest';
import { BatchTransport, IdentityQueue, type BatchEvent, type DroppedBatch } from '../transport';
import { deferred } from './deferred';

function event(name: string): BatchEvent {
  return { event: name, distinct_id: 'anon-1', properties: {} };
}

// Typed params, so `mock.calls[0][1].body` is a checked read rather than an
// index into an empty tuple.
function okFetch() {
  return vi.fn(async (_url: string | URL | Request, _init?: RequestInit) =>
    new Response('', { status: 200 }),
  );
}

function beaconSpy() {
  return vi.fn((_url: string | URL, _data?: BodyInit | null) => true);
}

const base = { host: 'https://agentray.test/', apiKey: 'agentray_test' };

describe('BatchTransport', () => {
  it('coalesces events into one request at batchSize', async () => {
    const fetchImpl = okFetch();
    const transport = new BatchTransport({ ...base, batchSize: 3, fetchImpl });

    transport.enqueue(event('a'));
    transport.enqueue(event('b'));
    expect(fetchImpl).not.toHaveBeenCalled();
    transport.enqueue(event('c'));
    await vi.waitFor(() => expect(fetchImpl).toHaveBeenCalledTimes(1));

    const body = JSON.parse(String(fetchImpl.mock.calls[0][1]?.body));
    expect(body.api_key).toBe('agentray_test');
    expect(body.batch.map((e: BatchEvent) => e.event)).toEqual(['a', 'b', 'c']);
    expect(String(fetchImpl.mock.calls[0][0])).toBe('https://agentray.test/batch');
  });

  it('flushes on the interval when the batch never fills', async () => {
    vi.useFakeTimers();
    const fetchImpl = okFetch();
    const transport = new BatchTransport({ ...base, batchSize: 50, flushIntervalMs: 100, fetchImpl });

    transport.enqueue(event('a'));
    expect(fetchImpl).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(150);

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    vi.useRealTimers();
  });

  it('sends nothing when the buffer is empty', async () => {
    const fetchImpl = okFetch();
    await new BatchTransport({ ...base, fetchImpl }).flush();
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it('retries a 5xx with backoff', async () => {
    vi.useFakeTimers();
    const fetchImpl = vi
      .fn()
      .mockResolvedValueOnce(new Response('', { status: 503 }))
      .mockResolvedValue(new Response('', { status: 200 }));
    const transport = new BatchTransport({ ...base, batchSize: 1, fetchImpl });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(5000);

    expect(fetchImpl).toHaveBeenCalledTimes(2);
    vi.useRealTimers();
  });

  it('does not retry a 4xx', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchImpl = vi.fn(async () => new Response('', { status: 401 }));
    const transport = new BatchTransport({ ...base, batchSize: 1, fetchImpl });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(30_000);

    // A bad key cannot be fixed by sending it again; retrying would only queue
    // every later batch behind a request that can never succeed.
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    vi.useRealTimers();
  });

  it('gives up after the retry budget rather than looping forever', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchImpl = vi.fn(async () => new Response('', { status: 500 }));
    const transport = new BatchTransport({ ...base, batchSize: 1, retryBudgetMs: 3000, fetchImpl });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(60_000);

    // Attempts at t, t+1 s, t+3 s: the deadline is what stops it, not a count.
    expect(fetchImpl).toHaveBeenCalledTimes(3);
    vi.useRealTimers();
  });

  it('delivers a batch that outlasts the old three-attempt budget', async () => {
    vi.useFakeTimers();
    const fetchImpl = vi
      .fn()
      .mockResolvedValueOnce(new Response('', { status: 502 }))
      .mockResolvedValueOnce(new Response('', { status: 502 }))
      .mockResolvedValueOnce(new Response('', { status: 502 }))
      .mockResolvedValue(new Response('', { status: 200 }));
    const dropped: DroppedBatch[] = [];
    const transport = new BatchTransport({
      ...base,
      batchSize: 1,
      fetchImpl,
      onBatchDropped: (drop) => dropped.push(drop),
    });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(30_000);

    // The pre-fix transport stopped after three attempts (~3 s) and dropped the
    // batch with no persistence and no signal. The fourth attempt lands at
    // t+7 s, well past that, and the reader's event survives.
    expect(fetchImpl).toHaveBeenCalledTimes(4);
    expect(dropped).toEqual([]);
    vi.useRealTimers();
  });

  it('reports a batch it abandons instead of dropping it silently', async () => {
    vi.useFakeTimers();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchImpl = vi.fn(async () => new Response('', { status: 502 }));
    const dropped: DroppedBatch[] = [];
    const heard: DroppedBatch[] = [];
    const onDrop = (e: Event) => heard.push((e as CustomEvent<DroppedBatch>).detail);
    window.addEventListener('agentray:batch_dropped', onDrop);
    const transport = new BatchTransport({
      ...base,
      batchSize: 1,
      retryBudgetMs: 3000,
      fetchImpl,
      onBatchDropped: (drop) => dropped.push(drop),
    });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(60_000);

    // An event batch has no durable copy, so a drop is data the product will
    // never see — the callback, the warning and the window event are the only
    // trace it leaves.
    expect(dropped).toHaveLength(1);
    expect(dropped[0]).toMatchObject({ attempts: 3, reason: 'status 502' });
    expect(dropped[0].events.map((e) => e.event)).toEqual(['a']);
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('dropped 1 event(s)'));
    expect(heard).toHaveLength(1);
    expect(heard[0].events.map((e) => e.event)).toEqual(['a']);
    window.removeEventListener('agentray:batch_dropped', onDrop);
    vi.useRealTimers();
  });

  it('reports a 4xx drop too, because a rejected batch is still lost', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchImpl = vi.fn(async () => new Response('', { status: 401 }));
    const dropped: DroppedBatch[] = [];
    const transport = new BatchTransport({
      ...base,
      batchSize: 1,
      fetchImpl,
      onBatchDropped: (drop) => dropped.push(drop),
    });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(30_000);

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(dropped).toHaveLength(1);
    expect(dropped[0]).toMatchObject({ attempts: 1, reason: 'status 401' });
    vi.useRealTimers();
  });

  it('does not let a throwing drop callback break the flush', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchImpl = vi.fn(async () => new Response('', { status: 502 }));
    const transport = new BatchTransport({
      ...base,
      batchSize: 1,
      retryBudgetMs: 0,
      fetchImpl,
      onBatchDropped: () => {
        throw new Error('host reporter is broken');
      },
    });

    transport.enqueue(event('a'));
    // The signal runs at the tail of the flush chain, so an exception escaping
    // it would reject `flush()` — an analytics SDK must never be the reason the
    // host page breaks.
    await expect(transport.flush()).resolves.toBeUndefined();
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('does not multiply requests during an outage', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchImpl = vi.fn(async () => new Response('', { status: 502 }));
    const transport = new BatchTransport({
      ...base,
      batchSize: 1,
      flushIntervalMs: 100,
      retryBudgetMs: 60_000,
      fetchImpl,
    });

    // One event every 100 ms for 5 s while the ingest refuses. Serialized, the
    // first batch's backoff (1 s, 2 s, 4 s) paces the requests; a loop per
    // flush interval would be ~50 of them, all hammering a server that is
    // already down.
    for (let i = 0; i < 50; i++) {
      transport.enqueue(event(`e${i}`));
      await vi.advanceTimersByTimeAsync(100);
    }

    expect(fetchImpl.mock.calls.length).toBeLessThan(10);
    vi.useRealTimers();
  });

  it('beacons the buffer out when the tab is hidden', async () => {
    const sendBeacon = beaconSpy();
    vi.stubGlobal('navigator', { ...navigator, sendBeacon });
    const fetchImpl = okFetch();
    const transport = new BatchTransport({ ...base, batchSize: 50, fetchImpl });

    transport.enqueue(event('a'));
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    document.dispatchEvent(new Event('visibilitychange'));

    // The last events of a session are the ones that say how it ended; a normal
    // fetch is abortable as the document tears down, a beacon is not.
    expect(sendBeacon).toHaveBeenCalledTimes(1);
    expect(String(sendBeacon.mock.calls[0][0])).toBe('https://agentray.test/batch');
    expect(fetchImpl).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it('beacons on pagehide, for the bfcache path', () => {
    const sendBeacon = beaconSpy();
    vi.stubGlobal('navigator', { ...navigator, sendBeacon });
    const transport = new BatchTransport({ ...base, batchSize: 50, fetchImpl: okFetch() });

    transport.enqueue(event('a'));
    window.dispatchEvent(new Event('pagehide'));

    expect(sendBeacon).toHaveBeenCalledTimes(1);
    vi.unstubAllGlobals();
  });

  it('does not throw when the network rejects outright', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchImpl = vi.fn(async () => {
      throw new Error('offline');
    });
    const transport = new BatchTransport({ ...base, batchSize: 1, retryBudgetMs: 1000, fetchImpl });

    transport.enqueue(event('a'));
    await expect(vi.advanceTimersByTimeAsync(30_000)).resolves.not.toThrow();
    vi.useRealTimers();
  });
});

describe('IdentityQueue', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('sends identity operations to their own endpoints, never to /batch', async () => {
    const paths: string[] = [];
    const fetchImpl = vi.fn(async (url: string | URL | Request, _init?: RequestInit) => {
      paths.push(new URL(String(url)).pathname);
      return new Response('', { status: 200 });
    });
    const queue = new IdentityQueue({ ...base, fetchImpl });

    queue.enqueue({ kind: 'alias', anonymousId: 'anon-1', distinctId: 'user_1' });
    queue.enqueue({
      kind: 'identify',
      distinctId: 'user_1',
      traits: { plan: 'pro' },
      timestamp: '2026-01-01T00:00:00.000Z',
    });
    await queue.flush();

    // /batch has no operation discriminator and never merges a top-level $set,
    // so an alias sent there is acknowledged and stitches nothing.
    expect(paths).toEqual(['/alias', '/identify']);
    expect(JSON.parse(String(fetchImpl.mock.calls[1][1]?.body))).toEqual({
      api_key: 'agentray_test',
      distinct_id: 'user_1',
      $set: { plan: 'pro' },
      timestamp: '2026-01-01T00:00:00.000Z',
    });
  });

  it('sends identity without fetch keepalive, which caps a body near 64 KiB', async () => {
    const inits: Array<RequestInit | undefined> = [];
    const fetchImpl = vi.fn(async (_url: string | URL | Request, init?: RequestInit) => {
      inits.push(init);
      return new Response('', { status: 200 });
    });
    const queue = new IdentityQueue({ ...base, fetchImpl });

    // Traits are the caller's to size; under keepalive a body this big is
    // rejected outright, so the payload would retry and then never land.
    queue.enqueue({
      kind: 'identify',
      distinctId: 'user_1',
      traits: { bio: 'x'.repeat(70_000) },
      timestamp: '2026-01-01T00:00:00.000Z',
    });
    await queue.flush();

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(inits[0]?.keepalive).toBeUndefined();
  });

  it('retries a 5xx before giving up on an alias', async () => {
    vi.useFakeTimers();
    const fetchImpl = vi
      .fn()
      .mockResolvedValueOnce(new Response('', { status: 503 }))
      .mockResolvedValue(new Response('', { status: 200 }));
    const queue = new IdentityQueue({ ...base, fetchImpl });

    queue.enqueue({ kind: 'alias', anonymousId: 'anon-1', distinctId: 'user_1' });
    await vi.advanceTimersByTimeAsync(5000);

    expect(fetchImpl).toHaveBeenCalledTimes(2);
    vi.useRealTimers();
  });

  it('keeps an unacknowledged alias for a later flush instead of dropping it', async () => {
    vi.useFakeTimers();
    const fetchImpl = vi.fn(async () => new Response('', { status: 500 }));
    const queue = new IdentityQueue({ ...base, maxRetries: 2, fetchImpl });

    queue.enqueue({ kind: 'alias', anonymousId: 'anon-1', distinctId: 'user_1' });
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchImpl).toHaveBeenCalledTimes(2);

    // A dropped alias orphans the anonymous history for good; the queue is the
    // only place left that knows the two ids belong together.
    fetchImpl.mockResolvedValue(new Response('', { status: 200 }));
    await queue.flush();
    expect(fetchImpl).toHaveBeenCalledTimes(3);
    vi.useRealTimers();
  });

  it('beacons unconfirmed operations on pagehide without treating that as delivery', async () => {
    const sendBeacon = vi.fn((_url: string | URL, _data?: BodyInit | null) => true);
    vi.stubGlobal('navigator', { ...navigator, sendBeacon });
    const aliasInFlight = deferred<void>();
    const fetchImpl = vi.fn(async (url: string | URL | Request) => {
      if (new URL(String(url)).pathname === '/alias') await aliasInFlight.promise;
      return new Response('', { status: 200 });
    });
    const queue = new IdentityQueue({ ...base, fetchImpl });

    queue.enqueue({ kind: 'alias', anonymousId: 'anon-1', distinctId: 'user_1' });
    queue.enqueue({
      kind: 'identify',
      distinctId: 'user_1',
      traits: { plan: 'pro' },
      timestamp: '2026-01-01T00:00:00.000Z',
    });
    window.dispatchEvent(new Event('pagehide'));

    // The in-flight alias is included: nothing is confirmed yet, so it is
    // still the only thing that can save this session's anonymous history.
    expect(sendBeacon).toHaveBeenCalledTimes(2);
    expect(String(sendBeacon.mock.calls[0][0])).toBe('https://agentray.test/alias');
    expect(String(sendBeacon.mock.calls[1][0])).toBe('https://agentray.test/identify');
    expect((sendBeacon.mock.calls[0][1] as Blob).type).toBe('application/json');

    window.dispatchEvent(new Event('pagehide'));
    // Accepted by the browser is not answered by the server, so the queue still
    // holds both, and a bfcache restore can deliver them for real.
    expect(sendBeacon).toHaveBeenCalledTimes(4);
    aliasInFlight.resolve();
    vi.unstubAllGlobals();
  });

  it('delivers an operation enqueued while the previous drain was finishing', async () => {
    const paths: string[] = [];
    const fetchImpl = vi.fn(async (url: string | URL | Request) => {
      paths.push(new URL(String(url)).pathname);
      return new Response('', { status: 200 });
    });
    const queue = new IdentityQueue({ ...base, fetchImpl });

    let armed = false;
    queue.onAliasConfirmed = () => {
      if (armed) return;
      armed = true;
      // A caller that resolves the next login in a promise continuation lands
      // exactly in this window: after the drain shifted its last operation and
      // before the in-flight pump is cleared, so its own enqueue finds a pump
      // that has already finished and starts nothing.
      queueMicrotask(() => {
        queue.enqueue({ kind: 'alias', anonymousId: 'anon-2', distinctId: 'user_2' });
      });
    };
    queue.enqueue({ kind: 'alias', anonymousId: 'anon-1', distinctId: 'user_1' });
    await queue.flush();

    // Nothing else on this page will trigger the queue, so an operation the
    // pump forgets is an alias that never reaches the server.
    await vi.waitFor(() => expect(paths).toEqual(['/alias', '/alias']));
  });
});
