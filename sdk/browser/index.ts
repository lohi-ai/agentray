/**
 * @agentray/browser — the one-line entrypoint.
 *
 *   import { init } from '@agentray/browser';
 *   const ar = init({ host: 'https://agentray.example.com', apiKey: 'phc_...' });
 *   ar.capture('user.pageview', { path: location.pathname });
 *   ar.identify('user-123', { email: 'alice@example.com' });
 *
 * `init()` wires the identity lifecycle (anonymous ↔ identified, alias on login,
 * reset on logout) from AgentRayClient onto the batching, retrying, beacon-on-
 * unload BatchTransport, and optionally installs autocapture. It returns a small
 * facade; call `.autocapture()` to turn on delegated click/pageview capture.
 */

import { AgentRayClient } from './client';
import { BatchTransport, type TransportOptions } from './transport';
import { installAutocapture, type AutocaptureOptions, type CaptureConfig } from './autocapture';
import { DEFAULT_PLATFORM, withPlatform } from './platform';

export interface InitOptions {
  /** Base URL of the AgentRay server. */
  host: string;
  /** Project API key. */
  apiKey: string;
  /** Turn on delegated autocapture immediately (clicks + pageviews). */
  autocapture?: boolean | AutocaptureOptions;
  /**
   * How autocapture constrains what it collects (`clickAllowlist`,
   * `internalHosts`). Applied to `autocapture: true` and to a later
   * `ar.autocapture()` call that does not pass its own.
   */
  captureConfig?: CaptureConfig;
  /**
   * Honor the browser's privacy signal: when `navigator.doNotTrack` is `"1"` or
   * the legacy `"yes"`, or `navigator.globalPrivacyControl` is true, `init()`
   * returns an inert facade — no anonymous id, no listeners, no network.
   *
   * Default `false`, because a snippet that stops collecting on its own turns a
   * missing number into a mystery. Set it deliberately: it is a promise to that
   * visitor, and one their browser is entitled to have kept.
   */
  respectDoNotTrack?: boolean;
  /** Override batching defaults (size, interval, retries). */
  batching?: Omit<TransportOptions, 'host' | 'apiKey'>;
  /**
   * Value stamped on every event's `platform` property (default `"web"`), which
   * is what lets a product with both a website and a native app read the two
   * audiences apart instead of averaging them. Override only when this bundle is
   * not the website — a Capacitor/Cordova build shipped inside the iOS app
   * should pass `"ios"`.
   */
  platform?: string;
}

export interface AgentRay {
  capture(event: string, properties?: Record<string, unknown>): void;
  identify(userId: string, traits?: Record<string, unknown>): void;
  alias(anonymousId: string, canonicalId: string): void;
  reset(): void;
  getDistinctId(): string;
  /** Flush any buffered events now (returns when the network call settles). */
  flush(): Promise<void>;
  /**
   * Enable delegated autocapture; returns an uninstall function. `config`
   * overrides the `captureConfig` given to `init()`.
   */
  autocapture(opts?: AutocaptureOptions, config?: CaptureConfig): () => void;
}

/**
 * Whether the browser has asserted a privacy preference this SDK can honor.
 * GPC is checked first because it is the newer, still-rare signal: a browser
 * that sends it means it.
 */
function privacySignalSet(): boolean {
  if (typeof navigator === 'undefined') return false;
  const nav = navigator as Navigator & { globalPrivacyControl?: boolean };
  if (nav.globalPrivacyControl === true) return true;
  const dnt = nav.doNotTrack;
  return dnt === '1' || dnt === 'yes';
}

/**
 * The facade a suppressed `init()` returns. Every method is inert and none
 * throws: the host page called `init()` for analytics, and an analytics SDK
 * must never be the reason it breaks. `getDistinctId()` answers `''` rather
 * than inventing an id, because there is no identity here to report.
 */
function suppressedFacade(): AgentRay {
  const noop = () => {};
  return {
    capture: noop,
    identify: noop,
    alias: noop,
    reset: noop,
    getDistinctId: () => '',
    flush: () => Promise.resolve(),
    autocapture: () => noop,
  };
}

export function init(options: InitOptions): AgentRay {
  // Before anything is constructed. AgentRayClient mints and stores an
  // anonymous id, and BatchTransport installs unload listeners, so a check
  // placed any later would still leave a trace of a visitor who said no.
  if (options.respectDoNotTrack && privacySignalSet()) return suppressedFacade();

  const transport = new BatchTransport({
    host: options.host,
    apiKey: options.apiKey,
    ...(options.batching ?? {}),
  });
  // The identity client owns distinct_id / alias / reset; we route its outbound
  // capture calls through the batching transport instead of one-shot fetches,
  // and its identify/alias calls through the transport's identity lane.
  const platform = options.platform ?? DEFAULT_PLATFORM;
  const client = new AgentRayClient({
    apiUrl: options.host,
    apiKey: options.apiKey,
    platform,
    identity: transport.identity,
  });

  const capture = (event: string, properties: Record<string, unknown> = {}) => {
    transport.enqueue({
      event,
      distinct_id: client.getDistinctId(),
      properties: withPlatform(properties, platform),
      timestamp: new Date().toISOString(),
    });
  };

  const facade: AgentRay = {
    capture,
    identify: (userId, traits) => client.identify(userId, traits),
    alias: (anonymousId, canonicalId) => client.alias(anonymousId, canonicalId),
    reset: () => client.reset(),
    getDistinctId: () => client.getDistinctId(),
    // Identity first: a pending alias is what makes the events behind it
    // attributable, so a flush that returns before it settles is a lie.
    flush: async () => {
      await transport.identity.flush();
      await transport.flush();
    },
    autocapture: (opts, config) =>
      installAutocapture(capture, opts, config ?? options.captureConfig),
  };

  if (options.autocapture) {
    facade.autocapture(typeof options.autocapture === 'object' ? options.autocapture : undefined);
  }
  return facade;
}

export { AgentRayClient } from './client';
export { BatchTransport } from './transport';
export type { TransportOptions, BatchEvent } from './transport';
export { installAutocapture } from './autocapture';
export type { AutocaptureOptions, CaptureConfig } from './autocapture';
export { DEFAULT_PLATFORM } from './platform';
