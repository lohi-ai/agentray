# Lohi evidence pack v1

Use this skill only for the Lohi revenue-drop investigation. It is portable: the
text in this file is the exact body installed on the stock `data-analyst` preset
and the exact body external MCP clients import.

## Safety and evidence contract

1. Call `list_sources`, `source_status`, and `explore_events` before a recipe.
   The operator, never the agent, provisions the six reviewed `ar_lohi.*_v1`
   views. Never issue source DDL or accept raw user PII, OAuth IDs, or a DSN.
2. These recipes bind both `connector_id` and `table_name`. The reviewed v1
   binding is `51515151-5151-4515-8515-515151515151`; an operator must update
   every binding and the manifest together when installing in another project.
3. Treat source readiness and coverage as evidence. A missing/incomplete source
   is `unavailable`, never zero. The frozen post-mortem cutoff is
   `2026-10-03T11:14:00Z` (18:14 HCM); October 3 is partial. Do not compare it to
   a full day without that label.
4. Money has three non-interchangeable universes: completed wallet topups are
   gross VND; canonical deduplicated event money is bookings minus reversals;
   wallet ledger amounts are LT credits, not money. A reversal is any
   `revenue_reversed`, `revenue` with `kind=refund`, or negative money amount;
   subtract `abs(amount)` exactly once when forms overlap. Preserve exact integer
   money/counts and round only ratios.
   For LT, positive reasons ending in `_refund` (and the legacy exact `refund`)
   are reversal credits; negative refund reasons are clawbacks. Negative reasons
   ending in `_hold` are escrow ledger movements, not directly attributable
   consumption. Keep all debits visible and reconcile them exactly to direct
   consumption debits, gross hold debits, and clawbacks. The six exports do not
   include auction/session settlement state, so `lt_spent` and `lt_spenders` are
   qualified direct-debit lower bounds; `lt_per_spender` covers directly observed
   consumers only. Never infer settled escrow consumption from hold/refund pairs.
5. Count event people by `canonical_id`. Join source users only on the approved
   `user_id`. Unknown signup attribution remains unknown; a payment rail is not
   an acquisition source. Never backfill TikTok from a later page view.
6. Causal, campaign-cost, balance, and recovery claims require their own
   evidence. Time alignment is association, projected loss is a scenario, and
   CAC is unavailable without the optional approved spend binding.
7. Before a board write, run the exact recipe and confirm non-empty evidence.
   Save only after explicit authorization. Extend the existing board; do not
   replace its description, charts, or user edits. Every added chart title or
   description must carry the definition version, exact range, unit, and recipe
   reference (`lohi-evidence-v1/Rnn`). Read-only users must stop before writes.

## Export recipes (operator-owned)

| View | Stable key | Minimal fields | Mode |
|---|---|---|---|
| `ar_lohi.users_v1` | `id` | `id,user_id,registered_at,updated_at,signup_provider` | maintained `updated_at` incremental or snapshot |
| `ar_lohi.topups_v1` | `id` | `id,user_id,created_at,completed_at,expires_at,payment_status,provider,amount_vnd,lt_amount` | snapshot |
| `ar_lohi.wallet_ledger_v1` | `id` | `id,user_id,created_at,amount_lt,reason,reference_id` | proven append-only cursor or snapshot |
| `ar_lohi.tts_daily_v1` | `id` | `id,user_id,usage_date,pro_count,free_edge_count` | snapshot |
| `ar_lohi.passes_v1` | `id` | `id,user_id,tier,created_at,status` | snapshot; current-state cross-check only |
| `ar_lohi.reader_days_v1` | encoded composite | `id,user_id,day` | snapshot; coverage start from manifest |

Timestamps are ISO-8601 with offsets. IDs are opaque strings. `signup_provider`
is only `google`, `apple`, or `unknown`; source SQL maps everything else to
`unknown`. `wallet_ledger_v1.reason` retains its source value; recipes normalize
trimmed, lowercased `topup`, `topup_apple_consented`, and legacy
`topup_purchase` to the canonical `topup_purchase` evidence bucket. Views must
exclude email, name, OAuth tokens/IDs, credentials, raw payment metadata, and
free-form user content.

## Canonical recipes

Replace no identifiers casually. The fixed dates reproduce the October 3, 2026
post-mortem. Results follow `date, series, value, unit, sample_size, state,
reason`; R10 adds cohort fields.

### R01 — daily completed topups

<!-- recipe:R01 -->
```sql
WITH bound AS (
  SELECT data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name = 'ar_lohi.topups_v1'
), topups AS (
  SELECT
    json_extract_string(data, '$.id') AS id,
    json_extract_string(data, '$.user_id') AS user_id,
    try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ) AS completed_at,
    try_cast(json_extract_string(data, '$.amount_vnd') AS BIGINT) AS amount_vnd,
    json_extract_string(data, '$.payment_status') AS payment_status
  FROM bound
), daily AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', completed_at) AS DATE) AS day,
         sum(amount_vnd)::BIGINT AS gross_vnd,
         count(DISTINCT id)::BIGINT AS payments,
         count(DISTINCT user_id)::BIGINT AS payers
  FROM topups
  WHERE payment_status = 'completed' AND completed_at < TIMESTAMPTZ '2026-10-03 11:14:00+00'
  GROUP BY 1
), shaped AS (
  SELECT day, 'gross_completed_vnd' AS series, gross_vnd AS value, 'VND' AS unit, payments AS sample_size FROM daily
  UNION ALL SELECT day, 'completed_payments', payments, 'transactions', payments FROM daily
  UNION ALL SELECT day, 'completed_payers', payers, 'people', payments FROM daily
)
SELECT strftime(day, '%Y-%m-%d') AS date, series, value, unit, sample_size,
       CASE WHEN day = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN day = DATE '2026-10-03' THEN 'exclusive cutoff 18:14 HCM' ELSE '' END AS reason
FROM shaped ORDER BY day, series
```

For period averages, use 7 days for Sep 13–19, 7 for Sep 20–26, and 6 for
Sep 27–Oct 2, including covered zero days. Gross VND is not net event revenue.

### R02 — payment rail and denomination

<!-- recipe:R02 -->
```sql
WITH bound AS (
  SELECT data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name = 'ar_lohi.topups_v1'
), topups AS (
  SELECT try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ) AS completed_at,
         lower(coalesce(json_extract_string(data, '$.provider'), 'unknown')) AS provider,
         try_cast(json_extract_string(data, '$.amount_vnd') AS BIGINT) AS amount_vnd
  FROM bound WHERE json_extract_string(data, '$.payment_status') = 'completed'
)
SELECT strftime(cast(timezone('Asia/Ho_Chi_Minh', completed_at) AS DATE), '%Y-%m-%d') AS date,
       'rail:' || provider || ':amount:' || cast(amount_vnd AS VARCHAR) AS series,
       count(*)::BIGINT AS value, 'transactions' AS unit, count(*)::BIGINT AS sample_size,
       CASE WHEN cast(timezone('Asia/Ho_Chi_Minh', completed_at) AS DATE) = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN cast(timezone('Asia/Ho_Chi_Minh', completed_at) AS DATE) = DATE '2026-10-03' THEN 'exclusive cutoff 18:14 HCM, rail is not acquisition' ELSE 'rail is not acquisition' END AS reason
FROM topups WHERE completed_at < TIMESTAMPTZ '2026-10-03 11:14:00+00'
GROUP BY 1, 2, 6, 7 ORDER BY 1, 2
```

The “no 100k+ SePay” claim is `sum(value)` where `series` starts with
`rail:sepay:` and its encoded amount is at least 100000. Include other/unknown
rails rather than forcing them into Apple or SePay.

### R03 — source registrations

<!-- recipe:R03 -->
```sql
WITH bound AS (
  SELECT data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name = 'ar_lohi.users_v1'
), users AS (
  SELECT try_cast(json_extract_string(data, '$.registered_at') AS TIMESTAMPTZ) AS registered_at,
         CASE lower(json_extract_string(data, '$.signup_provider'))
           WHEN 'google' THEN 'google' WHEN 'apple' THEN 'apple' ELSE 'unknown' END AS provider
  FROM bound
)
SELECT strftime(cast(timezone('Asia/Ho_Chi_Minh', registered_at) AS DATE), '%Y-%m-%d') AS date,
       'signups:' || provider AS series, count(*)::BIGINT AS value, 'people' AS unit,
       count(*)::BIGINT AS sample_size,
       CASE WHEN cast(timezone('Asia/Ho_Chi_Minh', registered_at) AS DATE) = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN provider = 'unknown' THEN 'original signup provider unavailable' WHEN cast(timezone('Asia/Ho_Chi_Minh', registered_at) AS DATE) = DATE '2026-10-03' THEN 'exclusive cutoff 18:14 HCM' ELSE '' END AS reason
FROM users WHERE registered_at < TIMESTAMPTZ '2026-10-03 11:14:00+00'
GROUP BY 1, 2, provider, 6, 7 ORDER BY 1, 2
```

Do not substitute sign-ins, first activity, or today’s linked OAuth account.

### R04 — lifetime first payers

<!-- recipe:R04 -->
```sql
WITH bound AS (
  SELECT data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name = 'ar_lohi.topups_v1'
), completed AS (
  SELECT json_extract_string(data, '$.id') AS id,
         json_extract_string(data, '$.user_id') AS user_id,
         try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ) AS completed_at,
         try_cast(json_extract_string(data, '$.amount_vnd') AS BIGINT) AS amount_vnd
  FROM bound WHERE json_extract_string(data, '$.payment_status') = 'completed'
), ranked AS (
  SELECT *, row_number() OVER (PARTITION BY user_id ORDER BY completed_at, id) AS first_rank
  FROM completed
), first_day AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', completed_at) AS DATE) AS day,
         count(*)::BIGINT AS first_payers,
         sum(amount_vnd)::BIGINT AS first_transaction_gross_vnd
  FROM ranked
  WHERE first_rank = 1 AND completed_at < TIMESTAMPTZ '2026-10-03 11:14:00+00' GROUP BY 1
), shaped AS (
  SELECT day, 'first_payers' AS series, first_payers AS value, 'people' AS unit, first_payers AS sample_size FROM first_day
  UNION ALL SELECT day, 'first_transaction_gross_vnd', first_transaction_gross_vnd, 'VND', first_payers FROM first_day
)
SELECT strftime(day, '%Y-%m-%d') AS date, series, value, unit, sample_size,
       CASE WHEN day = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN day = DATE '2026-10-03' THEN 'exclusive cutoff 18:14 HCM, lifetime history required' ELSE 'lifetime history required' END AS reason
FROM shaped ORDER BY day, series
```

Never compute the minimum only inside the report window. This recipe reports one
deterministic first transaction per payer, ordered by `(completed_at, id)`, not
all payments tied at the first timestamp or made on the same day.

### R05 — TTS listeners and paid TTS ledger use

<!-- recipe:R05 -->
```sql
WITH bound AS (
  SELECT table_name, data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name IN ('ar_lohi.tts_daily_v1', 'ar_lohi.wallet_ledger_v1')
), tts AS (
  SELECT try_cast(json_extract_string(data, '$.usage_date') AS DATE) AS day,
         json_extract_string(data, '$.user_id') AS user_id,
         coalesce(try_cast(json_extract_string(data, '$.pro_count') AS BIGINT), 0) AS pro_count,
         coalesce(try_cast(json_extract_string(data, '$.free_edge_count') AS BIGINT), 0) AS free_count
  FROM bound WHERE table_name = 'ar_lohi.tts_daily_v1'
    AND try_cast(json_extract_string(data, '$.usage_date') AS DATE)
      BETWEEN DATE '2026-09-01' AND DATE '2026-10-03'
), ledger AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)) AS DATE) AS day,
         coalesce(try_cast(json_extract_string(data, '$.amount_lt') AS BIGINT), 0) AS amount_lt
  FROM bound WHERE table_name = 'ar_lohi.wallet_ledger_v1'
    AND json_extract_string(data, '$.reason') = 'tts_pro'
    AND try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)
      >= TIMESTAMPTZ '2026-09-01 00:00:00+07'
    AND try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)
      < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), metrics AS (
  SELECT day, 'tts_listeners' AS series, count(DISTINCT user_id)::BIGINT AS value, 'people' AS unit, count(*)::BIGINT AS sample_size
  FROM tts WHERE pro_count + free_count > 0 GROUP BY 1
  UNION ALL
  SELECT day, 'tts_pro_source_count', sum(pro_count)::BIGINT, 'uses', count(*)::BIGINT FROM tts GROUP BY 1
  UNION ALL
  SELECT day, 'tts_pro_paid_lt', sum(CASE WHEN amount_lt < 0 THEN -amount_lt ELSE 0 END)::BIGINT, 'LT', count(*)::BIGINT FROM ledger GROUP BY 1
)
SELECT strftime(day, '%Y-%m-%d') AS date, series, value, unit, sample_size,
       CASE WHEN day = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN day = DATE '2026-10-03' AND series = 'tts_listeners' THEN 'date-only source is bounded by the cutoff snapshot, partial HCM day, request/usage evidence not proof of playback'
            WHEN day = DATE '2026-10-03' THEN 'exclusive cutoff 18:14 HCM, paid ledger use is separate from listener count'
            WHEN series = 'tts_listeners' THEN 'request/usage evidence, not proof of playback'
            ELSE 'paid ledger use is separate from listener count' END AS reason
FROM metrics ORDER BY day, series
```

Do not sum distinct people across the source and ledger; `tts_requested` is
intent and neither source proves audio playback.

### R06 — LT issue/spend flow

<!-- recipe:R06 -->
```sql
WITH bound AS (
  SELECT table_name, data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name IN ('ar_lohi.wallet_ledger_v1', 'ar_lohi.topups_v1')
), ledger AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)) AS DATE) AS day,
         json_extract_string(data, '$.user_id') AS user_id,
         try_cast(json_extract_string(data, '$.amount_lt') AS BIGINT) AS amount_lt,
         CASE
           WHEN lower(trim(coalesce(json_extract_string(data, '$.reason'), '')))
             IN ('topup', 'topup_apple_consented', 'topup_purchase') THEN 'topup_purchase'
           ELSE lower(trim(coalesce(json_extract_string(data, '$.reason'), '')))
         END AS reason
  FROM bound WHERE table_name = 'ar_lohi.wallet_ledger_v1'
    AND try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)
      >= TIMESTAMPTZ '2026-09-01 00:00:00+07'
    AND try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)
      < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), topups AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ)) AS DATE) AS day,
         try_cast(json_extract_string(data, '$.lt_amount') AS BIGINT) AS lt_amount
  FROM bound WHERE table_name = 'ar_lohi.topups_v1'
    AND json_extract_string(data, '$.payment_status') = 'completed'
    AND try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ)
      >= TIMESTAMPTZ '2026-09-01 00:00:00+07'
    AND try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ)
      < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), ledger_daily AS (
  SELECT day,
         sum(CASE WHEN amount_lt > 0 THEN amount_lt ELSE 0 END)::BIGINT AS issued_lt,
         sum(CASE WHEN amount_lt > 0 AND reason = 'topup_purchase' THEN amount_lt ELSE 0 END)::BIGINT AS purchased_lt,
         sum(CASE WHEN amount_lt > 0 AND (reason = 'refund' OR reason LIKE '%_refund') THEN amount_lt ELSE 0 END)::BIGINT AS refunded_lt,
         sum(CASE WHEN amount_lt > 0 AND reason = 'grant' THEN amount_lt ELSE 0 END)::BIGINT AS granted_lt,
         sum(CASE WHEN amount_lt > 0 AND reason <> 'topup_purchase' AND reason <> 'grant'
                       AND NOT (reason = 'refund' OR reason LIKE '%_refund') THEN amount_lt ELSE 0 END)::BIGINT AS other_issued_lt,
         sum(CASE WHEN amount_lt < 0 THEN -amount_lt ELSE 0 END)::BIGINT AS all_debits_lt,
         sum(CASE WHEN amount_lt < 0 AND (reason = 'refund' OR reason LIKE '%_refund') THEN -amount_lt ELSE 0 END)::BIGINT AS clawed_back_lt,
         sum(CASE WHEN amount_lt < 0 AND reason LIKE '%_hold' THEN -amount_lt ELSE 0 END)::BIGINT AS held_lt,
         sum(CASE WHEN amount_lt < 0 AND NOT (reason = 'refund' OR reason LIKE '%_refund' OR reason LIKE '%_hold') THEN -amount_lt ELSE 0 END)::BIGINT AS spent_lt,
         count(DISTINCT CASE WHEN amount_lt < 0 AND NOT (reason = 'refund' OR reason LIKE '%_refund' OR reason LIKE '%_hold') THEN user_id END)::BIGINT AS spenders,
         sum(CASE WHEN amount_lt < 0 AND reason = 'tts_pro' THEN -amount_lt ELSE 0 END)::BIGINT AS audio_spent_lt
  FROM ledger GROUP BY 1
), topup_daily AS (
  SELECT day, sum(lt_amount)::BIGINT AS topup_purchased_lt, count(*)::BIGINT AS topup_rows
  FROM topups GROUP BY 1
), daily AS (
  SELECT coalesce(l.day, t.day) AS day,
         coalesce(l.issued_lt, 0)::BIGINT AS issued_lt,
         coalesce(l.purchased_lt, 0)::BIGINT AS purchased_lt,
         coalesce(l.refunded_lt, 0)::BIGINT AS refunded_lt,
         coalesce(l.granted_lt, 0)::BIGINT AS granted_lt,
         coalesce(l.other_issued_lt, 0)::BIGINT AS other_issued_lt,
         coalesce(l.all_debits_lt, 0)::BIGINT AS all_debits_lt,
         coalesce(l.clawed_back_lt, 0)::BIGINT AS clawed_back_lt,
         coalesce(l.held_lt, 0)::BIGINT AS held_lt,
         coalesce(l.spent_lt, 0)::BIGINT AS spent_lt,
         coalesce(l.spenders, 0)::BIGINT AS spenders,
         coalesce(l.audio_spent_lt, 0)::BIGINT AS audio_spent_lt,
         coalesce(t.topup_purchased_lt, 0)::BIGINT AS topup_purchased_lt,
         coalesce(t.topup_rows, 0)::BIGINT AS topup_rows
  FROM ledger_daily l FULL OUTER JOIN topup_daily t USING (day)
), metrics AS (
  SELECT day, 'lt_issued' AS series, issued_lt::DOUBLE AS value, 'LT' AS unit, spenders AS sample_size FROM daily
  UNION ALL SELECT day, 'lt_purchased_ledger', purchased_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_refunded', refunded_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_granted', granted_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_issued_other', other_issued_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_purchased_topup_control', topup_purchased_lt::DOUBLE, 'LT', topup_rows FROM daily
  UNION ALL SELECT day, 'lt_purchase_reconciliation_delta', (purchased_lt-topup_purchased_lt)::DOUBLE, 'LT', topup_rows FROM daily
  UNION ALL SELECT day, 'lt_all_debits', all_debits_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_clawed_back', clawed_back_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_held', held_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_spent', spent_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_audio_spent', audio_spent_lt::DOUBLE, 'LT', spenders FROM daily
  UNION ALL SELECT day, 'lt_spenders', spenders::DOUBLE, 'people', spenders FROM daily
  UNION ALL SELECT day, 'lt_per_spender', CASE WHEN spenders = 0 THEN NULL ELSE round(spent_lt::DOUBLE / spenders, 2) END, 'LT/person', spenders FROM daily
)
SELECT strftime(day, '%Y-%m-%d') AS date, series, value, unit, sample_size,
       CASE WHEN value IS NULL THEN 'unavailable'
            WHEN day = DATE '2026-10-03' THEN 'partial'
            WHEN series IN ('lt_spent', 'lt_spenders', 'lt_per_spender') THEN 'qualified'
            ELSE 'complete' END AS state,
       CASE WHEN value IS NULL AND series = 'lt_per_spender' THEN 'undefined direct-debit denominator, settled escrow consumption and spender evidence are unavailable in the six exports'
            WHEN value IS NULL THEN 'undefined denominator'
            WHEN day = DATE '2026-10-03' AND series IN ('lt_spent', 'lt_spenders', 'lt_per_spender') THEN 'exclusive cutoff 18:14 HCM, partial HCM day, direct-debit consumption only, settled escrow consumption and spender evidence are unavailable in the six exports'
            WHEN day = DATE '2026-10-03' AND series = 'lt_all_debits' THEN 'exclusive cutoff 18:14 HCM, partial HCM day, exact ledger-debit total: direct consumption debits plus gross escrow-hold debits plus refund clawbacks'
            WHEN day = DATE '2026-10-03' AND series = 'lt_clawed_back' THEN 'exclusive cutoff 18:14 HCM, partial HCM day, negative refund-reason reversals excluded from consumption'
            WHEN day = DATE '2026-10-03' AND series = 'lt_held' THEN 'exclusive cutoff 18:14 HCM, partial HCM day, gross escrow-hold debits, not outstanding escrow or settled price'
            WHEN series = 'lt_issued' THEN 'sum of purchased, refunded, granted and other positive ledger components'
            WHEN series = 'lt_purchased_topup_control' THEN 'completed topups_v1.lt_amount control, compare with lt_purchased_ledger'
            WHEN series = 'lt_purchase_reconciliation_delta' THEN 'ledger purchased minus completed-topup control, nonzero means the covered extracts do not reconcile'
            WHEN series = 'lt_all_debits' THEN 'exact ledger-debit total: direct consumption debits plus gross escrow-hold debits plus refund clawbacks'
            WHEN series = 'lt_clawed_back' THEN 'negative refund-reason reversals, excluded from consumption and spender counts'
            WHEN series = 'lt_held' THEN 'gross negative escrow-hold debits, not outstanding escrow or settled price'
            WHEN series IN ('lt_spent', 'lt_spenders', 'lt_per_spender') THEN 'qualified direct-debit consumption only, settled escrow consumption and spender evidence are unavailable in the six exports'
            WHEN day = DATE '2026-10-03' THEN 'exclusive cutoff 18:14 HCM, partial HCM day'
            ELSE '' END AS reason
FROM metrics ORDER BY day, series
```

`lt_issued` must equal the four positive-ledger components. `lt_all_debits` must
equal `lt_spent + lt_held + lt_clawed_back` as a classification of ledger
movements. This equation does not prove auction lifecycle settlement: the six
exports carry neither settlement state nor the settled price. `lt_spent` and
`lt_spenders` are therefore qualified direct-debit lower bounds, and
`lt_per_spender` describes directly observed consumers only when defined.
Reconcile
`lt_purchased_ledger` to `lt_purchased_topup_control`; do not hide a nonzero
delta. A flow deficit does not prove current balances or the next purchase time.

### R07 — reading controls

<!-- recipe:R07 -->
```sql
WITH source_bound AS (
  SELECT data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name = 'ar_lohi.reader_days_v1'
), reader_days AS (
  SELECT try_cast(json_extract_string(data, '$.day') AS DATE) AS day,
         json_extract_string(data, '$.user_id') AS user_id FROM source_bound
  WHERE try_cast(json_extract_string(data, '$.day') AS DATE)
    BETWEEN DATE '2026-09-01' AND DATE '2026-10-03'
), covered_events AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', timestamp) AS DATE) AS day, canonical_id
  FROM events
  WHERE event_name = 'chapter_view' AND timestamp >= TIMESTAMPTZ '2026-09-01 00:00:00+07'
    AND timestamp < TIMESTAMPTZ '2026-10-03 11:14:00+00'
    AND coalesce(visitor_class, 'human') = 'human'
), metrics AS (
  SELECT day, 'reader_dau' AS series, count(DISTINCT user_id)::BIGINT AS value, 'people' AS unit, count(*)::BIGINT AS sample_size FROM reader_days GROUP BY 1
  UNION ALL SELECT day, 'chapter_views', count(*)::BIGINT, 'events', count(*)::BIGINT FROM covered_events GROUP BY 1
  UNION ALL SELECT day, 'chapter_view_people', count(DISTINCT canonical_id)::BIGINT, 'people', count(*)::BIGINT FROM covered_events GROUP BY 1
)
SELECT strftime(day, '%Y-%m-%d') AS date, series, value, unit, sample_size,
       CASE WHEN day = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN day = DATE '2026-10-03' AND series = 'reader_dau' THEN 'date-only source is bounded by the cutoff snapshot, partial HCM day, durable daily-active control not an event count'
            WHEN day = DATE '2026-10-03' THEN 'exclusive cutoff 18:14 HCM, only within declared event coverage'
            WHEN series = 'reader_dau' THEN 'durable daily-active control, not an event count'
            ELSE 'only within declared event coverage' END AS reason
FROM metrics ORDER BY day, series
```

If coverage does not include September, the read-events/person claim is
unavailable. Current reading history is not historical activity.

### R08 — paid passes

<!-- recipe:R08 -->
```sql
WITH bound AS (
  SELECT data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name = 'ar_lohi.passes_v1'
), current_passes AS (
  SELECT nullif(trim(json_extract_string(data, '$.user_id')), '') AS user_id,
         coalesce(nullif(lower(trim(json_extract_string(data, '$.tier'))), ''), 'unknown') AS tier,
         lower(trim(coalesce(json_extract_string(data, '$.status'), ''))) AS status
  FROM bound
), activation_ranked AS (
  SELECT event_id, canonical_id AS user_id,
         cast(timezone('Asia/Ho_Chi_Minh', timestamp) AS DATE) AS day,
         coalesce(nullif(lower(trim(json_extract_string(properties, '$.tier'))), ''), 'unknown') AS tier,
         lower(trim(coalesce(json_extract_string(properties, '$.source'), ''))) AS source,
         try_cast(json_extract_string(properties, '$.lt_cost') AS BIGINT) AS lt_cost,
         row_number() OVER (
           PARTITION BY coalesce(nullif(insert_id, ''), cast(event_id AS VARCHAR))
           ORDER BY timestamp DESC, event_id DESC
         ) AS retry_rank
  FROM events WHERE event_name = 'subscription_activated'
    AND timestamp >= TIMESTAMPTZ '2026-09-01 00:00:00+07'
    AND timestamp < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), paid AS (
  SELECT day, tier, event_id,
         EXISTS (
           SELECT 1 FROM current_passes p
           WHERE p.user_id = a.user_id AND p.tier = a.tier AND p.status = 'active'
         ) AS current_active_match
  FROM activation_ranked a
  WHERE retry_rank = 1 AND source = 'paid' AND lt_cost > 0
), metrics AS (
  SELECT day, tier, 'paid_pass:' || tier AS series,
         count(DISTINCT event_id)::BIGINT AS value, count(*)::BIGINT AS sample_size
  FROM paid GROUP BY 1, 2
  UNION ALL
  SELECT day, tier, 'paid_pass_current_active_match:' || tier,
         count(DISTINCT event_id) FILTER (WHERE current_active_match)::BIGINT, count(*)::BIGINT
  FROM paid GROUP BY 1, 2
)
SELECT strftime(day, '%Y-%m-%d') AS date, series, value, 'passes' AS unit, sample_size,
       CASE WHEN day = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN series LIKE 'paid_pass_current_active_match:%'
              THEN 'non-authoritative current-state cross-check, cancellations and switches can reduce matches'
            WHEN day = DATE '2026-10-03'
              THEN 'event-backed paid activation with positive incremental LT charge, exclusive cutoff 18:14 HCM, LT is not VND revenue'
            ELSE 'event-backed paid activation with positive incremental LT charge, excludes comps and renewals, LT is not VND revenue' END AS reason
FROM metrics ORDER BY day, series
```

`subscription_activated` is the verified paid-pass mapping: it carries `source`,
`tier`, and the actual incremental `lt_cost`. Wallet subscription rows have null
`reference_id`, so never join them to a fabricated pass reference. Current-state
pass exports are only a cross-check and cannot reconstruct historical
cancellations/switches without dated snapshots. If activation-event coverage is
not verified, the paid-pass claim is unavailable, never zero.

### R09 — checkout state

<!-- recipe:R09 -->
```sql
WITH bound AS (
  SELECT data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name = 'ar_lohi.topups_v1'
), checkouts AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)) AS DATE) AS day,
         json_extract_string(data, '$.payment_status') AS status
  FROM bound
  WHERE try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ) < TIMESTAMPTZ '2026-10-03 11:14:00+00'
)
SELECT strftime(day, '%Y-%m-%d') AS date, 'checkout_status:' || status AS series,
       count(*)::BIGINT AS value, 'checkouts' AS unit, count(*)::BIGINT AS sample_size,
       CASE WHEN day = DATE '2026-10-03' THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN status = 'pending' THEN 'current snapshot status, pending is not abandoned, exact as-of needs an immutable dated export'
            ELSE 'current snapshot status, exact as-of needs an immutable dated export' END AS reason
FROM checkouts GROUP BY 1, 2, day, status ORDER BY 1, 2
```

Low pending count alone does not prove a healthy funnel.

### R10 — signup-to-first-payment cohort lag and attribution

<!-- recipe:R10 -->
```sql
WITH source_bound AS (
  SELECT table_name, data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name IN ('ar_lohi.users_v1', 'ar_lohi.topups_v1')
), users AS (
  SELECT json_extract_string(data, '$.user_id') AS user_id,
         try_cast(json_extract_string(data, '$.registered_at') AS TIMESTAMPTZ) AS registered_at
  FROM source_bound WHERE table_name = 'ar_lohi.users_v1'
    AND try_cast(json_extract_string(data, '$.registered_at') AS TIMESTAMPTZ)
      < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), first_pay AS (
  SELECT json_extract_string(data, '$.user_id') AS user_id,
         min(try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ)) AS first_completed_at
  FROM source_bound
  WHERE table_name = 'ar_lohi.topups_v1' AND json_extract_string(data, '$.payment_status') = 'completed'
    AND try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ)
      < TIMESTAMPTZ '2026-10-03 11:14:00+00'
  GROUP BY 1
), signup_events_ranked AS (
  SELECT canonical_id AS user_id, nullif(utm_source, '') AS acquisition,
         row_number() OVER (PARTITION BY canonical_id ORDER BY timestamp, event_id) AS touch_rank
  FROM events WHERE event_name = 'user_registered'
    AND timestamp < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), signup_events AS (
  SELECT user_id, coalesce(acquisition, 'unknown') AS acquisition
  FROM signup_events_ranked WHERE touch_rank = 1
), people AS (
  SELECT u.user_id, u.registered_at, cast(timezone('Asia/Ho_Chi_Minh', u.registered_at) AS DATE) AS cohort_date,
         floor(epoch(TIMESTAMPTZ '2026-10-03 11:14:00+00' - u.registered_at) / 86400)::BIGINT AS cohort_age_days,
         fp.first_completed_at,
         coalesce(se.acquisition, 'unknown') AS acquisition
  FROM users u LEFT JOIN first_pay fp USING (user_id) LEFT JOIN signup_events se USING (user_id)
), horizons AS (SELECT * FROM (VALUES (7), (14), (30)) AS h(days)), cohort AS (
  SELECT cohort_date, acquisition, days,
         min(cohort_age_days)::BIGINT AS age_days,
         count(*)::BIGINT AS cohort_size,
         count(*) FILTER (WHERE registered_at + days * INTERVAL 1 DAY <= TIMESTAMPTZ '2026-10-03 11:14:00+00')::BIGINT AS eligible,
         count(*) FILTER (WHERE registered_at + days * INTERVAL 1 DAY <= TIMESTAMPTZ '2026-10-03 11:14:00+00'
                           AND first_completed_at < registered_at + days * INTERVAL 1 DAY)::BIGINT AS converted
  FROM people CROSS JOIN horizons GROUP BY 1, 2, 3
)
SELECT strftime(cohort_date, '%Y-%m-%d') AS date,
       'conversion_' || cast(days AS VARCHAR) || 'd:' || acquisition AS series,
       CASE WHEN eligible = 0 THEN NULL ELSE round(100.0 * converted / eligible, 2) END AS value,
       'percent' AS unit, eligible AS sample_size,
       CASE WHEN eligible = 0 THEN 'not_ready' WHEN eligible < cohort_size THEN 'partial' ELSE 'complete' END AS state,
       CASE WHEN eligible = 0 THEN 'no member has a full elapsed horizon at the exclusive cutoff'
            WHEN eligible < cohort_size THEN 'only the elapsed-window-eligible subset is reported'
            WHEN acquisition = 'unknown' THEN 'first registration touch has no source, later tagged touches are not promoted'
            ELSE 'time alignment is association, not causation' END AS reason,
       strftime(cohort_date, '%Y-%m-%d') AS cohort_date, age_days, eligible, converted
FROM cohort ORDER BY cohort_date, days, acquisition
```

The campaign stop itself requires operator/ad-platform evidence. This recipe
uses first-touch-with-unknown attribution: the first `user_registered` event is
selected deterministically, and a missing source stays `unknown` even if a later
registration retry is tagged. It does not prove the seven-day allowance or
causality.

### R11 — scorecard and measurement-universe reconciliation

<!-- recipe:R11 -->
```sql
WITH source_bound AS (
  SELECT table_name, data FROM external_rows
  WHERE connector_id = '51515151-5151-4515-8515-515151515151'
    AND table_name IN ('ar_lohi.topups_v1', 'ar_lohi.wallet_ledger_v1')
), topups AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ)) AS DATE) AS day,
         json_extract_string(data, '$.user_id') AS user_id,
         try_cast(json_extract_string(data, '$.amount_vnd') AS BIGINT) AS amount_vnd,
         try_cast(json_extract_string(data, '$.lt_amount') AS BIGINT) AS lt_amount
  FROM source_bound WHERE table_name = 'ar_lohi.topups_v1'
    AND json_extract_string(data, '$.payment_status') = 'completed'
    AND try_cast(json_extract_string(data, '$.completed_at') AS TIMESTAMPTZ)
      < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), ledger AS (
  SELECT cast(timezone('Asia/Ho_Chi_Minh', try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)) AS DATE) AS day,
         try_cast(json_extract_string(data, '$.amount_lt') AS BIGINT) AS amount_lt,
         CASE
           WHEN lower(trim(coalesce(json_extract_string(data, '$.reason'), '')))
             IN ('topup', 'topup_apple_consented', 'topup_purchase') THEN 'topup_purchase'
           ELSE lower(trim(coalesce(json_extract_string(data, '$.reason'), '')))
         END AS reason
  FROM source_bound WHERE table_name = 'ar_lohi.wallet_ledger_v1'
    AND try_cast(json_extract_string(data, '$.created_at') AS TIMESTAMPTZ)
      < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), event_input AS (
  SELECT event_id, event_name, insert_id, timestamp, canonical_id, utm_source,
         coalesce(try_cast(json_extract_string(properties, '$.amount') AS BIGINT), 0) AS amount,
         upper(trim(coalesce(json_extract_string(properties, '$.currency'), ''))) AS currency,
         lower(trim(coalesce(json_extract_string(properties, '$.kind'), ''))) AS kind
  FROM events WHERE event_name IN ('revenue', 'revenue_reversed', 'user_registered')
    AND timestamp < TIMESTAMPTZ '2026-10-03 11:14:00+00'
), event_money_raw AS (
  SELECT event_id, event_name, insert_id, timestamp, amount, currency, kind,
         row_number() OVER (
           PARTITION BY coalesce(nullif(insert_id, ''), cast(event_id AS VARCHAR))
           ORDER BY timestamp DESC, event_id DESC
         ) AS retry_rank
  FROM event_input WHERE event_name IN ('revenue', 'revenue_reversed')
), event_net AS (
  SELECT coalesce(sum(CASE
           WHEN event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0 THEN -abs(amount)
           ELSE amount
         END), 0)::BIGINT AS value,
         count(*) FILTER (WHERE cast(timezone('Asia/Ho_Chi_Minh', timestamp) AS DATE) = DATE '2026-10-03')::BIGINT AS partial_rows
  FROM event_money_raw WHERE retry_rank = 1 AND currency = 'VND'
), daily AS (
  SELECT day, sum(amount_vnd)::BIGINT AS gross_vnd FROM topups GROUP BY 1
), first_pay AS (
  SELECT user_id, min(day) AS first_day FROM topups GROUP BY 1
), signup_source_ranked AS (
  SELECT canonical_id AS user_id, nullif(utm_source, '') AS acquisition,
         row_number() OVER (PARTITION BY canonical_id ORDER BY timestamp, event_id) AS touch_rank
  FROM event_input WHERE event_name = 'user_registered'
), signup_source AS (
  SELECT user_id, coalesce(acquisition, 'unknown') AS acquisition
  FROM signup_source_ranked WHERE touch_rank = 1
), source_counts AS (
  SELECT coalesce(s.acquisition, 'unknown') AS acquisition, count(*)::BIGINT AS payers
  FROM first_pay f LEFT JOIN signup_source s USING (user_id) GROUP BY 1
), source_mix AS (
  SELECT coalesce(sum(payers) FILTER (WHERE acquisition <> 'unknown'), 0)::BIGINT AS known_payers,
         coalesce(max(payers) FILTER (WHERE acquisition <> 'unknown'), 0)::BIGINT AS largest_known_source,
         coalesce(sum(payers) FILTER (WHERE acquisition NOT IN ('unknown', 'tiktok')), 0)::BIGINT AS outside_tiktok
  FROM source_counts
), score AS (
  SELECT
    coalesce(sum(gross_vnd) FILTER (WHERE day BETWEEN DATE '2026-09-13' AND DATE '2026-09-19'), 0)::DOUBLE / 7 AS baseline_daily,
    coalesce(sum(gross_vnd) FILTER (WHERE day BETWEEN DATE '2026-09-27' AND DATE '2026-10-02'), 0)::DOUBLE / 6 AS current_daily
  FROM daily
), ledger_totals AS (
  SELECT
    coalesce(sum(CASE WHEN amount_lt > 0 THEN amount_lt ELSE 0 END), 0)::BIGINT AS issued_lt,
    coalesce(sum(CASE WHEN amount_lt > 0 AND reason = 'topup_purchase' THEN amount_lt ELSE 0 END), 0)::BIGINT AS purchased_lt,
    coalesce(sum(CASE WHEN amount_lt > 0 AND (reason = 'refund' OR reason LIKE '%_refund') THEN amount_lt ELSE 0 END), 0)::BIGINT AS refunded_lt,
    coalesce(sum(CASE WHEN amount_lt > 0 AND reason = 'grant' THEN amount_lt ELSE 0 END), 0)::BIGINT AS granted_lt,
    coalesce(sum(CASE WHEN amount_lt > 0 AND reason <> 'topup_purchase' AND reason <> 'grant'
                           AND NOT (reason = 'refund' OR reason LIKE '%_refund') THEN amount_lt ELSE 0 END), 0)::BIGINT AS other_issued_lt,
    coalesce(sum(CASE WHEN amount_lt < 0 THEN -amount_lt ELSE 0 END), 0)::BIGINT AS all_debits_lt,
    coalesce(sum(CASE WHEN amount_lt < 0 AND (reason = 'refund' OR reason LIKE '%_refund') THEN -amount_lt ELSE 0 END), 0)::BIGINT AS clawed_back_lt,
    coalesce(sum(CASE WHEN amount_lt < 0 AND reason LIKE '%_hold' THEN -amount_lt ELSE 0 END), 0)::BIGINT AS held_lt,
    coalesce(sum(CASE WHEN amount_lt < 0 AND NOT (reason = 'refund' OR reason LIKE '%_refund' OR reason LIKE '%_hold') THEN -amount_lt ELSE 0 END), 0)::BIGINT AS spent_lt,
    count(*)::BIGINT AS ledger_rows
  FROM ledger WHERE day BETWEEN DATE '2026-09-01' AND DATE '2026-10-02'
), topup_control AS (
  SELECT coalesce(sum(lt_amount), 0)::BIGINT AS purchased_lt, count(*)::BIGINT AS topup_rows
  FROM topups WHERE day BETWEEN DATE '2026-09-01' AND DATE '2026-10-02'
), rows AS (
  SELECT 'baseline_gross_vnd_daily_avg' AS series, round(baseline_daily, 2) AS value, 'VND/day' AS unit, 7::BIGINT AS sample_size, 'complete' AS state, 'Sep 13-19, covered zero days included' AS reason FROM score
  UNION ALL SELECT 'current_gross_vnd_daily_avg', round(current_daily, 2), 'VND/day', 6, 'complete', 'Sep 27-Oct 2, excludes partial Oct 3' FROM score
  UNION ALL SELECT 'gross_vnd_daily_loss', round(baseline_daily-current_daily, 2), 'VND/day', 13, 'complete', 'gross wallet topups, not booked net revenue' FROM score
  UNION ALL SELECT 'projected_30d_loss', round((baseline_daily-current_daily)*30, 2), 'VND/scenario', 13, 'qualified', 'scenario projection, not booked loss' FROM score
  UNION ALL SELECT 'net_event_revenue_vnd', value::DOUBLE, 'VND', 1,
    CASE WHEN partial_rows > 0 THEN 'partial' ELSE 'complete' END,
    CASE WHEN partial_rows > 0
      THEN 'canonical deduplicated bookings minus reversals over available event history through exclusive cutoff 18:14 HCM, includes partial Oct 3, separate universe'
      ELSE 'canonical deduplicated bookings minus reversals over available event history through Oct 2 complete HCM days, separate universe' END
    FROM event_net
  UNION ALL SELECT 'lt_issued', issued_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 complete days, sum of purchased, refunded, granted and other issuance' FROM ledger_totals
  UNION ALL SELECT 'lt_purchased_ledger', purchased_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 positive ledger rows normalized from topup, topup_apple_consented or legacy topup_purchase' FROM ledger_totals
  UNION ALL SELECT 'lt_refunded', refunded_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 positive ledger rows classified as refund issuance' FROM ledger_totals
  UNION ALL SELECT 'lt_granted', granted_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 positive ledger rows classified as grant issuance' FROM ledger_totals
  UNION ALL SELECT 'lt_issued_other', other_issued_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 positive ledger rows outside purchase, refund and grant classes' FROM ledger_totals
  UNION ALL SELECT 'lt_purchased_topup_control', purchased_lt::DOUBLE, 'LT', topup_rows, 'complete', 'Sep 1-Oct 2 completed topups_v1.lt_amount control' FROM topup_control
  UNION ALL SELECT 'lt_purchase_reconciliation_delta', (l.purchased_lt-t.purchased_lt)::DOUBLE, 'LT', t.topup_rows, 'complete', 'ledger purchased minus completed-topup control, nonzero means the covered extracts do not reconcile' FROM ledger_totals l CROSS JOIN topup_control t
  UNION ALL SELECT 'lt_all_debits', all_debits_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 exact ledger-debit total: direct consumption debits plus gross escrow-hold debits plus refund clawbacks' FROM ledger_totals
  UNION ALL SELECT 'lt_clawed_back', clawed_back_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 negative refund-reason reversals, excluded from consumption' FROM ledger_totals
  UNION ALL SELECT 'lt_held', held_lt::DOUBLE, 'LT', ledger_rows, 'complete', 'Sep 1-Oct 2 gross escrow-hold debits, not outstanding escrow or settled price' FROM ledger_totals
  UNION ALL SELECT 'lt_spent', spent_lt::DOUBLE, 'LT', ledger_rows, 'qualified', 'Sep 1-Oct 2 direct-debit consumption lower bound, settled escrow consumption is unavailable in the six exports, LT is not VND' FROM ledger_totals
  UNION ALL SELECT 'known_payer_source_max_share', CASE WHEN known_payers = 0 THEN NULL ELSE round(100.0*largest_known_source/known_payers, 2) END, 'percent', known_payers, CASE WHEN known_payers = 0 THEN 'unavailable' ELSE 'qualified' END, 'known-source denominator, unknown remains separate, 60 percent is a target' FROM source_mix
  UNION ALL SELECT 'payer_outside_tiktok_share', CASE WHEN known_payers = 0 THEN NULL ELSE round(100.0*outside_tiktok/known_payers, 2) END, 'percent', known_payers, CASE WHEN known_payers = 0 THEN 'unavailable' ELSE 'qualified' END, 'known-source denominator, association not campaign proof' FROM source_mix
  UNION ALL SELECT 'cost_per_signup', NULL::DOUBLE, 'VND/person', 0, 'unavailable', 'approved campaign-cost binding not installed'
  UNION ALL SELECT 'cac_first_payer', NULL::DOUBLE, 'VND/person', 0, 'unavailable', 'approved campaign-cost binding not installed'
  UNION ALL SELECT 'recovery', NULL::DOUBLE, 'state', 0, 'unavailable', 'no mature post-action cohort observed'
)
SELECT '2026-10-03' AS date, series, value, unit, sample_size, state, reason FROM rows ORDER BY series
```

The document’s 30–40M VND/month is approximate (`1.4M × 30 = 42M`). Do not
report cost/signup, CAC, channel concentration, or recovery when their bindings
or mature cohorts are absent.

## Saved-board declaration

Suggested existing-board charts are R01 daily VND/payments/payers; R02 rail and
denomination; R03 signups/provider; R04 first payers; R05 TTS; R06 LT flow; R07
reading; R08 paid passes; R09 checkout states; R10 cohort lag; and R11 scorecard.
Append a `Lohi evidence — lohi-evidence-v1` section only after authorization.
Every chart must include its fixed HCM range, source coverage, unit, and recipe
reference. Re-read the board immediately before `save_board`, preserve unknown
fields and existing chart IDs/order, and abort on a revision conflict instead of
overwriting concurrent user edits.
