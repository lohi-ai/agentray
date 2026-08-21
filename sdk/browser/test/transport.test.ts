import { describe, expect, it, vi } from 'vitest';
import { BatchTransport, type BatchEvent } from '../transport';

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
