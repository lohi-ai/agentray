package ingestion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func postCapture(t *testing.T, h Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	var err error
	switch path {
	case "/batch":
		err = h.Batch(c)
	default:
		err = h.Capture(c)
	}
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rec
}

// The standard money taxonomy travels over the transport that already existed:
// `revenue` / `revenue_reversed` are ordinary free-form names, the money facts
// ride in `properties`, and the dedup key is `$insert_id`. Nothing about the
// envelope is special-cased for money, which is what lets a billing provider
// emit the contract with a plain HTTP POST.
func TestRawHTTPMoneyEnvelopeRemainsAdditive(t *testing.T) {
	events := &captureEvents{}
	h := Handler{projects: fakeProjects{}, events: events}

	rec := postCapture(t, h, "/capture", `{
		"api_key": "good-key",
		"event": "revenue",
		"distinct_id": "payer-1",
		"properties": {
			"amount": 1900,
			"currency": "USD",
			"kind": "payment",
			"provider": "stripe",
			"plan": "pro",
			"$insert_id": "pay-1"
		}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture: %d %s", rec.Code, rec.Body.String())
	}
	if len(events.got) != 1 {
		t.Fatalf("expected one stored event, got %d", len(events.got))
	}
	ev := events.got[0]
	if ev.EventName != "revenue" {
		t.Errorf("event name = %q, want revenue (the name is the contract the read queries)", ev.EventName)
	}
	if ev.InsertID != "pay-1" {
		t.Errorf("insert_id = %q, want pay-1 (the key the read de-duplicates on)", ev.InsertID)
	}
	if ev.DistinctID != "payer-1" {
		t.Errorf("distinct_id = %q, want payer-1", ev.DistinctID)
	}
	var props map[string]any
	if err := json.Unmarshal([]byte(ev.Properties), &props); err != nil {
		t.Fatalf("properties not json: %v", err)
	}
	for key, want := range map[string]any{"amount": float64(1900), "currency": "USD", "kind": "payment", "provider": "stripe", "plan": "pro"} {
		if props[key] != want {
			t.Errorf("properties[%q] = %v, want %v", key, props[key], want)
		}
	}

	// A reversal is a second name, not a flag on the first, and it carries the
	// positive amount actually returned.
	rec = postCapture(t, h, "/capture", `{
		"api_key": "good-key",
		"event": "revenue_reversed",
		"distinct_id": "payer-1",
		"properties": {"amount": 300, "currency": "VND", "kind": "refund", "$insert_id": "refund-1"}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reversal capture: %d %s", rec.Code, rec.Body.String())
	}
	if got := events.got[1].EventName; got != "revenue_reversed" {
		t.Errorf("reversal name = %q", got)
	}
}

// Ingest stays permissive on purpose: it has no money validator, so a malformed
// or unconventional money row is stored rather than rejected. The Overview read
// is the single place that decides what counts as money (excluded currencies and
// missing-currency rows are surfaced there), and a customer whose older SDK
// emits a shape this ticket did not anticipate must not lose the event.
func TestRawHTTPMoneyIngestIsNotAValidationGate(t *testing.T) {
	events := &captureEvents{}
	h := Handler{projects: fakeProjects{}, events: events}

	for _, body := range []string{
		// No currency: the read counts it as an excluded row.
		`{"api_key":"good-key","event":"revenue","distinct_id":"p1","properties":{"amount":100}}`,
		// No $insert_id: the read de-duplicates it by event_id instead.
		`{"api_key":"good-key","event":"revenue","distinct_id":"p1","properties":{"amount":100,"currency":"USD","kind":"payment"}}`,
		// A customer-defined kind and a non-money unit the read reports as excluded.
		`{"api_key":"good-key","event":"revenue","distinct_id":"p1","properties":{"amount":5,"currency":"LT","kind":"topup","$insert_id":"lt-1"}}`,
	} {
		if rec := postCapture(t, h, "/capture", body); rec.Code != http.StatusOK {
			t.Fatalf("capture rejected a money row it must store: %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(events.got) != 3 {
		t.Fatalf("stored %d events, want 3", len(events.got))
	}
}

// A batch is how a billing backfill or a nightly settlement arrives: each entry
// carries its own name and key, and the batch's key authenticates them all.
func TestRawHTTPMoneyBatchKeepsPerRowKeys(t *testing.T) {
	events := &captureEvents{}
	h := Handler{projects: fakeProjects{}, events: events}

	rec := postCapture(t, h, "/batch", `{
		"api_key": "good-key",
		"batch": [
			{"event": "revenue", "distinct_id": "p1", "properties": {"amount": 100, "currency": "VND", "kind": "wallet_topup", "$insert_id": "topup:1"}},
			{"event": "revenue", "distinct_id": "p2", "properties": {"amount": 200, "currency": "VND", "kind": "donation", "$insert_id": "donation:9"}},
			{"event": "revenue_reversed", "distinct_id": "p1", "properties": {"amount": 100, "currency": "VND", "kind": "refund", "$insert_id": "refund:1"}}
		]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	if len(events.got) != 3 {
		t.Fatalf("stored %d events, want 3", len(events.got))
	}
	for i, want := range []string{"topup:1", "donation:9", "refund:1"} {
		if events.got[i].InsertID != want {
			t.Errorf("event %d insert_id = %q, want %q", i, events.got[i].InsertID, want)
		}
	}
}
