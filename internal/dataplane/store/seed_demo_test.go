package storage

import (
	"strings"
	"testing"
	"time"
)

// The demo seeder's generator is pure, so its shape can be pinned without a DB.
// The invariants that matter for a good first-boot impression: a real funnel
// (visit > signup > activate > purchase, strictly decreasing), multiple
// acquisition channels, a retention signal (day-0 users returning on day 1), and
// every event well-formed (project set, human class, ~2-day window).
func TestBuildDemoEventsShape(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	pid := "11111111-1111-1111-1111-111111111111"
	events := buildDemoEvents(pid, now)

	if len(events) < 50 {
		t.Fatalf("expected a few hundred demo events, got %d", len(events))
	}

	counts := map[string]int{}
	channels := map[string]bool{}
	earliest, latest := now, now.Add(-999*time.Hour)
	for _, e := range events {
		counts[e.EventName]++
		channels[e.ReferrerChannel] = true
		if e.ProjectID != pid {
			t.Fatalf("event has wrong project_id %q", e.ProjectID)
		}
		if e.VisitorClass != "human" {
			t.Errorf("demo event %q is not classed human (would be filtered from metrics)", e.EventName)
		}
		if e.Timestamp.After(now) {
			t.Errorf("demo event is in the future: %v", e.Timestamp)
		}
		if e.Timestamp.Before(earliest) {
			earliest = e.Timestamp
		}
		if e.Timestamp.After(latest) {
			latest = e.Timestamp
		}
	}

	// Strictly-decreasing funnel — the picture that makes PMF analysis meaningful.
	if !(counts["pageview"] > counts["signup"] && counts["signup"] > counts["activation"] && counts["activation"] > counts["purchase"]) {
		t.Fatalf("funnel is not strictly decreasing: %v", counts)
	}
	if counts["purchase"] == 0 {
		t.Error("no purchases — revenue dashboard would be empty")
	}
	// Multiple channels so the Web-analytics breakdown is not one bar.
	if len(channels) < 3 {
		t.Errorf("expected a mix of acquisition channels, got %v", channels)
	}
	// Two-day-ish window (retention needs day-0 and day-1 cohorts).
	if span := latest.Sub(earliest); span < 20*time.Hour {
		t.Errorf("events span only %v, want ~2 days for a retention curve", span)
	}
}

// The generator must be deterministic for a given day so a re-seed (or a test)
// produces the same corpus — the seed is derived from the truncated day.
func TestBuildDemoEventsDeterministicPerDay(t *testing.T) {
	pid := "22222222-2222-2222-2222-222222222222"
	a := buildDemoEvents(pid, time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC))
	b := buildDemoEvents(pid, time.Date(2026, 7, 2, 18, 0, 0, 0, time.UTC)) // same day, different hour
	if len(a) != len(b) {
		t.Fatalf("same-day seeds differ in size: %d vs %d", len(a), len(b))
	}
	// Names line up position-for-position (timestamps differ by the hour offset).
	for i := range a {
		if a[i].EventName != b[i].EventName {
			t.Fatalf("same-day seed diverged at %d: %q vs %q", i, a[i].EventName, b[i].EventName)
		}
	}
	// A structural spot-check that properties are valid-looking JSON objects.
	for _, e := range a {
		if !strings.HasPrefix(e.Properties, "{") {
			t.Fatalf("event %q has non-object properties %q", e.EventName, e.Properties)
		}
	}
}
