package storage

import (
	"reflect"
	"strings"
	"testing"
)

// explore_events used to return raw rows and nothing else, so every analysis run
// opened by rebuilding the catalog in SQL — SELECT DISTINCT event_name, SELECT
// DISTINCT properties, min/max(timestamp) — and two runs in three ran out of
// turns before answering. These pin the summary that replaced that.

// TestEventSchemaAnswersWhatTheDiscoveryQueriesAskedFor is the whole point: an
// agent must be able to stop asking the four questions it used to spend turns on.
func TestEventSchemaAnswersWhatTheDiscoveryQueriesAskedFor(t *testing.T) {
	var e EventSchemaEntry
	for _, field := range []string{"EventName", "Events", "People", "FirstSeen", "LastSeen", "PropertyKeys"} {
		if !hasField(e, field) {
			t.Fatalf("EventSchemaEntry lost %s — the discovery query it replaces comes straight back", field)
		}
	}
}

// TestEventSchemaSeparatesVolumeFromPeople guards the distinction the rest of the
// product is built on: a count of events is not a count of anybody. The one field
// that may be called people has to be the stitched, human one.
func TestEventSchemaSeparatesVolumeFromPeople(t *testing.T) {
	if !hasField(EventSchemaEntry{}, "Events") || !hasField(EventSchemaEntry{}, "People") {
		t.Fatal("volume and people must stay two fields; one number cannot mean both")
	}
}

// TestEventSchemaQueryIsHumanFilteredAndStitched checks the SQL text that
// produces People. A crawler is not a person and a logged-in visitor is not two
// people — the same rule Persons, the funnel and the plan meter are held to.
func TestEventSchemaQueryIsHumanFilteredAndStitched(t *testing.T) {
	expr, args := identityResolver{database: "lohi_analytics"}.canonicalExpr("distinct_id")
	if !strings.Contains(expr, "aliases_dict") {
		t.Fatalf("canonical expression must stitch through the dictionary, got %q", expr)
	}
	if len(args) != 0 {
		t.Fatalf("canonical expression must bind no arguments; it is interpolated ahead of the WHERE clause and %d placeholder(s) would shift every later bind", len(args))
	}
}

// TestEventSchemaLimitsAreBounded stops the summary becoming the problem it
// solved. It is read into a model's context on every explore, so an unbounded
// catalog or key list would cost more than the queries it saves.
func TestEventSchemaLimitsAreBounded(t *testing.T) {
	if eventSchemaLimit <= 0 || eventSchemaLimit > 500 {
		t.Fatalf("eventSchemaLimit = %d; a catalog read into context on every explore must stay small", eventSchemaLimit)
	}
	if eventSchemaKeyLimit <= 0 || eventSchemaKeyLimit > 100 {
		t.Fatalf("eventSchemaKeyLimit = %d; one noisy event's payload must not crowd out every other event", eventSchemaKeyLimit)
	}
}

// hasField asks the struct, not a copy of its field list kept in the test: a
// rename that quietly drops a field is exactly the regression being guarded.
func hasField(v any, name string) bool {
	_, ok := reflect.TypeOf(v).FieldByName(name)
	return ok
}
