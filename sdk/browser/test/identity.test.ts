import { describe, expect, it, beforeEach, vi } from 'vitest';
import { AgentRayClient } from '../client';
import { deferred } from './deferred';

/** Captures what would have gone over the wire. */
function stubFetch() {
  const calls: Array<{ path: string; body: Record<string, unknown> }> = [];
  const impl = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({
      path: new URL(String(url)).pathname,
      body: JSON.parse(String(init?.body ?? '{}')),
    });
    return new Response('', { status: 200 });
  });
  vi.stubGlobal('fetch', impl);
  return calls;
}

const config = { apiUrl: 'https://agentray.test/', apiKey: 'agentray_test' };

describe('AgentRayClient identity', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it('mints an anonymous id and keeps it across page loads', () => {
    const first = new AgentRayClient(config).getDistinctId();
    // A second client is a second page load in the same browser.
    const second = new AgentRayClient(config).getDistinctId();

    expect(first).toBeTruthy();
    // If this ever regresses, every page load becomes a new "visitor" and the
    // site's people count silently turns into its pageview count.
    expect(second).toBe(first);
  });

  it('aliases the anonymous history to the user on identify', async () => {
    const calls = stubFetch();
    const client = new AgentRayClient(config);
    const anonymous = client.getDistinctId();

    client.identify('user_123', { email: 'alice@example.com' });

    // Delivery is queued and ordered: the alias must land before the traits,
    // so the person the traits attach to already owns the anonymous history.
    await vi.waitFor(() => expect(calls.find((c) => c.path === '/identify')).toBeTruthy());
    const alias = calls.find((c) => c.path === '/alias');
    expect(alias?.body).toMatchObject({
      anonymous_id: anonymous,
      distinct_id: 'user_123',
    });
    const identify = calls.find((c) => c.path === '/identify');
    expect(identify?.body).toMatchObject({
      distinct_id: 'user_123',
      $set: { email: 'alice@example.com' },
    });
    expect(client.getDistinctId()).toBe('user_123');
  });

  it('does not alias a second time for the same user', () => {
    const calls = stubFetch();
    const client = new AgentRayClient(config);

    client.identify('user_123');
    client.identify('user_123');

    expect(calls.filter((c) => c.path === '/alias')).toHaveLength(1);
  });

  it('starts a fresh anonymous identity on reset', () => {
    const client = new AgentRayClient(config);
    const anonymous = client.getDistinctId();
    client.identify('user_123');

    client.reset();

    // The next person on this shared browser must not inherit user_123.
    expect(client.getDistinctId()).not.toBe('user_123');
    expect(client.getDistinctId()).not.toBe(anonymous);
  });

  it('still works when localStorage throws (Safari private mode)', () => {
    const getItem = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied');
    });
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied');
    });

    // A blocked store costs identity continuity, never a thrown error into the
    // host page: an analytics SDK must not be able to break the site.
    expect(() => new AgentRayClient(config).getDistinctId()).not.toThrow();

    getItem.mockRestore();
    setItem.mockRestore();
  });

  it('trims a trailing slash off the host instead of posting to //capture', () => {
    const calls = stubFetch();
    new AgentRayClient(config).capture('user.pageview');
    expect(calls[0].path).toBe('/capture');
  });

  it('keeps the anonymous id until the alias is acknowledged, then drops it', async () => {
    const calls = stubFetch();
    localStorage.setItem('agentray_anon_id', 'anon-kept');
    const client = new AgentRayClient(config);

    client.identify('user_123');
    expect(localStorage.getItem('agentray_anon_id')).toBe('anon-kept');

    // The alias is the only record of how this history belongs to user_123;
    // forgetting the id before the server confirms it would lose the link.
    await vi.waitFor(() => expect(calls.filter((c) => c.path === '/alias')).toHaveLength(1));
    await vi.waitFor(() => expect(localStorage.getItem('agentray_anon_id')).toBeNull());
  });

  it('keeps the anonymous id when the alias is rejected', async () => {
    vi.useFakeTimers();
    const calls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string | URL | Request) => {
        calls.push(new URL(String(url)).pathname);
        return new Response('', { status: 401 });
      }),
    );
    localStorage.setItem('agentray_anon_id', 'anon-kept');
    const client = new AgentRayClient(config);

    client.identify('user_123');
    // Past every backoff window, so a retry would have happened by now.
    await vi.advanceTimersByTimeAsync(60_000);

    // A bad key cannot be fixed by retrying, and the id is not the problem —
    // erasing it would strand the history of a visitor who may come back.
    expect(calls.filter((p) => p === '/alias')).toHaveLength(1);
    expect(localStorage.getItem('agentray_anon_id')).toBe('anon-kept');
    vi.useRealTimers();
  });

  it('does not send identity work as batch events', async () => {
    const calls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string | URL | Request) => {
        calls.push(new URL(String(url)).pathname);
        return new Response('', { status: 200 });
      }),
    );
    const client = new AgentRayClient(config);

    client.identify('user_123', { plan: 'pro' });
    await vi.waitFor(() => expect(calls).toContain('/identify'));

    // /batch has no operation discriminator and never merges a top-level $set,
    // so an "$alias" sent there is accepted and stitches nothing.
    expect(calls).not.toContain('/batch');
    expect(calls.sort()).toEqual(['/alias', '/identify']);
  });

  it('replays an alias that a previous page load never confirmed', async () => {
    const calls = stubFetch();
    localStorage.setItem('agentray_anon_id', 'anon-from-last-page');
    localStorage.setItem(
      'agentray_pending_alias',
      JSON.stringify({ anonymousId: 'anon-from-last-page', distinctId: 'user_123' }),
    );

    new AgentRayClient(config);

    // Only the alias is recoverable: traits were never written to storage, so
    // replaying them is neither possible nor wanted.
    await vi.waitFor(() => expect(calls.filter((c) => c.path === '/alias')).toHaveLength(1));
    expect(calls[0].body).toMatchObject({
      anonymous_id: 'anon-from-last-page',
      distinct_id: 'user_123',
    });
    await vi.waitFor(() =>
      expect(localStorage.getItem('agentray_pending_alias')).toBeNull(),
    );
    expect(calls.filter((c) => c.path === '/identify')).toHaveLength(0);
  });

  it('lets a logout reset win the race against its own in-flight alias', async () => {
    const calls: string[] = [];
    const aliasInFlight = deferred<void>();
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string | URL | Request) => {
        const path = new URL(String(url)).pathname;
        calls.push(path);
        if (path === '/alias') await aliasInFlight.promise;
        return new Response('', { status: 200 });
      }),
    );
    const client = new AgentRayClient(config);
    const anonymous = client.getDistinctId();
    client.identify('user_123');
    await vi.waitFor(() => expect(calls).toContain('/alias'));

    client.reset();
    const nextVisitor = client.getDistinctId();
    aliasInFlight.resolve();
    // The identify that follows the alias only goes out once the alias has
    // settled, so this waits on the confirmation the race turns on.
    await vi.waitFor(() => expect(calls).toContain('/identify'));

    // Confirming the previous user's alias must not erase the id the next
    // visitor is already being tracked under.
    expect(nextVisitor).not.toBe(anonymous);
    expect(localStorage.getItem('agentray_anon_id')).toBe(nextVisitor);
    expect(client.getDistinctId()).toBe(nextVisitor);
  });
});
