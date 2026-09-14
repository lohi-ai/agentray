package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// money.go — the ONE money read. Everything that answers "what did this project
// earn?" goes through the grid in moneyRowsCTE: the Overview Net revenue tile
// (overviewRevenue below), and the paid_at recipe published in
// docs/ANALYTICS.md for cohorts and agents. A second money computation
// elsewhere would drift from this one, and the one that drifts is always the
// one somebody charts.
//
// The semantics are the sealed A2 contract (evidence 06):
//
//   - rows are `event_name IN ('revenue','revenue_reversed')` in the window,
//     under the same project/platform filter every other metric uses;
//   - de-duplication groups by `coalesce(nullif(insert_id,''), event_id)` and
//     keeps the LAST write (greatest ("timestamp", event_id)), so a corrected
//     re-send under the same key wins and a JetStream redelivery of an
//     identical body cannot double-count;
//   - a row is money only if it declares a non-empty currency, and `LT` is a
//     platform credit rather than money, so it is excluded and named;
//   - a `revenue` row with `kind='refund'` or a negative amount is a reversal
//     (the shape the server SDK documented before `revenue_reversed` existed),
//     a `revenue_reversed` row contributes abs(amount), and every other
//     `kind` books normally — `kind` is documentation, never a filter;
//   - per currency, net = bookings − reversals, never clamped: more reversals
//     than bookings is a negative number, which is a fact and not an error;
//   - no FX and no cross-currency total, ever. The headline names one declared
//     currency (the largest deduplicated gross, ties broken by code ascending)
//     and every other currency stays separate.

const (
	// moneyBookingEvent is a settled money booking.
	moneyBookingEvent = "revenue"
	// moneyReversalEvent is money that came back: refund, chargeback, clawback.
	moneyReversalEvent = "revenue_reversed"
	// moneyRefundKind is the documented kind of a reversal.
	moneyRefundKind = "refund"
	// moneyNonCurrency is a platform credit, not money. It is excluded from
	// every total and surfaced as an exclusion rather than reinterpreted.
	moneyNonCurrency = "LT"
)

// moneyRowsCTE builds the canonical deduplicated money grid. `where` is the
// caller's project/range/platform fragment (already built, always bound); the
// grid adds only the two money event names, so a caller cannot accidentally
// make "money" mean something else.
//
// The grid carries the canonical person id because the paid_at recipe needs it;
// the tile ignores that column and DuckDB prunes it.
func moneyRowsCTE(where string) string {
	return `
money_raw AS (
	SELECT
		canonical_distinct_id AS person_id,
		event_id,
		event_name,
		"timestamp" AS occurred_at,
		upper(trim(coalesce(json_extract_string(properties, '$.currency'), ''))) AS currency,
		coalesce(try_cast(json_extract_string(properties, '$.amount') AS BIGINT), 0) AS amount,
		lower(trim(coalesce(json_extract_string(properties, '$.kind'), ''))) AS kind,
		coalesce(nullif(insert_id, ''), CAST(event_id AS VARCHAR)) AS row_key
	FROM resolved_events
	WHERE ` + where + ` AND event_name IN ('` + moneyBookingEvent + `', '` + moneyReversalEvent + `')
),
money_rows AS (
	SELECT
		person_id,
		event_id,
		event_name,
		occurred_at,
		currency,
		amount,
		kind,
		row_number() OVER (PARTITION BY row_key ORDER BY occurred_at DESC, event_id DESC) AS write_rank
	FROM money_raw
)`
}

// moneyReverses is the SQL predicate for "this row gives money back". A booking
// that says it is a refund, or carries a negative amount, is a reversal; a
// `revenue_reversed` row always is. It is one expression, reused by every
// consumer of the grid, so the tile and a hand-written query cannot disagree
// about which direction a row moves the total.
const moneyReverses = `(event_name = '` + moneyReversalEvent + `' OR kind = '` + moneyRefundKind + `' OR amount < 0)`

// OverviewRevenueCurrency is one declared currency's arithmetic in the window.
// Gross, Reversed and Net are integers in that currency's smallest unit, as the
// sender declared them; they are never added to another currency's.
type OverviewRevenueCurrency struct {
	Currency string `json:"currency"`
	Gross    int64  `json:"gross"`
	Reversed int64  `json:"reversed"`
	Net      int64  `json:"net"`
	Rows     int64  `json:"rows"`
}

// OverviewRevenueDetail is the signed arithmetic behind the revenue headline:
// the per-currency breakdown, the exclusions that make the total honest, and
// the previous window's net for the same headline currency. It is additive on
// the overview packet — clients that do not know it ignore it.
type OverviewRevenueDetail struct {
	// Window is the range the numbers cover. The context also carries it; it is
	// repeated here so a chart saved from this block is self-describing.
	Window OverviewRange `json:"window"`
	// Currency is the headline currency (dominant deduplicated gross), empty
	// when the window holds no money row.
	Currency string `json:"currency,omitempty"`
	Gross    int64  `json:"gross"`
	Reversed int64  `json:"reversed"`
	// Net is signed and never clamped: reversals can exceed bookings.
	Net int64 `json:"net"`
	// PreviousNet is the previous complete-day window's net FOR THE SAME
	// CURRENCY — a delta is only meaningful within one currency, since there is
	// no FX. Unset for a partial ("today") range, and unset when that window
	// held no money row in the headline currency: an absent comparison is not
	// the same fact as a measured zero.
	PreviousNet *int64 `json:"previous_net,omitempty"`
	// DedupedRows counts the deduplicated MONEY rows the totals were computed
	// from; ExcludedRows counts the deduplicated rows the money test dropped, so
	// DedupedRows+ExcludedRows is every money-shaped row the window held. This
	// is the coverage the tile's provenance line reports.
	DedupedRows  int64 `json:"deduped_rows"`
	ExcludedRows int64 `json:"excluded_rows"`
	// ExcludedCurrencies names the currencies that were deliberately left out
	// (`LT`). Rows that declared no currency at all are counted in
	// ExcludedRows but not named — there is nothing to name.
	ExcludedCurrencies []string                  `json:"excluded_currencies,omitempty"`
	ByCurrency         []OverviewRevenueCurrency `json:"by_currency"`
}

// overviewMoneyWindow is one window's money arithmetic, before the headline
// currency is chosen.
type overviewMoneyWindow struct {
	byCurrency []OverviewRevenueCurrency
	currency   string
	gross      int64
	reversed   int64
	net        int64
	moneyRows  int64
	noCurrency int64
	ltRows     int64
}

// excludedRows is every deduplicated money-shaped row the money test dropped —
// derived, never a third counter to keep in step with the two reasons.
func (w overviewMoneyWindow) excludedRows() int64 { return w.noCurrency + w.ltRows }

// overviewMoney computes one window. The aggregate stays inside DuckDB — the
// grid is grouped by currency there, never materialized as rows in Go — so a
// 90-day window costs one grouped scan, not one row per event.
func (s *Store) overviewMoney(ctx context.Context, projectID string, r OverviewRange, platform string) (overviewMoneyWindow, error) {
	var out overviewMoneyWindow
	where, args := overviewWindowWhere(projectID, r, platform)

	// Rows are ordered by gross descending and then by currency code, which is
	// exactly the headline rule (dominant deduplicated gross, ties broken
	// ascending). The first row that is money is therefore the headline — no
	// second pass, no separate tie-break in Go.
	err := s.duckQuery(ctx, `
WITH `+moneyRowsCTE(where)+`
SELECT
	currency,
	CAST(sum(gross) AS BIGINT) AS gross,
	CAST(sum(reversed) AS BIGINT) AS reversed,
	-- Net is the two columns above subtracted, not a third expression over the
	-- rows: the tile renders all three side by side, so they can never disagree.
	CAST(sum(gross) - sum(reversed) AS BIGINT) AS net,
	CAST(count(*) AS BIGINT) AS rows
FROM (
	SELECT
		`+moneyReverses+` AS reverses,
		currency,
		amount,
		CASE WHEN `+moneyReverses+` THEN 0 ELSE amount END AS gross,
		CASE WHEN `+moneyReverses+` THEN abs(amount) ELSE 0 END AS reversed
	FROM money_rows
	WHERE write_rank = 1
)
GROUP BY currency
ORDER BY gross DESC, currency ASC`, args, func(rows *sql.Rows) error {
		var row OverviewRevenueCurrency
		if err := rows.Scan(&row.Currency, &row.Gross, &row.Reversed, &row.Net, &row.Rows); err != nil {
			return err
		}
		out.byCurrency = append(out.byCurrency, row)
		return nil
	})
	if err != nil {
		return out, err
	}
	// One pass classifies each currency group and keeps the money ones: the
	// "which currencies are money" rule exists once, and the SQL's ordering
	// (gross desc, currency asc) means the first money group is the headline.
	money := make([]OverviewRevenueCurrency, 0, len(out.byCurrency))
	for _, row := range out.byCurrency {
		switch {
		case row.Currency == "":
			// A money-shaped row that declared no unit cannot be added to any
			// total: it is a tracking defect to report, not a number to invent.
			out.noCurrency += row.Rows
		case row.Currency == moneyNonCurrency:
			out.ltRows += row.Rows
		default:
			money = append(money, row)
			out.moneyRows += row.Rows
			if out.currency == "" {
				out.currency, out.gross, out.reversed, out.net = row.Currency, row.Gross, row.Reversed, row.Net
			}
		}
	}
	out.byCurrency = money
	return out, nil
}

// overviewRevenue reads the window's money and, for complete-day ranges, the
// previous window's net in the same headline currency. It returns the tile's
// metric plus the signed detail block.
//
// instrumented is the lifetime signal, not a window fact: it is true when the
// project has EVER delivered a money-shaped event (revenue or
// revenue_reversed, any platform, any window). A project that never has is
// "unconfigured" — the metric is defined but nothing was connected — while an
// instrumented project whose window holds no money row is "no_data". The
// distinction is what the tile renders: Set up versus No data.
func (s *Store) overviewRevenue(ctx context.Context, projectID string, r, prev OverviewRange, platform string, instrumented bool) (OverviewMetric, *OverviewRevenueDetail, error) {
	win, err := s.overviewMoney(ctx, projectID, r, platform)
	if err != nil {
		return OverviewMetric{}, nil, err
	}

	detail := &OverviewRevenueDetail{
		Window:             r,
		Currency:           win.currency,
		Gross:              win.gross,
		Reversed:           win.reversed,
		Net:                win.net,
		DedupedRows:        win.moneyRows,
		ExcludedRows:       win.excludedRows(),
		ExcludedCurrencies: nil,
		ByCurrency:         win.byCurrency,
	}
	if win.ltRows > 0 {
		detail.ExcludedCurrencies = []string{moneyNonCurrency}
	}
	// The previous-window scan is only worth running when there is a headline
	// currency to compare against: with no money row in this window the match
	// below can never fire (excluded groups are not in byCurrency).
	if r.CompleteDays && win.currency != "" {
		prior, err := s.overviewMoney(ctx, projectID, prev, platform)
		if err != nil {
			return OverviewMetric{}, nil, err
		}
		// Same currency, same window length, or the delta would compare two
		// different facts — and a currency with no row at all in the previous
		// window has no measured net to compare against.
		for _, row := range prior.byCurrency {
			if row.Currency == win.currency {
				net := row.Net
				detail.PreviousNet = &net
				break
			}
		}
	}

	metric := OverviewMetric{
		Definition: "Deduplicated net revenue per declared currency: rows de-duplicate by $insert_id (last write wins, event_id fallback), refunds and revenue_reversed rows net against bookings, and the headline is the currency with the largest deduplicated gross. No FX — currencies are never summed together.",
		Notes:      revenueNotes(win),
	}
	if win.moneyRows == 0 {
		if !instrumented {
			// Never a money-shaped event: the tile is Set up, and the first
			// note names the instrumentation that would change it — the
			// "nothing arrived in this range" note would be a false claim
			// about a project that has never sent one.
			metric.State = OverviewStateUnconfigured
			metric.Notes = append([]string{
				"requires a trusted, deduplicated server or billing source that sends " + moneyBookingEvent + " events with a declared currency and gross/net basis",
			}, metric.Notes[:len(metric.Notes)-1]...)
			return metric, detail, nil
		}
		metric.State = OverviewStateNoData
		return metric, detail, nil
	}
	metric.State = OverviewStateOK
	// Value is unsigned and shared by every tile, so a negative net cannot be
	// carried there; it stays visible in the signed detail and the note beside
	// it rather than being clamped to zero or hidden as "no data".
	if win.net >= 0 {
		metric.Value = uint64Ptr(uint64(win.net))
	}
	if detail.PreviousNet != nil && *detail.PreviousNet >= 0 {
		metric.Previous = uint64Ptr(uint64(*detail.PreviousNet))
	}
	return metric, detail, nil
}

// revenueNotes explains the number the tile is about to render: what the unit
// is, which currency heads the tile and why, what was excluded, and — when the
// net is negative — that this is a fact rather than a failure.
func revenueNotes(win overviewMoneyWindow) []string {
	notes := []string{
		"amounts are integers in each currency's smallest unit exactly as the sender declared them; no FX and no cross-currency total",
	}
	noCurrency := rowPhrase(win.noCurrency)
	lt := rowPhrase(win.ltRows)
	if win.moneyRows == 0 {
		switch {
		case win.noCurrency > 0 && win.ltRows > 0:
			notes = append(notes, noCurrency+" declared no currency and "+lt+" declared "+moneyNonCurrency+", so no money total was computed")
		case win.noCurrency > 0:
			notes = append(notes, noCurrency+" declared no currency and was excluded — a money event must name the unit it is in")
		case win.ltRows > 0:
			notes = append(notes, lt+" declared "+moneyNonCurrency+", a platform credit rather than money, and was excluded from every total (pre-cutover "+moneyNonCurrency+" history stays readable in run_sql)")
		default:
			notes = append(notes, "no deduplicated "+moneyBookingEvent+" or "+moneyReversalEvent+" row arrived in this range")
		}
		return notes
	}
	if win.noCurrency > 0 {
		notes = append(notes, noCurrency+" declared no currency and was excluded — a money event must name the unit it is in")
	}
	if win.ltRows > 0 {
		notes = append(notes, lt+" declared "+moneyNonCurrency+", a platform credit rather than money, and was excluded (pre-cutover "+moneyNonCurrency+" history stays readable in run_sql)")
	}
	if win.net < 0 {
		notes = append(notes, "reversals exceeded bookings in this window — the net is negative and shown signed, not clamped")
	}
	if win.currency != "" {
		notes = append(notes, win.currency+" heads the tile: it has the highest deduplicated gross in this range; every other declared currency is listed separately")
	}
	return notes
}

// rowPhrase renders "1 deduplicated row" / "3 deduplicated rows". Zero renders
// as "no deduplicated rows" so a note that counts nothing still reads.
func rowPhrase(n int64) string {
	switch n {
	case 0:
		return "no deduplicated rows"
	case 1:
		return "1 deduplicated row"
	default:
		return fmt.Sprintf("%d deduplicated rows", n)
	}
}

// MoneyPaidAtRecipe is the "when did this person become a paying customer"
// recipe, written against the store's own grid (resolved_events, and the
// stitched canonical_distinct_id the tile also reads), so a caller inside the
// store can run it with duckQuery and a bound window. It derives the milestone
// from the ledger instead of emitting a `paid_user` event, which would be a
// second fact able to disagree with the money it claims to describe:
//
//	a person is paid from their earliest de-duplicated POSITIVE booking onward.
//
// This is NOT the run_sql form: the sandbox accepts one readable source, spelled
// `events`, and rewrites it — a query reading `resolved_events` is rejected
// ("SQL must read from the events table exactly once"). The published form, for
// run_sql, the SQL page, a saved chart and an agent, is the same recipe written
// against `events` and `canonical_id` in docs/ANALYTICS.md § Reading money with
// SQL; TestMoneyPaidAtRecipeIsThePublishedOne pins that the two stay one recipe
// and TestPublishedMoneySQLRunsInTheSandbox pins that the published form is
// something run_sql actually accepts.
//
// Reversals do not erase the milestone (a refund is not a time machine) and
// neither does a later correction, because the grid resolves the correction
// before paid_at is derived. `where` is the window fragment; pass a range that
// covers the payer's whole history when you want a lifetime cohort.
func MoneyPaidAtRecipe(where string) string {
	return `WITH ` + moneyRowsCTE(where) + `
SELECT person_id, min(occurred_at) AS paid_at
FROM money_rows
WHERE write_rank = 1
  AND event_name = '` + moneyBookingEvent + `'
  AND NOT ` + moneyReverses + `
  AND amount > 0
  AND currency <> ''
  AND currency <> '` + moneyNonCurrency + `'
GROUP BY person_id
ORDER BY paid_at ASC, person_id ASC`
}
