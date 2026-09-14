# Changelog

`@agentray/browser` follows the version policy in
[`docs/RELEASING-SDK.md`](../../docs/RELEASING-SDK.md): pre-1.0, a change to what
an event is named is a **minor** bump, because customers already read those
numbers.

## 0.2.0 — 2026-09-14

### Added

- The standard money taxonomy, so a booking means the same thing in every
  AgentRay project:
  - `REVENUE_EVENT` (`'revenue'`) — one settled money booking.
  - `REVENUE_REVERSED_EVENT` (`'revenue_reversed'`) — money that came back.
  - `REFUND_KIND` (`'refund'`) and `STANDARD_REVENUE_KINDS`
    (`payment`, `in_app_purchase`, `subscription`, `wallet_topup`, `donation`).
  - The `RevenueProperties` type: `amount` (integer in the smallest unit of
    `currency`), `currency` (uppercase ISO 4217), `kind`, a stable `$insert_id`,
    and flat non-PII dimensions (`provider`, `transaction_id`, `product_id`,
    `plan`).

### Notes

- **Additive.** Nothing that worked on 0.1.0 stops working; there is no new
  required call and no renamed export.

- **Money has no privileged browser path.** These constants go through the
  ordinary identity-aware `capture()`, which is the point: a browser is a
  forgeable environment, so its claims about money are worth exactly as much as
  its other claims. The SDK supplies `distinct_id`, `timestamp` and `platform`;
  the caller supplies `$insert_id`, and it must be stable, because the AgentRay
  Overview read de-duplicates money rows on it.

- Bookings whose truth is the server's (`POST /capture` from your billing
  provider, or `@agentray/server`) are read identically — one event name, one
  shape, one net-money total.

- `CHANGELOG.md` is now included in the published tarball.
