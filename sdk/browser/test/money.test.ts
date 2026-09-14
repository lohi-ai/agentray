import { describe, expect, it, beforeEach, vi } from 'vitest';
import { init, REVENUE_EVENT, REVENUE_REVERSED_EVENT, REFUND_KIND, STANDARD_REVENUE_KINDS } from '../index';
import type { AgentRay, RevenueProperties } from '../index';

/** One request the SDK made, as this suite reads it back. */
interface SentCall {
  path: string;
  body: {
    batch: Array<{
      event: string;
      distinct_id: string;
      timestamp?: string;
      properties: Record<string, unknown>;
    }>;
  };
}

function stubFetch() {
  const calls: SentCall[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string | URL | Request, opts?: RequestInit) => {
      calls.push({
        path: new URL(String(url)).pathname,
        body: JSON.parse(String(opts?.body ?? '{}')),
      });
      return new Response('', { status: 200 });
    }),
  );
  return calls;
}

const base = { host: 'https://agentray.test', apiKey: 'agentray_test' };

/** Flush, then read back what actually left the SDK. */
async function sentBatch(calls: SentCall[], ar: AgentRay) {
  await ar.flush();
  return calls.find((c) => c.path === '/batch')!.body.batch;
}

describe('money taxonomy', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  // The stored names are the contract between every SDK, the raw HTTP envelope
  // and the Overview read. Renaming one here would split a customer's revenue
  // series in half without failing any other test, so they are pinned.
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

  it('sends a booking through the ordinary identity-aware capture, with the caller’s own $insert_id', async () => {
    const calls = stubFetch();
    const ar = init(base);

    const booking: RevenueProperties = {
      amount: 1900,
      currency: 'USD',
      kind: 'in_app_purchase',
      $insert_id: 'receipt-8812',
      product_id: 'coins.500',
    };
    ar.capture(REVENUE_EVENT, booking);
    const batch = await sentBatch(calls, ar);

    expect(batch).toHaveLength(1);
    expect(batch[0].event).toBe('revenue');
    // The caller's key is the dedup key: the read collapses two sends carrying
    // it, so the SDK must not overwrite it with a random one.
    expect(batch[0].properties).toMatchObject({
      amount: 1900,
      currency: 'USD',
      kind: 'in_app_purchase',
      $insert_id: 'receipt-8812',
      product_id: 'coins.500',
    });
    // Identity, platform and time stay the SDK's to assert, not the caller's.
    expect(batch[0].distinct_id).toBe(ar.getDistinctId());
    expect(batch[0].properties.platform).toBe('web');
    expect(typeof batch[0].timestamp).toBe('string');
  });

  it('carries a reversal under its own name and key', async () => {
    const calls = stubFetch();
    const ar = init(base);

    ar.capture(REVENUE_REVERSED_EVENT, {
      amount: 300,
      currency: 'VND',
      kind: REFUND_KIND,
      $insert_id: 'refund-8812',
    });
    const batch = await sentBatch(calls, ar);

    expect(batch[0].event).toBe('revenue_reversed');
    expect(batch[0].properties).toMatchObject({ amount: 300, currency: 'VND', kind: 'refund' });
  });
});
