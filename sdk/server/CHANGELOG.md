# Changelog

`@agentray/server` follows the version policy in
[`docs/RELEASING-SDK.md`](../../docs/RELEASING-SDK.md): pre-1.0, a change to what
an event is named, or to a number a customer already reads, is a **minor** bump.

## 0.2.0 — 2026-09-14

### Added

- The standard money taxonomy, shared with `@agentray/browser` and raw
  `POST /capture`: `REVENUE_EVENT`, `REVENUE_REVERSED_EVENT`, `REFUND_KIND`,
  `STANDARD_REVENUE_KINDS`, and the `RevenueProperties` wire type.
- `revenueReversed(distinctId, facts, { idempotencyKey })` — money that came
  back (refund, chargeback, clawback), sent as `revenue_reversed` with `kind`
  defaulting to `refund`.
- Runtime rejection of payloads the read cannot represent honestly: a
  non-integer `amount`, an empty `currency`, an empty `kind`, or an empty
  `idempotencyKey` throws instead of writing a row that books as zero.

### Changed — migration required

- **`revenue()` now requires an `idempotencyKey`.** Its options are typed
  `RevenueOptions` instead of `CaptureOptions`, so a call without one fails at
  compile time. This is the point of the release: a money write with a random
  key books twice when a payment webhook retries, and the Overview tile would
  report revenue the provider never collected.
- **`amount` is an integer in the smallest unit of `currency`.** 0.1.0 said
  "smallest natural unit you report on (e.g. dollars, not cents — be
  consistent)"; 0.2.0 says cents, đồng, and so on. If your 0.1.0 code sent
  dollars, multiply by 100 (`19` → `1900`) — the stored number is not
  reinterpreted for you, and the read has no way to tell which convention an
  older row used.
- **`revenue()` requires `kind`.** Use the documented vocabulary that describes
  the sale (`payment`, `in_app_purchase`, `subscription`, `wallet_topup`,
  `donation`) or a customer-defined kind you already emit — `one_time` and
  `renewal` still book normally, because the read never filters on `kind`.
- **`RevenueEvent` is replaced** by `RevenueFacts` (what a caller declares) and
  `RevenueProperties` (the wire shape, i.e. the facts plus `$insert_id`).
  Update the type import; the fields are otherwise the same.

### Notes

- A `revenue` row with a negative `amount`, or with `kind: 'refund'`, is still
  read as a reversal — that 0.1.0 shape was never withdrawn, and `revenue()`
  does not reject it, so existing refund emitters keep working. Prefer
  `revenueReversed()`, and give the reversal its own `$insert_id`: re-using the
  booking's key makes the read treat it as a correction of that booking instead
  of a distinct row that nets against it.
- `currency` is not validated against the ISO registry (a sender may declare a
  non-money unit such as `LT`); the read reports such rows as excluded rather
  than silently reinterpreting them.

- `CHANGELOG.md` is now included in the published tarball.
