/**
 * The standard money taxonomy — two stored event names every AgentRay project
 * shares, so revenue analysis works without each customer inventing its own
 * convention.
 *
 *   import { init, REVENUE_EVENT, REVENUE_REVERSED_EVENT } from '@agentray/browser';
 *
 *   const ar = init({ host, apiKey });
 *   ar.capture(REVENUE_EVENT, {
 *     amount: 1900,             // smallest unit: $19.00
 *     currency: 'USD',          // uppercase ISO 4217
 *     kind: 'in_app_purchase',
 *     $insert_id: receipt.transactionId,   // stable; a retried send de-dups
 *   });
 *
 * There is deliberately **no** `revenue()` helper on this SDK. A browser is a
 * forgeable environment, so it must not carry a privileged money path: these
 * constants go through the ordinary identity-aware `capture` like any other
 * event, and the browser's claims about money are worth exactly as much as its
 * other claims. Server-truthful bookings and reversals belong to
 * `@agentray/server`, or to raw HTTP from your billing provider.
 *
 * The SDK owns `distinct_id`, `timestamp` and `platform`; the caller owns
 * `$insert_id`. That split is the whole contract — the read de-duplicates on
 * `$insert_id`, so a random one silently double-counts a retried send.
 */

/**
 * One settled money booking: money the payer actually handed over. Reuse the
 * same `$insert_id` to correct a booking; a refund is never a correction.
 */
export const REVENUE_EVENT = 'revenue';

/**
 * Money that came back: a refund, chargeback, or clawback. Always give it its
 * own `$insert_id` — reusing the booking's key would replace the booking
 * instead of netting against it.
 */
export const REVENUE_REVERSED_EVENT = 'revenue_reversed';

/** The documented kind for a row under {@link REVENUE_REVERSED_EVENT}. */
export const REFUND_KIND = 'refund';

/**
 * The documented booking kinds. They are documentation, not a filter: the read
 * never inspects `kind` for a booking, so an existing `one_time`, `renewal` or
 * customer-defined kind books normally. Pick the one that describes the sale so
 * the data answers "subscription vs. top-up vs. one-off" later.
 */
export const STANDARD_REVENUE_KINDS = [
  'payment',
  'in_app_purchase',
  'subscription',
  'wallet_topup',
  'donation',
] as const;

/** One of {@link STANDARD_REVENUE_KINDS}. */
export type StandardRevenueKind = (typeof STANDARD_REVENUE_KINDS)[number];

/**
 * `kind` accepts the documented vocabulary (which editors autocomplete) and any
 * other non-empty customer string, because an existing taxonomy must keep
 * working through this upgrade.
 */
export type RevenueKind = StandardRevenueKind | typeof REFUND_KIND | (string & {});

/**
 * Properties of {@link REVENUE_EVENT} and {@link REVENUE_REVERSED_EVENT}.
 *
 * `amount` and `currency` are declared by the sender and are never converted:
 * no FX, no cross-currency total, and per-currency sums stay separate. `amount`
 * is an integer in the smallest unit of `currency` (`5000` VND, `1900` USD for
 * $19.00) — a fractional amount is not representable in this contract. Under
 * {@link REVENUE_EVENT} a negative `amount`, or `kind: 'refund'`, is read as a
 * reversal (the shape this SDK documented before this taxonomy existed); under
 * {@link REVENUE_REVERSED_EVENT} `amount` is the positive sum actually
 * returned.
 *
 * The optional keys are flat, non-PII dimensions — ids and enums only. Traits
 * such as email or plan name do not belong on a financial row: send them
 * through `identify()`, so a money event can never rewrite who a person is.
 */
export interface RevenueProperties {
  /** Integer in the smallest unit of `currency`. */
  amount: number;
  /** Uppercase ISO 4217 code, e.g. `VND`, `USD`. */
  currency: string;
  /** Documented kind, or a customer-defined one that already exists. */
  kind: RevenueKind;
  /** Stable, row-specific idempotency key. Sent as `$insert_id`. */
  $insert_id: string;
  /** Payment provider, e.g. `stripe`, `sepay`, `app_store`. */
  provider?: string;
  /** The provider's transaction id, when it differs from `$insert_id`. */
  transaction_id?: string;
  /** The purchased product or SKU. */
  product_id?: string;
  /** The plan id for a subscription purchase. */
  plan?: string;
  [dimension: string]: unknown;
}
