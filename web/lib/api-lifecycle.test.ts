import { afterEach, describe, expect, it, vi } from 'vitest';
import { AgentRayAPI, ApiError, apiErrorMessage, newIdempotencyKey } from './api';

// Lifecycle consumer evidence: the web client is one adapter over the same
// reversible-archive contract. These tests pin the wire shape the hooks rely
// on — revision + one idempotency key per intent on every mutation — and the
// typed-error mapping the UI renders (409 → conflict, 404 → not_found,
// 5xx/429 → retryable).

function mockFetch(status: number, payload: unknown = {}) {
  const calls: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal('fetch', async (url: string | URL, init: RequestInit = {}) => {
    calls.push({ url: String(url), init });
    if (status === 204) return new Response(null, { status: 204 });
    return new Response(JSON.stringify(payload), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  });
  return calls;
}

afterEach(() => vi.unstubAllGlobals());

describe('lifecycle mutation wire shape', () => {
  it('sends the expected revision and idempotency key on dashboard update', async () => {
    const calls = mockFetch(200, { dashboard: { id: 'd1', revision: 2 } });
    await new AgentRayAPI('p1').updateDashboard('d1', 'n', 'd', { revision: 1, idempotencyKey: 'k1' });
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toContain('/api/dashboards/d1');
    expect(calls[0].url).toContain('project_id=p1');
    expect(calls[0].init.method).toBe('PUT');
    expect(JSON.parse(String(calls[0].init.body))).toEqual({
      name: 'n', description: 'd', revision: 1, idempotency_key: 'k1',
    });
  });

  it('sends revision + key on the reversible deletes and reorder', async () => {
    const calls = mockFetch(204);
    const api = new AgentRayAPI('p1');
    await api.deleteDashboard('d1', { revision: 3, idempotencyKey: 'dk' });
    await api.deleteChart('c1', { revision: 2, idempotencyKey: 'ck' });
    await api.reorderCharts('d1', ['c2', 'c1'], { revision: 4, idempotencyKey: 'rk' });
    await api.deleteConnector('x1', { revision: 5, idempotencyKey: 'xk' });
    const bodies = calls.map((c) => JSON.parse(String(c.init.body)));
    expect(bodies).toEqual([
      { revision: 3, idempotency_key: 'dk' },
      { revision: 2, idempotency_key: 'ck' },
      { chart_ids: ['c2', 'c1'], revision: 4, idempotency_key: 'rk' },
      { revision: 5, idempotency_key: 'xk' },
    ]);
    expect(calls.map((c) => c.init.method)).toEqual(['DELETE', 'DELETE', 'PUT', 'DELETE']);
    // The endpoint and project routing are part of the contract: a mutation
    // aimed at the wrong URL or a dropped ?project_id must fail this suite.
    expect(calls[0].url).toContain('/api/dashboards/d1');
    expect(calls[1].url).toContain('/api/charts/c1');
    expect(calls[2].url).toContain('/api/dashboards/d1/charts/order');
    expect(calls[3].url).toContain('/api/connectors/x1');
    for (const c of calls) expect(c.url).toContain('project_id=p1');
  });

  it('passes the intent key through atomic connector create and run-now', async () => {
    const calls = mockFetch(200, { connector: { id: 'x1' } });
    const api = new AgentRayAPI('p1');
    await api.createConnector(
      { name: 'pg', kind: 'postgres', dsn: 'postgres://u:p@h/d' },
      { idempotencyKey: 'cc1' },
    );
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toContain('/api/projects/p1/source-connectors');
    expect(JSON.parse(String(calls[0].init.body))).toEqual({
      name: 'pg',
      kind: 'postgres',
      dsn: 'postgres://u:p@h/d',
      idempotency_key: 'cc1',
    });

    await api.runConnectorSync('s1', { idempotencyKey: 'rn1' });
    expect(calls[1].url).toContain('/api/op/run_source');
    expect(calls[1].url).toContain('project_id=p1');
    expect(JSON.parse(String(calls[1].init.body))).toEqual({ sync_id: 's1', idempotency_key: 'rn1' });
  });

  it('omitting opts keeps the pre-revision wire shape', async () => {
    const calls = mockFetch(200, { dashboard: { id: 'd1' } });
    await new AgentRayAPI('p1').updateDashboard('d1', 'n', 'd');
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ name: 'n', description: 'd' });
  });
});

describe('typed error mapping', () => {
  it.each([
    [409, 'conflict'],
    [404, 'not_found'],
    [503, 'retryable'],
    [429, 'retryable'],
    [400, 'error'],
  ] as const)('maps HTTP %i to kind %s', async (status, kind) => {
    mockFetch(status, { message: 'server said no' });
    const err = await new AgentRayAPI('p1').updateDashboard('d1', 'n', 'd').catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.kind).toBe(kind);
    expect(err.status).toBe(status);
  });

  it('renders actionable copy for the typed kinds', () => {
    expect(apiErrorMessage(new ApiError('x', 409, 'conflict'), 'f')).toContain('refresh');
    expect(apiErrorMessage(new ApiError('x', 404, 'not_found'), 'f')).toContain('no longer exists');
    expect(apiErrorMessage(new ApiError('x', 503, 'retryable'), 'f')).toContain('busy');
    expect(apiErrorMessage(new ApiError('x', 400, 'error'), 'f')).toBe('x');
    expect(apiErrorMessage('not an error', 'f')).toBe('f');
  });
});

describe('newIdempotencyKey', () => {
  it('mints a fresh key per intent', () => {
    const a = newIdempotencyKey();
    const b = newIdempotencyKey();
    expect(a).toBeTruthy();
    expect(a).not.toBe(b);
  });
});
