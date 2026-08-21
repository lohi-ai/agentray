import { describe, expect, it, beforeEach, vi } from 'vitest';
import { AgentRayClient } from '../client';

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

  it('aliases the anonymous history to the user on identify', () => {
    const calls = stubFetch();
    const client = new AgentRayClient(config);
    const anonymous = client.getDistinctId();

    client.identify('user_123', { email: 'alice@example.com' });

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
});
