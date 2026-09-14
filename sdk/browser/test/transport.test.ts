import { describe, expect, it, beforeEach, vi } from 'vitest';
import { BatchTransport, IdentityQueue, type BatchEvent } from '../transport';
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
    const fetchImpl = vi.fn(async () => new Response('', { status: 401 }));
    const transport = new BatchTransport({ ...base, batchSize: 1, fetchImpl });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(30_000);

    // A bad key cannot be fixed by sending it again; retrying would only queue
    // every later batch behind a request that can never succeed.
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    vi.useRealTimers();
  });

  it('gives up after maxRetries rather than looping forever', async () => {
    vi.useFakeTimers();
    const fetchImpl = vi.fn(async () => new Response('', { status: 500 }));
    const transport = new BatchTransport({ ...base, batchSize: 1, maxRetries: 3, fetchImpl });

    transport.enqueue(event('a'));
    await vi.advanceTimersByTimeAsync(60_000);

    expect(fetchImpl).toHaveBeenCalledTimes(3);
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
    const fetchImpl = vi.fn(async () => {
      throw new Error('offline');
    });
    const transport = new BatchTransport({ ...base, batchSize: 1, maxRetries: 2, fetchImpl });

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
