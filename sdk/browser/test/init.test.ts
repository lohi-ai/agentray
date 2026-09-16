import { describe, expect, it, beforeEach, vi } from 'vitest';
import { init } from '../index';

/** One request the SDK made, as the suite reads it back. */
interface CapturedCall {
  path: string;
  body: {
    batch: Array<{
      event: string;
      distinct_id: string;
      properties: Record<string, unknown>;
    }>;
    [field: string]: unknown;
  };
}

function stubFetch() {
  const calls: CapturedCall[] = [];
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

describe('init()', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it('tags every event with platform: web', async () => {
    const calls = stubFetch();
    const ar = init(base);

    ar.capture('user.signup', { plan: 'free' });
    await ar.flush();

    const batch = calls.find((c) => c.path === '/batch')!.body.batch;
    // Without this the site's events fall back to user-agent sniffing on the
    // server; a product that also ships an app then can't tell its two
    // audiences apart, which is the entire reason the column exists.
    expect(batch[0].properties).toMatchObject({ platform: 'web', plan: 'free' });
  });

  it('lets a native shell relabel itself, but not per-call', async () => {
    const calls = stubFetch();
    const ar = init({ ...base, platform: 'ios' });

    // A Capacitor build inside the iOS app is configured once at init; an
    // individual capture must not be able to contradict it by accident.
    ar.capture('user.signup', { platform: 'web' });
    await ar.flush();

    const batch = calls.find((c) => c.path === '/batch')!.body.batch;
    expect(batch[0].properties.platform).toBe('ios');
  });

  it('attributes events to the user after identify', async () => {
    const calls = stubFetch();
    const ar = init(base);
    const anonymous = ar.getDistinctId();

    ar.identify('user_123');
    ar.capture('user.conversion');
    await ar.flush();

    expect(calls.some((c) => c.path === '/alias' && c.body.anonymous_id === anonymous)).toBe(true);
    const batch = calls.find((c) => c.path === '/batch')!.body.batch;
    expect(batch[0].distinct_id).toBe('user_123');
  });

  it('emits a pageview when autocapture is enabled', async () => {
    const calls = stubFetch();
    const ar = init({ ...base, autocapture: true });
    await ar.flush();

    const batch = calls.find((c) => c.path === '/batch')!.body.batch;
    const pageview = batch.find((e) => e.event === 'user.pageview')!;
    expect(pageview).toBeTruthy();
    expect(pageview.properties.platform).toBe('web');
  });

  it('captures a click through the delegated listener', async () => {
    const calls = stubFetch();
    document.body.innerHTML = '<button data-track="Start trial">Start</button>';
    const ar = init({ ...base, autocapture: { pageviews: false } });

    document.querySelector('button')!.click();
    await ar.flush();

    const batch = calls.find((c) => c.path === '/batch')!.body.batch;
    const click = batch.find((e) => e.event === '$autocapture')!;
    expect(click.properties).toMatchObject({ tag: 'button', label: 'Start trial' });
  });

  it('stops capturing once autocapture is uninstalled', async () => {
    const calls = stubFetch();
    document.body.innerHTML = '<button data-track="Start trial">Start</button>';
    const ar = init(base);
    const uninstall = ar.autocapture({ pageviews: false });

    // Prove the listener was live, so the assertion after uninstall means
    // something rather than passing on a button nothing would have captured.
    document.querySelector('button')!.click();
    await ar.flush();
    expect(calls.filter((c) => c.path === '/batch')).toHaveLength(1);

    uninstall();
    document.querySelector('button')!.click();
    await ar.flush();

    expect(calls.filter((c) => c.path === '/batch')).toHaveLength(1);
  });

  it('skips an element marked data-track-ignore', async () => {
    const calls = stubFetch();
    document.body.innerHTML = '<div data-track-ignore><button data-track="Close">x</button></div>';
    const ar = init({ ...base, autocapture: { pageviews: false } });

    document.querySelector('button')!.click();
    await ar.flush();

    expect(calls.filter((c) => c.path === '/batch')).toHaveLength(0);
  });

  it('applies an init-time clickAllowlist to immediate autocapture', async () => {
    const calls = stubFetch();
    document.body.innerHTML = '<button>Plain</button><button data-track="Chosen">Chosen</button>';
    const ar = init({
      ...base,
      autocapture: { pageviews: false },
      captureConfig: { clickAllowlist: '[data-track]' },
    });

    document.querySelectorAll('button')[0].click();
    document.querySelectorAll('button')[1].click();
    await ar.flush();

    const batch = calls.find((c) => c.path === '/batch')!.body.batch;
    const labels = batch
      .filter((e) => e.event === '$autocapture')
      .map((e) => e.properties.label);
    // A consumer that constrains click capture must not have to reach past
    // init() and call installAutocapture itself to do it.
    expect(labels).toEqual(['Chosen']);
  });

  it('normalizes a referrer from init-time internalHosts, and keeps an external one', async () => {
    const calls = stubFetch();
    const referrer = vi.spyOn(document, 'referrer', 'get');

    referrer.mockReturnValue('https://lohi2.com/doc/truyen/1');
    let ar = init({ ...base, autocapture: true, captureConfig: { internalHosts: ['lohi2.com'] } });
    await ar.flush();
    let pageview = calls.find((c) => c.path === '/batch')!.body.batch.find((e) => e.event === 'user.pageview')!;
    expect(pageview.properties.$referrer).toBe('');

    calls.length = 0;
    referrer.mockReturnValue('https://google.com/');
    ar = init({ ...base, autocapture: true, captureConfig: { internalHosts: ['lohi2.com'] } });
    await ar.flush();
    pageview = calls.find((c) => c.path === '/batch')!.body.batch.find((e) => e.event === 'user.pageview')!;
    expect(pageview.properties.$referrer).toBe('https://google.com/');
  });

  it('attaches the pageview\'s UTM tags from location.search', async () => {
    const calls = stubFetch();
    history.replaceState({}, '', '/landing?utm_source=newsletter&utm_campaign=launch-week');
    const ar = init({ ...base, autocapture: true });
    await ar.flush();

    const pageview = calls.find((c) => c.path === '/batch')!.body.batch.find((e) => e.event === 'user.pageview')!;
    expect(pageview.properties.$utm_source).toBe('newsletter');
    expect(pageview.properties.$utm_campaign).toBe('launch-week');
    // Untagged params are absent, not empty — the ingest columns stay ''.
    expect(pageview.properties.$utm_medium).toBeUndefined();

    // An SPA navigation that changes the query string re-reads it per view.
    calls.length = 0;
    history.pushState({}, '', '/pricing?utm_source=google');
    await ar.flush();
    const second = calls.find((c) => c.path === '/batch')!.body.batch.find((e) => e.event === 'user.pageview')!;
    expect(second.properties.$utm_source).toBe('google');
    expect(second.properties.$utm_campaign).toBeUndefined();
    history.replaceState({}, '', '/');
  });

  it('lets an explicit autocapture config override the init default', async () => {
    const calls = stubFetch();
    document.body.innerHTML = '<button>Plain</button>';
    const ar = init({
      ...base,
      autocapture: { pageviews: false },
      captureConfig: { clickAllowlist: '[data-track]' },
    });

    ar.autocapture({ pageviews: false }, { clickAllowlist: 'button' });
    document.querySelector('button')!.click();
    await ar.flush();

    const batch = calls.find((c) => c.path === '/batch')!.body.batch;
    expect(batch.filter((e) => e.event === '$autocapture')).toHaveLength(1);
  });
});
