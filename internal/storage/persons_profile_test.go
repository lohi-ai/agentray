package storage

import (
	"encoding/json"
	"testing"
	"time"
)

func evAt(project, distinct string, ts time.Time, props string) Event {
	return Event{ProjectID: project, DistinctID: distinct, Timestamp: ts, Properties: props}
}

const proj = "11111111-1111-1111-1111-111111111111"

// $set is last-write-wins and $set_once is first-write-wins, resolved by event
// timestamp even when the batch is out of order.
func TestExtractPersonDeltasResolvesByTimestamp(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	// Deliberately out of order: newer event first.
	events := []Event{
		evAt(proj, "u1", t1, `{"$set":{"plan":"pro"},"$set_once":{"signup":"2026-07-01"}}`),
		evAt(proj, "u1", t0, `{"$set":{"plan":"free","lang":"vi"},"$set_once":{"signup":"2026-06-30"}}`),
	}
	d := extractPersonDeltas(events, nil)[personKey{proj, "u1"}]
	if d == nil {
		t.Fatal("want a delta")
	}
	if got := string(d.setProps["plan"]); got != `"pro"` {
		t.Fatalf("$set plan: want newest \"pro\", got %s", got)
	}
	if got := string(d.setProps["lang"]); got != `"vi"` {
		t.Fatalf("$set lang: want \"vi\", got %s", got)
	}
	if got := string(d.onceProps["signup"]); got != `"2026-06-30"` {
		t.Fatalf("$set_once signup: want earliest 2026-06-30, got %s", got)
	}
	if !d.firstSeen.Equal(t0) || !d.lastSeen.Equal(t1) {
		t.Fatalf("seen window: got [%v,%v]", d.firstSeen, d.lastSeen)
	}
}

// Events with no traits are ignored entirely.
func TestExtractPersonDeltasSkipsTraitlessEvents(t *testing.T) {
	ts := time.Now().UTC()
	d := extractPersonDeltas([]Event{
		evAt(proj, "u1", ts, `{"path":"/home"}`),
		evAt(proj, "", ts, `{"$set":{"x":1}}`), // no distinct id
	}, nil)
	if len(d) != 0 {
		t.Fatalf("want no deltas, got %d", len(d))
	}
}

// email/name are picked up from both top-level and $set.
func TestExtractPersonDeltasEmailName(t *testing.T) {
	ts := time.Now().UTC()
	d := extractPersonDeltas([]Event{
		evAt(proj, "u1", ts, `{"email":"a@b.com","$set":{"name":"Ann"}}`),
	}, nil)[personKey{proj, "u1"}]
	if d.email != "a@b.com" || d.name != "Ann" {
		t.Fatalf("email/name: got %q / %q", d.email, d.name)
	}
}

// Merging a delta onto an existing profile overlays $set, preserves existing
// $set_once, and widens the seen window.
func TestMergePersonDeltaOntoExisting(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(48 * time.Hour)
	existing := &personRow{
		SetProps:  map[string]json.RawMessage{"plan": json.RawMessage(`"free"`), "lang": json.RawMessage(`"vi"`)},
		OnceProps: map[string]json.RawMessage{"signup": json.RawMessage(`"2026-06-01"`)},
		Email:     "old@b.com",
		FirstSeen: t0,
		LastSeen:  t0,
	}
	d := &personDelta{
		setProps:  map[string]json.RawMessage{"plan": json.RawMessage(`"pro"`)},
		onceProps: map[string]json.RawMessage{"signup": json.RawMessage(`"2026-07-01"`), "ref": json.RawMessage(`"ad"`)},
		email:     "new@b.com",
		firstSeen: t1,
		lastSeen:  t1,
	}
	got := mergePersonDelta(existing, d)
	if string(got.SetProps["plan"]) != `"pro"` {
		t.Fatalf("$set overlay failed: %s", got.SetProps["plan"])
	}
	if string(got.SetProps["lang"]) != `"vi"` {
		t.Fatal("existing $set key lost")
	}
	if string(got.OnceProps["signup"]) != `"2026-06-01"` {
		t.Fatalf("$set_once must not be overwritten, got %s", got.OnceProps["signup"])
	}
	if string(got.OnceProps["ref"]) != `"ad"` {
		t.Fatal("new $set_once key not added")
	}
	if got.Email != "new@b.com" {
		t.Fatalf("email should update, got %s", got.Email)
	}
	if !got.FirstSeen.Equal(t0) || !got.LastSeen.Equal(t1) {
		t.Fatalf("seen window: got [%v,%v]", got.FirstSeen, got.LastSeen)
	}
}

// A resolve func folds an anonymous id and its canonical id into one delta, so
// pre-alias traits and post-login traits build a single profile (finding [1]).
func TestExtractPersonDeltasFoldsCanonical(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	resolve := func(_, distinct string) string {
		if distinct == "anon1" {
			return "user1"
		}
		return distinct
	}
	events := []Event{
		evAt(proj, "anon1", t0, `{"$set":{"lang":"vi"},"$set_once":{"first_touch":"ad"}}`),
		evAt(proj, "user1", t1, `{"$set":{"plan":"pro"}}`),
	}
	deltas := extractPersonDeltas(events, resolve)
	if _, orphan := deltas[personKey{proj, "anon1"}]; orphan {
		t.Fatal("anon id must not remain its own profile after stitching")
	}
	d := deltas[personKey{proj, "user1"}]
	if d == nil {
		t.Fatal("want a canonical delta")
	}
	if string(d.setProps["lang"]) != `"vi"` || string(d.setProps["plan"]) != `"pro"` {
		t.Fatalf("traits not folded onto canonical id: %v", d.setProps)
	}
	if string(d.onceProps["first_touch"]) != `"ad"` {
		t.Fatalf("anon $set_once lost: %v", d.onceProps)
	}
}

// An out-of-order (redelivered) older batch must not stomp a fresher stored $set
// value, but must still contribute brand-new keys (finding [3]).
func TestMergePersonDeltaRejectsStaleOverwrite(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)
	// Stored profile already reflects the newer batch (plan=pro at t1).
	existing := &personRow{
		SetProps:  map[string]json.RawMessage{"plan": json.RawMessage(`"pro"`)},
		OnceProps: map[string]json.RawMessage{},
		FirstSeen: t0,
		LastSeen:  t1,
	}
	// A late older batch (t0) carrying the stale plan=free plus a genuinely new key.
	d := &personDelta{
		setProps:  map[string]json.RawMessage{"plan": json.RawMessage(`"free"`), "lang": json.RawMessage(`"vi"`)},
		onceProps: map[string]json.RawMessage{},
		setTS:     map[string]time.Time{"plan": t0, "lang": t0},
		firstSeen: t0,
		lastSeen:  t0,
	}
	got := mergePersonDelta(existing, d)
	if string(got.SetProps["plan"]) != `"pro"` {
		t.Fatalf("stale older $set overwrote fresher value: %s", got.SetProps["plan"])
	}
	if string(got.SetProps["lang"]) != `"vi"` {
		t.Fatalf("new key from older batch should still be added: %v", got.SetProps)
	}
}

// Merging onto no existing profile just materialises the delta.
func TestMergePersonDeltaFresh(t *testing.T) {
	ts := time.Now().UTC()
	d := &personDelta{
		setProps:  map[string]json.RawMessage{"plan": json.RawMessage(`"pro"`)},
		onceProps: map[string]json.RawMessage{},
		firstSeen: ts,
		lastSeen:  ts,
	}
	got := mergePersonDelta(nil, d)
	if string(got.SetProps["plan"]) != `"pro"` || !got.FirstSeen.Equal(ts) {
		t.Fatalf("fresh merge wrong: %+v", got)
	}
}

func TestTraitMapRoundTrip(t *testing.T) {
	if marshalTraitMap(nil) != "{}" {
		t.Fatal("nil map should marshal to {}")
	}
	m := parseTraitMap(`{"plan":"pro","n":3}`)
	if string(m["plan"]) != `"pro"` || string(m["n"]) != `3` {
		t.Fatalf("round trip failed: %v", m)
	}
	if len(parseTraitMap("")) != 0 || len(parseTraitMap("{}")) != 0 {
		t.Fatal("empty inputs should parse to empty map")
	}
}
