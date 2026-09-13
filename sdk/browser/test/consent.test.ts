import { describe, expect, it, beforeEach, vi } from 'vitest';
import { init } from '../index';

const base = { host: 'https://agentray.test', apiKey: 'agentray_test' };

/** The only navigator members this contract reads, so a row cannot pass by accident. */
function stubNavigator(doNotTrack: string | null, globalPrivacyControl?: boolean) {
  vi.stubGlobal('navigator', { ...navigator, doNotTrack, globalPrivacyControl });
}

function stubFetch() {
  const calls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string | URL | Request) => {
      calls.push(new URL(String(url)).pathname);
      return new Response('', { status: 200 });
    }),
  );
  return calls;
}

describe('init() privacy signal', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it('collects normally when the opt-in is absent, whatever the browser asks', async () => {
    stubNavigator('1');
    const calls = stubFetch();

    const ar = init(base);
    ar.capture('user.signup');
    await ar.flush();

    // Default off is the contract: a site that has not promised DNT handling
    // must not silently lose the events it is used to receiving.
    expect(calls).toContain('/batch');
    expect(localStorage.getItem('agentray_anon_id')).toBeTruthy();
  });

  it.each<[string | null, boolean | undefined]>([
    [null, undefined],
    ['0', undefined],
    [null, false],
  ])('collects when the opt-in is set but the signal says nothing (dnt=%s gpc=%s)', async (dnt, gpc) => {
    stubNavigator(dnt, gpc);
    const calls = stubFetch();

    const ar = init({ ...base, respectDoNotTrack: true });
    ar.capture('user.signup');
    await ar.flush();

    expect(calls).toContain('/batch');
  });

  it.each<[string | null, boolean | undefined]>([
    ['1', undefined],
    ['yes', undefined],
    [null, true],
  ])('is inert when the opt-in meets a signal (dnt=%s gpc=%s)', async (dnt, gpc) => {
    const calls = stubFetch();
    const sendBeacon = vi.fn(() => true);
    vi.stubGlobal('navigator', { ...navigator, doNotTrack: dnt, globalPrivacyControl: gpc, sendBeacon });
    const randomUUID = vi.spyOn(crypto, 'randomUUID');
    const docAdd = vi.spyOn(document, 'addEventListener');
    const winAdd = vi.spyOn(window, 'addEventListener');

    const ar = init({ ...base, respectDoNotTrack: true });
    // The host page called init() for analytics; every one of these has to be
    // safe to call, because an SDK must never be why the site breaks.
    expect(() => {
      ar.capture('user.signup', { plan: 'pro' });
      ar.identify('user_123', { email: 'a@b.c' });
      ar.alias('anon', 'user_123');
      ar.reset();
    }).not.toThrow();
    const uninstall = ar.autocapture({ pageviews: true });

    expect(() => uninstall()).not.toThrow();
    expect(ar.getDistinctId()).toBe('');
    await expect(ar.flush()).resolves.toBeUndefined();

    // Nothing was constructed, so there is nothing to send and nothing that
    // could send it after the page hides.
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    document.dispatchEvent(new Event('visibilitychange'));
    window.dispatchEvent(new Event('pagehide'));
    expect(calls).toHaveLength(0);
    expect(sendBeacon).not.toHaveBeenCalled();
    expect(docAdd).not.toHaveBeenCalled();
    expect(winAdd).not.toHaveBeenCalled();
    expect(randomUUID).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);

    docAdd.mockRestore();
    winAdd.mockRestore();
    randomUUID.mockRestore();
  });
});
