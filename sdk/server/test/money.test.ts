import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  AgentRayServerClient,
  REFUND_KIND,
  REVENUE_EVENT,
  REVENUE_REVERSED_EVENT,
  STANDARD_REVENUE_KINDS,
} from '../index';

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

describe('money taxonomy', () => {
  beforeEach(() => vi.unstubAllGlobals());

  // The stored names are the contract between this SDK, the browser SDK, raw
  // `POST /capture` and the Overview read. Renaming one here would split a
  // customer's revenue series in half without failing any other test.
  it('pins the stored event names and the reversal kind', () => {
    expect(REVENUE_EVENT).toBe('revenue');
    expect(REVENUE_REVERSED_EVENT).toBe('revenue_reversed');
    expect(REFUND_KIND).toBe('refund');
    expect([...STANDARD_REVENUE_KINDS]).toEqual([
      'payment',
      'in_app_purchase',
      'subscription',
      'wallet_topup',
      'donation',
    ]);
  });

  it('emits a booking under the standard name, keyed by the caller’s idempotency key', async () => {
    const calls = stubFetch();
    const ar = new AgentRayServerClient(config);

    await ar.revenue(
      'u1',
      { amount: 1900, currency: 'USD', kind: 'subscription', plan: 'pro' },
      { idempotencyKey: 'evt_abc' },
    );

    expect(calls[0].path).toBe('/capture');
    expect(calls[0].body.event).toBe('revenue');
    expect(calls[0].body.distinct_id).toBe('u1');
    // The provider's id IS the dedup key: it must survive as `$insert_id`
    // rather than being replaced by the random one `capture()` would mint.
    expect(calls[0].body.properties).toMatchObject({
      amount: 1900,
      currency: 'USD',
      kind: 'subscription',
      plan: 'pro',
      platform: 'server',
      $insert_id: 'evt_abc',
    });
  });

  it('keeps a customer-defined kind readable', async () => {
    const calls = stubFetch();
    const ar = new AgentRayServerClient(config);

    await ar.revenue('u1', { amount: 50000, currency: 'VND', kind: 'one_time' }, { idempotencyKey: 'evt_1' });

    expect(calls[0].body.properties.kind).toBe('one_time');
  });

  it('nets a legacy refund sent as a negative booking instead of rejecting it', async () => {
    const calls = stubFetch();
    const ar = new AgentRayServerClient(config);

    // 0.1.x documented this shape and the read still resolves it as a reversal,
    // so an existing emitter must not start throwing (and dropping the refund).
    await ar.revenue('u1', { amount: -1900, currency: 'USD', kind: 'refund' }, { idempotencyKey: 'evt_2' });

    expect(calls[0].body.event).toBe('revenue');
    expect(calls[0].body.properties.amount).toBe(-1900);
  });

  it('emits a reversal under its own name with kind refund by default', async () => {
    const calls = stubFetch();
    const ar = new AgentRayServerClient(config);

    await ar.revenueReversed('u1', { amount: 1900, currency: 'USD' }, { idempotencyKey: 'refund_9' });

    expect(calls[0].body.event).toBe('revenue_reversed');
    expect(calls[0].body.properties).toMatchObject({
      amount: 1900,
      currency: 'USD',
      kind: 'refund',
      platform: 'server',
      $insert_id: 'refund_9',
    });
  });

  // A key is what makes the row de-dupable, so an empty one is a defect rather
  // than a missing convenience. The type requires it; only these checks catch
  // `idempotencyKey: ''` from a provider field that came back blank.
  it('refuses a blank or missing idempotency key', async () => {
    stubFetch();
    const ar = new AgentRayServerClient(config);
    const booking = { amount: 100, currency: 'USD', kind: 'payment' };

    await expect(ar.revenue('u1', booking, { idempotencyKey: '  ' })).rejects.toThrow(/idempotencyKey/);
    await expect(
      ar.revenue('u1', booking, {} as { idempotencyKey: string }),
    ).rejects.toThrow(/idempotencyKey/);
  });

  it('refuses an amount the read could only book as zero', async () => {
    stubFetch();
    const ar = new AgentRayServerClient(config);
    const key = { idempotencyKey: 'evt_3' };

    await expect(ar.revenue('u1', { amount: 19.5, currency: 'USD', kind: 'payment' }, key)).rejects.toThrow(
      /smallest unit/,
    );
    await expect(
      ar.revenue('u1', { amount: Number.NaN, currency: 'USD', kind: 'payment' }, key),
    ).rejects.toThrow(/smallest unit/);
    await expect(ar.revenue('u1', { amount: 100, currency: '  ', kind: 'payment' }, key)).rejects.toThrow(
      /currency/,
    );
  });

  it('refuses a reversal that reports no money returned', async () => {
    stubFetch();
    const ar = new AgentRayServerClient(config);

    await expect(
      ar.revenueReversed('u1', { amount: 0, currency: 'USD' }, { idempotencyKey: 'refund_0' }),
    ).rejects.toThrow(/positive/);
    await expect(
      ar.revenueReversed('u1', { amount: -100, currency: 'USD' }, { idempotencyKey: 'refund_neg' }),
    ).rejects.toThrow(/positive/);
  });
});
