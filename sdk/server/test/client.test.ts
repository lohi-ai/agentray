import { describe, expect, it, vi } from 'vitest';
import { AgentRayServerClient } from '../client';

function stubFetch(status = 200) {
  const calls: Array<{ path: string; body: any }> = [];
  const impl = vi.fn(async (url: string | URL | Request, opts?: RequestInit) => {
    calls.push({
      path: new URL(String(url)).pathname,
      body: JSON.parse(String(opts?.body ?? '{}')),
    });
    return new Response('', { status });
  });
  vi.stubGlobal('fetch', impl);
  return calls;
}

const config = { apiUrl: 'https://agentray.test/', apiKey: 'agentray_test' };

describe('AgentRayServerClient', () => {
  it('tags events as platform: server', async () => {
    const calls = stubFetch();
    await new AgentRayServerClient(config).capture('u1', 'order_paid', { amount: 19 });

    // Node's global fetch identifies itself only as "node", which no user-agent
    // heuristic can place — without this key, revenue events land in "unknown"
    // and skew the platform split exactly where money is counted.
    expect(calls[0].body.properties).toMatchObject({ platform: 'server', amount: 19 });
    vi.unstubAllGlobals();
  });

  it('sends the provider event id as $insert_id so a webhook retry is de-dupable', async () => {
    const calls = stubFetch();
    const ar = new AgentRayServerClient(config);

    await ar.revenue('u1', { amount: 19, currency: 'USD' }, { idempotencyKey: 'evt_abc' });
    await ar.revenue('u1', { amount: 19, currency: 'USD' }, { idempotencyKey: 'evt_abc' });

    expect(calls.map((c) => c.body.properties.$insert_id)).toEqual(['evt_abc', 'evt_abc']);
    expect(calls[0].body.event).toBe('revenue');
    vi.unstubAllGlobals();
  });

  it('mints a distinct $insert_id when the caller gives none', async () => {
    const calls = stubFetch();
    const ar = new AgentRayServerClient(config);

    await ar.capture('u1', 'a');
    await ar.capture('u1', 'a');

    const [first, second] = calls.map((c) => c.body.properties.$insert_id);
    expect(first).toBeTruthy();
    expect(second).not.toBe(first);
    vi.unstubAllGlobals();
  });

  it('throws on a rejected write, so a dropped payment is not silent', async () => {
    stubFetch(401);
    await expect(
      new AgentRayServerClient(config).capture('u1', 'order_paid'),
    ).rejects.toThrow();
    vi.unstubAllGlobals();
  });

  it('tags and keys every event in a batch', async () => {
    const calls = stubFetch();
    await new AgentRayServerClient(config).batch([
      { distinctId: 'u1', event: 'a' },
      { distinctId: 'u2', event: 'b', options: { idempotencyKey: 'evt_b' } },
    ]);

    const batch = calls[0].body.batch;
    expect(batch).toHaveLength(2);
    expect(batch.every((e: any) => e.properties.platform === 'server')).toBe(true);
    expect(batch[1].properties.$insert_id).toBe('evt_b');
    vi.unstubAllGlobals();
  });

  it('sends nothing for an empty batch', async () => {
    const calls = stubFetch();
    await new AgentRayServerClient(config).batch([]);
    expect(calls).toHaveLength(0);
    vi.unstubAllGlobals();
  });

  it('trims a trailing slash off the host', async () => {
    const calls = stubFetch();
    await new AgentRayServerClient(config).identify('u1', { plan: 'pro' });
    expect(calls[0].path).toBe('/identify');
    vi.unstubAllGlobals();
  });
});
