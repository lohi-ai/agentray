/**
 * The standard money taxonomy — two stored event names every AgentRay project
 * shares, so revenue analysis works without each customer inventing its own
 * convention:
 *
 *   `revenue`          one settled money booking
 *   `revenue_reversed` money that came back (refund, chargeback, clawback)
 *
 * Payments, subscriptions, in-app purchases, top-ups and donations are all the
 * same event with a different documented `kind`; there is no separate event per
 * business model. The Overview money tile and the SQL recipe in
 * `docs/ANALYTICS.md` both read exactly these two names.
 *
 * The two rules that make the numbers trustworthy — and the two this SDK
 * enforces — are:
 *
 *   1. **The sender declares the unit.** `amount` is an integer in the smallest
 *      unit of `currency` (`1900` USD is $19.00, `50000` VND is 50,000 ₫) and
 *      `currency` is an uppercase ISO 4217 code. Nothing is converted: there is
 *      no FX, and totals never cross currencies.
 *   2. **Every money write carries a stable idempotency key.** The read
 *      de-duplicates on `$insert_id` (last write wins), so a retried webhook
 *      carrying the same key is one booking, and a correction re-uses the key it
 *      corrects. A refund is *not* a correction — it gets its own key, or it
 *      would replace the booking instead of netting against it.
 *
 * Both come from the same contract the browser SDK and raw `POST /capture`
 * publish, so a booking sent any of the three ways is read the same way.
 */

/**
 * One settled money booking: money the payer actually handed over.
 *
 * A correction re-uses the booking's `$insert_id`. A refund is emitted under
 * {@link REVENUE_REVERSED_EVENT} instead — or, for callers that predate that
 * name, as a `revenue` row with a negative `amount` or `kind: 'refund'`, which
 * the read nets identically.
 */
export const REVENUE_EVENT = 'revenue';

/**
 * Money that came back. `amount` is the positive sum actually returned; always
 * give the row its own `$insert_id` so it nets against the booking rather than
 * replacing it.
 */
export const REVENUE_REVERSED_EVENT = 'revenue_reversed';

/** The documented kind for a row under {@link REVENUE_REVERSED_EVENT}. */
export const REFUND_KIND = 'refund';

/**
 * The documented booking kinds. They are documentation, not a filter: the read
 * never inspects `kind` for a booking, so an existing `one_time`, `renewal` or
 * customer-defined kind books normally. Pick the one that describes the sale so
 * the data can answer "subscription vs. top-up vs. one-off" later.
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
 * The money facts for a booking. `distinct_id` and `timestamp` are envelope
 * fields, not properties; `platform` and `$insert_id` are stamped by the SDK,
 * so a caller declares exactly these.
 */
export interface RevenueFacts {
  /** Integer in the smallest unit of `currency`. */
  amount: number;
  /** Uppercase ISO 4217 code, e.g. `VND`, `USD`. */
  currency: string;
  /** Documented kind, or a customer-defined one that already exists. */
  kind: RevenueKind;
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

/**
 * The money facts for a reversal: the same dimensions, except `kind` is
 * optional (the SDK stamps `refund` when it is omitted) and `amount` is the
 * positive sum actually returned.
 */
export interface RevenueReversalFacts {
  /** Positive integer in the smallest unit of `currency`. */
  amount: number;
  /** Uppercase ISO 4217 code, e.g. `VND`, `USD`. */
  currency: string;
  /** Defaults to `refund` — the documented reversal kind. */
  kind?: RevenueKind;
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

/**
 * The properties of a money event on the wire, whichever SDK sent it: the facts
 * plus the `$insert_id` that identifies the write. The browser SDK exports this
 * same contract — a row sent from either side is one shape, read one way.
 */
export interface RevenueProperties extends RevenueFacts {
  /** Stable, row-specific idempotency key. Sent as `$insert_id`. */
  $insert_id: string;
}

/**
 * Rejects an amount or currency the read cannot represent honestly, instead of
 * shipping a row that silently books as zero. A `kind` that is present but
 * empty is rejected for the same reason: it would read as "no kind declared".
 */
export function assertRevenueFacts(
  input: { amount: number; currency: string; kind?: RevenueKind },
  event: string,
): void {
  if (typeof input.amount !== 'number' || !Number.isSafeInteger(input.amount)) {
    throw new Error(
      `${event}: amount must be an integer in the currency's smallest unit (got ${String(input.amount)})`,
    );
  }
  if (typeof input.currency !== 'string' || input.currency.trim() === '') {
    throw new Error(`${event}: currency is required (uppercase ISO 4217, e.g. "USD")`);
  }
  if (input.kind !== undefined && (typeof input.kind !== 'string' || input.kind.trim() === '')) {
    throw new Error(`${event}: kind must be a non-empty string when present`);
  }
}

/**
 * A reversal reports what was actually returned, so its amount is strictly
 * positive; the read takes the absolute value either way, but a zero or
 * negative reversal is a caller bug that would understate the money returned.
 */
export function assertReversalFacts(input: RevenueReversalFacts, event: string): void {
  assertRevenueFacts(input, event);
  if (input.amount <= 0) {
    throw new Error(`${event}: amount must be the positive sum actually returned (got ${input.amount})`);
  }
}

/**
 * A money write with no key is exactly the defect the key exists to prevent, so
 * an empty string is rejected too — the type requires a key, but only this
 * catches `idempotencyKey: ''` from a provider field that came back blank.
 */
export function assertIdempotencyKey(key: string, event: string): void {
  if (typeof key !== 'string' || key.trim() === '') {
    throw new Error(`${event}: idempotencyKey must be a non-empty stable id`);
  }
}
