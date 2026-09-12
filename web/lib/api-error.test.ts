import { afterEach, describe, expect, it, vi } from 'vitest';
import { AgentRayAPI, APIError } from './api';

// The opcore error contract over the wire: {error, code} — the client must
// surface the kind on APIError so consumers branch on it instead of text.
describe('APIError', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('carries the op error code and status', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(
      JSON.stringify({ error: 'sync is paused — resume it before running', code: 'conflict' }),
      { status: 409, headers: { 'Content-Type': 'application/json' } },
    )));

    const api = new AgentRayAPI('proj-1');
    const err = await api.callOp('run_source', { sync_id: 's1' }).then(
      () => { throw new Error('should have failed'); },
      (e) => e,
    );
    expect(err).toBeInstanceOf(APIError);
    expect(err.code).toBe('conflict');
    expect(err.status).toBe(409);
    expect(err.message).toContain('paused');
  });

  it('still reads legacy {message} bodies with an empty code', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(
      JSON.stringify({ message: 'credential may not invoke run_source' }),
      { status: 403, headers: { 'Content-Type': 'application/json' } },
    )));

    const api = new AgentRayAPI('proj-1');
    const err = await api.callOp('run_source', { sync_id: 's1' }).then(
      () => { throw new Error('should have failed'); },
      (e) => e,
    );
    expect(err).toBeInstanceOf(APIError);
    expect(err.code).toBe('');
    expect(err.status).toBe(403);
    expect(err.message).toContain('credential may not invoke');
  });

  it('connectorSyncs maps source_status rows onto syncs with latest_run', async () => {
    const run = { id: 'run-1', status: 'running', cancel_requested: false, rows: 12, queued_at: '2026-09-12T00:00:00Z' };
    const sync = { id: 's1', connector_id: 'c1', source_table: 'users', enabled: true, revision: 3 };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      expect(String(input)).toContain('/api/op/source_status');
      return new Response(
        JSON.stringify({ syncs: [{ sync, latest_run: run }] }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }));

    const api = new AgentRayAPI('proj-1');
    const res = await api.connectorSyncs('c1');
    expect(res.syncs).toHaveLength(1);
    expect(res.syncs[0].id).toBe('s1');
    expect(res.syncs[0].latest_run?.status).toBe('running');
  });
});
