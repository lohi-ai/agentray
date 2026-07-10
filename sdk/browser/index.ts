/**
 * @agentray/browser — the one-line entrypoint.
 *
 *   import { init } from '@agentray/browser';
 *   const ar = init({ host: 'https://agentray.example.com', apiKey: 'phc_...' });
 *   ar.capture('pageview', { path: location.pathname });
 *   ar.identify('user-123', { email: 'alice@example.com' });
 *
 * `init()` wires the identity lifecycle (anonymous ↔ identified, alias on login,
 * reset on logout) from AgentRayClient onto the batching, retrying, beacon-on-
 * unload BatchTransport, and optionally installs autocapture. It returns a small
 * facade; call `.autocapture()` to turn on delegated click/pageview capture.
 */

import { AgentRayClient } from './client';
import { BatchTransport, type TransportOptions } from './transport';
import { installAutocapture, type AutocaptureOptions } from './autocapture';

export interface InitOptions {
  /** Base URL of the AgentRay server. */
  host: string;
  /** Project API key. */
  apiKey: string;
  /** Turn on delegated autocapture immediately (clicks + pageviews). */
  autocapture?: boolean | AutocaptureOptions;
  /** Override batching defaults (size, interval, retries). */
  batching?: Omit<TransportOptions, 'host' | 'apiKey'>;
}

export interface AgentRay {
  capture(event: string, properties?: Record<string, unknown>): void;
  identify(userId: string, traits?: Record<string, unknown>): void;
  alias(anonymousId: string, canonicalId: string): void;
  reset(): void;
  getDistinctId(): string;
  /** Flush any buffered events now (returns when the network call settles). */
  flush(): Promise<void>;
  /** Enable delegated autocapture; returns an uninstall function. */
  autocapture(opts?: AutocaptureOptions): () => void;
}

export function init(options: InitOptions): AgentRay {
  const transport = new BatchTransport({
    host: options.host,
    apiKey: options.apiKey,
    ...(options.batching ?? {}),
  });
  // The identity client owns distinct_id / alias / reset; we route its outbound
  // capture calls through the batching transport instead of one-shot fetches.
  const client = new AgentRayClient({ apiUrl: options.host, apiKey: options.apiKey });

  const capture = (event: string, properties: Record<string, unknown> = {}) => {
    transport.enqueue({
      event,
      distinct_id: client.getDistinctId(),
      properties,
      timestamp: new Date().toISOString(),
    });
  };

  const facade: AgentRay = {
    capture,
    identify: (userId, traits) => client.identify(userId, traits),
    alias: (anonymousId, canonicalId) => client.alias(anonymousId, canonicalId),
    reset: () => client.reset(),
    getDistinctId: () => client.getDistinctId(),
    flush: () => transport.flush(),
    autocapture: (opts) => installAutocapture(capture, opts),
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
export type { AutocaptureOptions } from './autocapture';
