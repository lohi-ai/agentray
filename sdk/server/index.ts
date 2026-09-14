/**
 * @agentray/server — server-side event capture for events the browser must not
 * be trusted to send (payments, subscriptions, refunds, webhook-driven state).
 *
 *   import { AgentRayServerClient, REVENUE_EVENT } from '@agentray/server';
 *   const ar = new AgentRayServerClient({ apiUrl: process.env.AGENTRAY_URL!, apiKey: process.env.AGENTRAY_API_KEY! });
 *   await ar.revenue('user-123', { amount: 1900, currency: 'USD', kind: 'subscription' },
 *     { idempotencyKey: webhook.id });
 */

export { AgentRayServerClient, DEFAULT_PLATFORM } from './client';
export type { AgentRayServerConfig, CaptureOptions, RevenueOptions } from './client';
// The standard money taxonomy. `RevenueProperties` is the wire contract — the
// same type the browser SDK exports — and `revenue()`/`revenueReversed()` are
// its sanctioned producers.
export {
  REVENUE_EVENT,
  REVENUE_REVERSED_EVENT,
  REFUND_KIND,
  STANDARD_REVENUE_KINDS,
} from './money';
export type {
  StandardRevenueKind,
  RevenueKind,
  RevenueProperties,
  RevenueFacts,
  RevenueReversalFacts,
} from './money';
