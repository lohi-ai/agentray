package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// money_test.go locks the sealed net-money contract (evidence 06): the eleven
// vectors, the compatibility the taxonomy must not narrow (custom kinds, a zero
// booking), the SQL reconciliation a customer runs by hand, and the derived
// paid_at recipe. Every case runs against a real DuckDB, because the contract
// lives in the SQL — a fake would only prove the Go around it.

// moneyProject is the project every case here writes to. Each case gets its own
// DuckDB, so one id is enough.
const moneyProject = "11111111-2222-3333-4444-555555555555"

// The fixture windows: all of September 2026, with August as its previous range.
var (
	moneyInWindow   = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	moneyPrevWindow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
)

func moneyRanges() (OverviewRange, OverviewRange) {
	cur := OverviewRange{
		From:         time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:           time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Days:         30,
		CompleteDays: true,
	}
	prev := OverviewRange{From: cur.From.AddDate(0, 0, -30), To: cur.From, Days: 30, CompleteDays: true}
	return cur, prev
}

// moneySeed is one event to write. An empty at falls back to the middle of the
// window; an empty eventID is minted, because the table's primary key is
// (project_id, event_id) and the read falls back to it for unkeyed rows.
type moneySeed struct {
	event    string
	person   string
	at       time.Time
	amount   int64
	currency string
	kind     string
	insertID string
	platform string
	eventID  string
}

func moneyEvent(t *testing.T, s moneySeed) Event {
	t.Helper()
	at := s.at
	if at.IsZero() {
		at = moneyInWindow
	}
	eventID := s.eventID
	if eventID == "" {
		eventID = uuid.NewString()
	}
	person := s.person
	if person == "" {
		person = "payer"
	}
	name := s.event
	if name == "" {
		name = "revenue"
	}
	props := map[string]any{"amount": s.amount}
	if s.currency != "" {
		props["currency"] = s.currency
	}
	if s.kind != "" {
		props["kind"] = s.kind
	}
	encoded, err := json.Marshal(props)
	if err != nil {
		t.Fatalf("props: %v", err)
	}
	return Event{
		ProjectID:    moneyProject,
		EventID:      eventID,
		EventName:    name,
		EventType:    "user",
		DistinctID:   person,
		Properties:   string(encoded),
		InsertID:     s.insertID,
		Platform:     s.platform,
		VisitorClass: "human",
		Timestamp:    at,
	}
}

func sinkSeeds(t *testing.T, d *DuckDB, seeds ...moneySeed) {
	t.Helper()
	events := make([]Event, 0, len(seeds))
	for _, seed := range seeds {
		events = append(events, moneyEvent(t, seed))
	}
	if err := d.SinkEvents(context.Background(), events); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
}

func moneyStore(t *testing.T, seeds ...moneySeed) *Store {
	t.Helper()
	d := openTestDuckDB(t)
	sinkSeeds(t, d, seeds...)
	return &Store{duck: d}
}

func readMoney(t *testing.T, s *Store, cur, prev OverviewRange, platform string) (OverviewMetric, *OverviewRevenueDetail) {
	t.Helper()
	metric, detail, err := s.overviewRevenue(context.Background(), moneyProject, cur, prev, platform)
	if err != nil {
		t.Fatalf("overviewRevenue: %v", err)
	}
	return metric, detail
}

// moneyWant is the whole observable read: the tile's metric and the signed
// detail block. Vector expectations are written out in full rather than
// spot-checked, because a wrong total and a wrong breakdown are two different
// bugs and both would be invisible to a test that only checked one.
type moneyWant struct {
	state        string
	value        *uint64
	previous     *uint64
	currency     string
	gross        int64
	reversed     int64
	net          int64
	previousNet  *int64
	dedupedRows  int64
	excludedRows int64
	excluded     []string
	byCurrency   []OverviewRevenueCurrency
	notesContain []string
}

func int64Ptr(v int64) *int64 { return &v }

func checkMoney(t *testing.T, got OverviewMetric, detail *OverviewRevenueDetail, want moneyWant) {
	t.Helper()
	if got.State != want.state {
		t.Errorf("state = %q, want %q", got.State, want.state)
	}
	if !reflect.DeepEqual(got.Value, want.value) {
		t.Errorf("value = %v, want %v", show(got.Value), show(want.value))
	}
	if !reflect.DeepEqual(got.Previous, want.previous) {
		t.Errorf("previous = %v, want %v", show(got.Previous), show(want.previous))
	}
	if detail == nil {
		t.Fatal("revenue_detail is nil — the tile cannot render a signed net or the breakdown without it")
	}
	if detail.Currency != want.currency {
		t.Errorf("headline currency = %q, want %q", detail.Currency, want.currency)
	}
	if detail.Gross != want.gross || detail.Reversed != want.reversed || detail.Net != want.net {
		t.Errorf("gross/reversed/net = %d/%d/%d, want %d/%d/%d",
			detail.Gross, detail.Reversed, detail.Net, want.gross, want.reversed, want.net)
	}
	if !reflect.DeepEqual(detail.PreviousNet, want.previousNet) {
		t.Errorf("previous_net = %v, want %v", show(detail.PreviousNet), show(want.previousNet))
	}
	if detail.DedupedRows != want.dedupedRows || detail.ExcludedRows != want.excludedRows {
		t.Errorf("deduped/excluded rows = %d/%d, want %d/%d",
			detail.DedupedRows, detail.ExcludedRows, want.dedupedRows, want.excludedRows)
	}
	if !reflect.DeepEqual(detail.ExcludedCurrencies, want.excluded) {
		t.Errorf("excluded_currencies = %v, want %v", detail.ExcludedCurrencies, want.excluded)
	}
	if want.byCurrency != nil && !reflect.DeepEqual(detail.ByCurrency, want.byCurrency) {
		t.Errorf("by_currency = %+v, want %+v", detail.ByCurrency, want.byCurrency)
	}
	if detail.ByCurrency == nil {
		t.Error("by_currency must serialize as [] so the tile can render an empty table, not null")
	}
	for _, want := range want.notesContain {
		if !containsNote(got.Notes, want) {
			t.Errorf("notes %q do not mention %q", got.Notes, want)
		}
	}
}

func show[T any](p *T) any {
	if p == nil {
		return "unset"
	}
	return *p
}

func containsNote(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}

// The eleven sealed vectors. Each is a fact the tile must report honestly; the
// table is the acceptance for the read half of the ticket.
func TestOverviewRevenueContractVectors(t *testing.T) {
	cur, prev := moneyRanges()

	for _, tc := range []struct {
		name  string
		seeds []moneySeed
		plat  string
		want  moneyWant
	}{
		{
			name: "1: two rows sharing a key are one booking",
			seeds: []moneySeed{
				{amount: 100, currency: "VND", insertID: "pay-1"},
				{amount: 100, currency: "VND", insertID: "pay-1", at: moneyInWindow.Add(time.Minute)},
			},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(100), currency: "VND", gross: 100, net: 100, dedupedRows: 1},
		},
		{
			name: "2: the last write wins, so a correction replaces the value it fixes",
			seeds: []moneySeed{
				{amount: 100, currency: "VND", insertID: "pay-1"},
				{amount: 80, currency: "VND", insertID: "pay-1", at: moneyInWindow.Add(time.Hour)},
			},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(80), currency: "VND", gross: 80, net: 80, dedupedRows: 1},
		},
		{
			name: "3: a reversal nets against the booking",
			seeds: []moneySeed{
				{amount: 100, currency: "VND", insertID: "pay-1"},
				{event: "revenue_reversed", amount: 30, currency: "VND", kind: "refund", insertID: "refund-1"},
			},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(70), currency: "VND", gross: 100, reversed: 30, net: 70, dedupedRows: 2},
		},
		{
			name:  "4: a reversal with no booking is a negative net, not an error",
			seeds: []moneySeed{{event: "revenue_reversed", amount: 30, currency: "VND", kind: "refund", insertID: "refund-1"}},
			want: moneyWant{state: OverviewStateOK, currency: "VND", reversed: 30, net: -30, dedupedRows: 1,
				notesContain: []string{"reversals exceeded bookings"}},
		},
		{
			name: "5: LT is a platform credit, excluded and named",
			seeds: []moneySeed{
				{amount: 5000000, currency: "LT", kind: "topup", insertID: "lt-1"},
				{amount: 90000, currency: "VND", kind: "wallet_topup", insertID: "pay-1"},
			},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(90000), currency: "VND", gross: 90000, net: 90000,
				dedupedRows: 1, excludedRows: 1, excluded: []string{"LT"},
				byCurrency:   []OverviewRevenueCurrency{{Currency: "VND", Gross: 90000, Net: 90000, Rows: 1}},
				notesContain: []string{"platform credit rather than money"}},
		},
		{
			name: "6: an unkeyed row falls back to its event_id, so distinct facts count twice",
			seeds: []moneySeed{
				{amount: 100, currency: "VND"},
				{amount: 100, currency: "VND"},
			},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(200), currency: "VND", gross: 200, net: 200, dedupedRows: 2},
		},
		{
			name:  "7: the headline is the dominant gross, and currencies never combine",
			seeds: []moneySeed{{amount: 100, currency: "USD", insertID: "usd-1"}, {amount: 90000, currency: "VND", insertID: "vnd-1"}},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(90000), currency: "VND", gross: 90000, net: 90000, dedupedRows: 2,
				byCurrency: []OverviewRevenueCurrency{
					{Currency: "VND", Gross: 90000, Net: 90000, Rows: 1},
					{Currency: "USD", Gross: 100, Net: 100, Rows: 1},
				},
				notesContain: []string{"heads the tile"}},
		},
		{
			name: "8: the platform filter still applies to money rows",
			seeds: []moneySeed{
				{amount: 100, currency: "VND", insertID: "web-1", platform: "web"},
				{amount: 500, currency: "VND", insertID: "server-1", platform: "server"},
			},
			plat: "web",
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(100), currency: "VND", gross: 100, net: 100, dedupedRows: 1},
		},
		{
			name: "9: a legacy negative refund booking reverses",
			seeds: []moneySeed{
				{amount: 100, currency: "VND", kind: "payment", insertID: "pay-1"},
				{amount: -30, currency: "VND", kind: "refund", insertID: "pay-1-refund"},
			},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(70), currency: "VND", gross: 100, reversed: 30, net: 70, dedupedRows: 2},
		},
		{
			name: "10: the previous window compares the same currency",
			seeds: []moneySeed{
				{amount: 70, currency: "VND", insertID: "now-1"},
				{amount: 50, currency: "VND", insertID: "then-1", at: moneyPrevWindow},
			},
			want: moneyWant{state: OverviewStateOK, value: uint64Ptr(70), previous: uint64Ptr(50), currency: "VND",
				gross: 70, net: 70, previousNet: int64Ptr(50), dedupedRows: 1},
		},
		{
			name:  "11: no money row at all is no_data, never a fabricated zero",
			seeds: []moneySeed{{event: "user.pageview", amount: 0}},
			want: moneyWant{state: OverviewStateNoData,
				notesContain: []string{"no deduplicated revenue"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := moneyStore(t, tc.seeds...)
			metric, detail := readMoney(t, s, cur, prev, tc.plat)
			checkMoney(t, metric, detail, tc.want)
		})
	}
}

// A redelivery of an identical body is the same row: the event_id is the
// primary key, and the read falls back to it when a producer sent no key, so a
// replayed batch cannot book twice.
func TestOverviewRevenueIdenticalBodyIsOneRow(t *testing.T) {
	cur, prev := moneyRanges()
	eventID := uuid.NewString()
	s := moneyStore(t,
		moneySeed{amount: 100, currency: "VND", eventID: eventID},
		moneySeed{amount: 100, currency: "VND", eventID: eventID},
	)
	metric, detail := readMoney(t, s, cur, prev, "")

	if detail.DedupedRows != 1 || detail.Gross != 100 {
		t.Fatalf("deduped rows/gross = %d/%d, want 1/100 — a redelivered body is one fact", detail.DedupedRows, detail.Gross)
	}
	if metric.Value == nil || *metric.Value != 100 {
		t.Fatalf("value = %v, want 100", show(metric.Value))
	}
}

// The taxonomy is documentation, not a filter. A customer whose money rides on
// its own kinds — or who books a zero-amount row while settling a free grant —
// must keep reading a real number, because narrowing this read would silently
// zero the tile for data that is already in the store.
func TestOverviewRevenueCompatibilityKindsAndZero(t *testing.T) {
	cur, prev := moneyRanges()
	s := moneyStore(t,
		moneySeed{amount: 100, currency: "VND", kind: "one_time", insertID: "a"},
		moneySeed{amount: 200, currency: "VND", kind: "renewal", insertID: "b"},
		moneySeed{amount: 300, currency: "VND", kind: "totally_custom", insertID: "c"},
		moneySeed{amount: 400, currency: "VND", kind: "", insertID: "d"},
		moneySeed{amount: 0, currency: "VND", kind: "payment", insertID: "e"},
	)
	metric, detail := readMoney(t, s, cur, prev, "")

	if metric.State != OverviewStateOK || detail.Gross != 1000 || detail.Net != 1000 {
		t.Fatalf("custom kinds or a zero booking were dropped: state=%s gross=%d net=%d", metric.State, detail.Gross, detail.Net)
	}
	if detail.DedupedRows != 5 {
		t.Fatalf("deduped rows = %d, want 5 (every currency-bearing row books)", detail.DedupedRows)
	}
}

// A zero booking on its own is money: the tile must read 0, not No data. That
// is the difference between "nobody paid" and "we cannot see payments", and the
// UI renders them differently.
func TestOverviewRevenueZeroBookingIsOK(t *testing.T) {
	cur, prev := moneyRanges()
	s := moneyStore(t, moneySeed{amount: 0, currency: "VND", kind: "payment", insertID: "free-1"})
	metric, detail := readMoney(t, s, cur, prev, "")

	if metric.State != OverviewStateOK {
		t.Fatalf("state = %q, want ok", metric.State)
	}
	if metric.Value == nil || *metric.Value != 0 {
		t.Fatalf("value = %v, want a measured 0", show(metric.Value))
	}
	if detail.Net != 0 || detail.Currency != "VND" {
		t.Fatalf("detail = %+v, want VND net 0", detail)
	}
}

// A partial ("today") range has no comparable previous window, so it carries no
// comparison at all rather than a delta against a partial day.
func TestOverviewRevenueTodayHasNoPreviousWindow(t *testing.T) {
	cur, prev := moneyRanges()
	s := moneyStore(t,
		moneySeed{amount: 70, currency: "VND", insertID: "now-1"},
		moneySeed{amount: 50, currency: "VND", insertID: "then-1", at: moneyPrevWindow},
	)
	partial := cur
	partial.CompleteDays = false
	metric, detail := readMoney(t, s, partial, prev, "")

	if detail.PreviousNet != nil || metric.Previous != nil {
		t.Fatalf("a partial range must carry no comparison: previous=%v previous_net=%v",
			show(metric.Previous), show(detail.PreviousNet))
	}
	if metric.Value == nil || *metric.Value != 70 {
		t.Fatalf("value = %v, want 70", show(metric.Value))
	}
}

// The comparison is per currency: a previous window that held only USD has no
// VND net to compare against, and an absent comparison is a different fact from
// a measured zero.
func TestOverviewRevenuePreviousNetIsPerCurrency(t *testing.T) {
	cur, prev := moneyRanges()
	s := moneyStore(t,
		moneySeed{amount: 90000, currency: "VND", insertID: "vnd-1"},
		moneySeed{amount: 5000, currency: "USD", insertID: "usd-1", at: moneyPrevWindow},
	)
	_, detail := readMoney(t, s, cur, prev, "")

	if detail.Currency != "VND" {
		t.Fatalf("headline = %q, want VND", detail.Currency)
	}
	if detail.PreviousNet != nil {
		t.Fatalf("previous_net = %d, want unset: the previous window held no VND row", *detail.PreviousNet)
	}
}

// A negative previous net cannot be carried by the unsigned metric, but the
// signed detail keeps it, so the tile can still show a fall from a reversal.
func TestOverviewRevenueNegativePreviousNetStaysSigned(t *testing.T) {
	cur, prev := moneyRanges()
	s := moneyStore(t,
		moneySeed{amount: 70, currency: "VND", insertID: "now-1"},
		moneySeed{event: "revenue_reversed", amount: 50, currency: "VND", kind: "refund", insertID: "then-1", at: moneyPrevWindow},
	)
	metric, detail := readMoney(t, s, cur, prev, "")

	if detail.PreviousNet == nil || *detail.PreviousNet != -50 {
		t.Fatalf("previous_net = %v, want -50", show(detail.PreviousNet))
	}
	if metric.Previous != nil {
		t.Fatalf("metric.previous = %v, want unset (the field is unsigned)", show(metric.Previous))
	}
}

// The reconciliation a customer runs by hand: the exact dedup CTE from the
// sealed contract, executed over the raw events table, must equal BOTH the tile
// and the detail block — and the naive sum it replaces must disagree, or the
// dedup layer would not be worth having.
func TestOverviewRevenueSQLReconciles(t *testing.T) {
	cur, prev := moneyRanges()
	s := moneyStore(t,
		// Two writes of one booking (100000, then corrected to 80000), one
		// refund, one USD booking, and legacy LT credit.
		moneySeed{amount: 100000, currency: "VND", kind: "wallet_topup", insertID: "pay-1"},
		moneySeed{amount: 80000, currency: "VND", kind: "wallet_topup", insertID: "pay-1", at: moneyInWindow.Add(time.Hour)},
		moneySeed{event: "revenue_reversed", amount: 30000, currency: "VND", kind: "refund", insertID: "refund-1"},
		moneySeed{amount: 100, currency: "USD", kind: "payment", insertID: "usd-1"},
		moneySeed{amount: 5000000, currency: "LT", kind: "topup", insertID: "lt-1"},
	)
	metric, detail := readMoney(t, s, cur, prev, "")

	if metric.Value == nil || *metric.Value != 50000 {
		t.Fatalf("tile value = %v, want 50000", show(metric.Value))
	}
	if detail.Gross != 80000 || detail.Reversed != 30000 || detail.Net != 50000 {
		t.Fatalf("detail = %d/%d/%d, want 80000/30000/50000", detail.Gross, detail.Reversed, detail.Net)
	}
	if !reflect.DeepEqual(detail.ByCurrency, []OverviewRevenueCurrency{
		{Currency: "VND", Gross: 80000, Reversed: 30000, Net: 50000, Rows: 2},
		{Currency: "USD", Gross: 100, Net: 100, Rows: 1},
	}) {
		t.Fatalf("by_currency = %+v, want VND ahead of USD with the refund netted", detail.ByCurrency)
	}

	// The sealed control, verbatim except for two mechanical changes: the
	// project is bound rather than inlined, and `event_id` carries a VARCHAR
	// cast because the column is typed UUID and COALESCE cannot mix types.
	// Neither changes which row wins.
	var currency string
	var net int64
	if err := s.duckQueryRow(context.Background(), `
WITH ranked AS (
  SELECT event_id, event_name, "timestamp",
    upper(trim(json_extract_string(properties, '$.currency'))) AS currency,
    try_cast(json_extract_string(properties, '$.amount') AS BIGINT) AS amount,
    lower(trim(json_extract_string(properties, '$.kind'))) AS kind,
    row_number() OVER (
      PARTITION BY coalesce(nullif(insert_id, ''), CAST(event_id AS VARCHAR))
      ORDER BY "timestamp" DESC, event_id DESC
    ) AS rn
  FROM events
  WHERE event_name IN ('revenue', 'revenue_reversed')
    AND project_id = ?
    AND "timestamp" >= TIMESTAMPTZ '2026-09-01T00:00:00Z'
    AND "timestamp" <  TIMESTAMPTZ '2026-10-01T00:00:00Z'
)
SELECT currency,
  CAST(sum(CASE
    WHEN event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0 THEN -abs(amount)
    ELSE amount
  END) AS BIGINT) AS net
FROM ranked
WHERE rn = 1 AND currency = 'VND'
GROUP BY currency`, []any{moneyProject}, &currency, &net); err != nil {
		t.Fatalf("sealed reconciliation query: %v", err)
	}
	if currency != "VND" || net != 50000 {
		t.Fatalf("hand-written SQL = %s %d, want VND 50000 — the published recipe and the tile must agree", currency, net)
	}

	// The failure the dedup prevents: a plain sum over the same rows counts the
	// corrected booking twice.
	var naive int64
	if err := s.duckQueryRow(context.Background(), `
SELECT CAST(sum(try_cast(json_extract_string(properties, '$.amount') AS BIGINT)) AS BIGINT)
FROM events
WHERE project_id = ? AND event_name = 'revenue'
  AND upper(trim(json_extract_string(properties, '$.currency'))) = 'VND'
  AND "timestamp" >= TIMESTAMPTZ '2026-09-01T00:00:00Z'
  AND "timestamp" <  TIMESTAMPTZ '2026-10-01T00:00:00Z'`, []any{moneyProject}, &naive); err != nil {
		t.Fatalf("naive control: %v", err)
	}
	if naive != 180000 {
		t.Fatalf("naive control = %d, want 180000 (the double count the dedup removes)", naive)
	}
}

// paid_at is DERIVED from the ledger, never emitted as a `paid_user` event: a
// person is paid from their earliest de-duplicated POSITIVE booking on their
// canonical id, and a refund does not erase the milestone. This runs the
// published recipe, so the SQL an agent or a cohort uses cannot drift from the
// semantics the tile applies.
func TestPaidAtUsesDeduplicatedMoneyBookings(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}

	// One person read as two ids, so "became paid" cannot fork one human in two.
	if err := d.UpsertAliases(context.Background(), [][3]string{{moneyProject, "anon-2", "payer-2"}}); err != nil {
		t.Fatalf("UpsertAliases: %v", err)
	}
	correctedAt := moneyInWindow.Add(5 * 24 * time.Hour)
	sinkSeeds(t, d,
		// payer-1 booked at T-0, then corrected the same booking five days
		// later: the correction is the surviving row, so its timestamp is the
		// milestone. A later, separate booking must not move it forward.
		moneySeed{person: "payer-1", amount: 100, currency: "VND", insertID: "pay-1"},
		moneySeed{person: "payer-1", amount: 80, currency: "VND", insertID: "pay-1", at: correctedAt},
		moneySeed{person: "payer-1", amount: 900, currency: "VND", insertID: "pay-2", at: moneyInWindow.Add(20 * 24 * time.Hour)},
		// payer-2 paid under the anonymous id only.
		moneySeed{person: "anon-2", amount: 50, currency: "USD", insertID: "pay-anon", at: moneyInWindow.Add(2 * 24 * time.Hour)},
		// Everyone below declared money but never paid: a reversal, a zero
		// booking, a refund-kind booking, LT credit, and a row with no currency.
		moneySeed{person: "refunded", event: "revenue_reversed", amount: 30, currency: "VND", kind: "refund", insertID: "r-1"},
		moneySeed{person: "zero", amount: 0, currency: "VND", kind: "payment", insertID: "z-1"},
		moneySeed{person: "refundkind", amount: 30, currency: "VND", kind: "refund", insertID: "rk-1"},
		moneySeed{person: "credit", amount: 5000, currency: "LT", kind: "topup", insertID: "lt-1"},
		moneySeed{person: "nounit", amount: 700},
	)

	where, args := overviewWindowWhere(moneyProject, OverviewRange{
		From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}, "")
	paid := map[string]time.Time{}
	err := s.duckQuery(context.Background(), MoneyPaidAtRecipe(where), args, func(rows *sql.Rows) error {
		var person string
		var paidAt time.Time
		if err := rows.Scan(&person, &paidAt); err != nil {
			return err
		}
		paid[person] = paidAt
		return nil
	})
	if err != nil {
		t.Fatalf("paid_at recipe: %v", err)
	}

	if len(paid) != 2 {
		t.Fatalf("paid_at named %d people, want 2: only a de-duplicated positive booking in a money currency makes someone paid (%v)", len(paid), paid)
	}
	if got := paid["payer-1"]; !got.Equal(correctedAt) {
		t.Errorf("payer-1 paid_at = %v, want %v: the surviving correction is the milestone, and the later booking must not move it", got, correctedAt)
	}
	if got := paid["payer-2"]; !got.Equal(moneyInWindow.Add(2 * 24 * time.Hour)) {
		t.Errorf("payer-2 paid_at = %v, want %v: the anonymous id's booking belongs to its canonical person",
			got, moneyInWindow.Add(2*24*time.Hour))
	}
}

// The undocumented-in-code half of the recipe: the SQL published in
// docs/ANALYTICS.md must be the one this package executes. A doc that drifts
// from the read is how two money numbers start disagreeing in public.
func TestMoneyPaidAtRecipeIsThePublishedOne(t *testing.T) {
	recipe := MoneyPaidAtRecipe(`project_id = '…' AND "timestamp" >= TIMESTAMPTZ '…' AND "timestamp" < TIMESTAMPTZ '…'`)
	for _, fragment := range []string{
		"money_rows",
		"row_number() OVER (PARTITION BY row_key ORDER BY occurred_at DESC, event_id DESC)",
		"event_name = 'revenue'",
		"NOT (event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0)",
		"amount > 0",
		"currency <> 'LT'",
		"min(occurred_at) AS paid_at",
	} {
		if !strings.Contains(recipe, fragment) {
			t.Errorf("the paid_at recipe lost %q", fragment)
		}
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "ANALYTICS.md"))
	if err != nil {
		t.Fatalf("read docs/ANALYTICS.md: %v", err)
	}
	for _, fragment := range []string{
		"row_number() OVER (PARTITION BY row_key ORDER BY occurred_at DESC, event_id DESC)",
		"NOT (event_name = 'revenue_reversed' OR kind = 'refund' OR amount < 0)",
		"min(occurred_at) AS paid_at",
	} {
		if !strings.Contains(string(doc), fragment) {
			t.Errorf("docs/ANALYTICS.md does not publish the recipe fragment %q", fragment)
		}
	}
}
