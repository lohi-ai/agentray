/**
 * @agentray/server — server-side event capture for events the browser must not
 * be trusted to send (payments, subscriptions, refunds, webhook-driven state).
 *
 *   import { AgentRayServerClient } from '@agentray/server';
 *   const ar = new AgentRayServerClient({ apiUrl: process.env.AGENTRAY_URL!, apiKey: process.env.AGENTRAY_API_KEY! });
 *   await ar.revenue('user-123', { amount: 19, currency: 'USD' }, { idempotencyKey: webhook.id });
 */

export { AgentRayServerClient } from './client';
export type { AgentRayServerConfig, CaptureOptions, RevenueEvent } from './client';
