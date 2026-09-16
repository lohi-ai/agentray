package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSuggestActivationEventsRanksByLift seeds a mature cohort where
// "onboarding.completed" users return on day 7 and "page_view" users do not,
// and proves the ranking surfaces the retained event first with its evidence —
// reach is the share of the mature cohort, lift is measured against the
// cohort's own D7 baseline, and a young cohort member counts in neither.
func TestSuggestActivationEventsRanksByLift(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	projectID := "cccccccc-1111-2222-3333-444444444444"
	day := func(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }
	ev := func(person, name string, at time.Time) Event {
		return Event{
			ProjectID: projectID, EventID: uuid.NewString(), EventName: name,
			EventType: "user", DistinctID: person, VisitorClass: "human", Timestamp: at,
		}
	}
	// now = Sep 21: cohorts on Sep 1–13 are mature (cohort_day + 8d <= Sep 21).
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

	events := []Event{}
	// 20 mature members: all fire page_view on day 0; the first 10 also fire
	// onboarding.completed on day 1 and return on day 7. The other 10 never
	// return — baseline D7 = 10/20 = 0.5, onboarding D7 = 10/10 = 1.0,
	// page_view D7 = 10/20 = 0.5.
	for i := range 20 {
		u := fmt.Sprintf("u%02d", i)
		events = append(events, ev(u, "page_view", day(1)))
		if i < 10 {
			events = append(events,
				ev(u, "onboarding.completed", day(2)),
				ev(u, "page_view", day(8)), // day-7 return
			)
		}
	}
	// One young member (Sep 18, not mature) fires a loud event — it must not
	// appear: its window has not closed, so it is outside the cohort.
	events = append(events,
		ev("young", "page_view", day(18)),
		ev("young", "young.event", day(18)),
	)
	if err := d.SinkEvents(context.Background(), events, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}

	res, err := s.SuggestActivationEvents(context.Background(), projectID, now)
	if err != nil {
		t.Fatalf("SuggestActivationEvents: %v", err)
	}
	if res.State != OverviewStateOK {
		t.Fatalf("state = %q, want ok", res.State)
	}
	if res.CohortSize != 20 {
		t.Fatalf("cohort_size = %d, want 20", res.CohortSize)
	}
	if len(res.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	top := res.Candidates[0]
	if top.EventName != "onboarding.completed" {
		t.Fatalf("top candidate = %q, want onboarding.completed (lift 0.5 beats page_view's 0)", top.EventName)
	}
	if top.Users != 10 || top.Reach != 0.5 {
		t.Fatalf("top users/reach = %d/%v, want 10/0.5", top.Users, top.Reach)
	}
	if top.D7Return != 1.0 || top.BaselineD7 != 0.5 || top.Lift != 0.5 {
		t.Fatalf("top d7/baseline/lift = %v/%v/%v, want 1/0.5/0.5", top.D7Return, top.BaselineD7, top.Lift)
	}
	for _, c := range res.Candidates {
		if c.EventName == "young.event" {
			t.Fatal("young.event ranked — an immature cohort member leaked into the candidates")
		}
	}
}

// TestSuggestActivationEventsNotReady: a project whose cohorts have not
// matured reports not_ready with no candidates — never an empty ok list.
func TestSuggestActivationEventsNotReady(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	projectID := "dddddddd-1111-2222-3333-444444444444"
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	events := []Event{{
		ProjectID: projectID, EventID: uuid.NewString(), EventName: "page_view",
		EventType: "user", DistinctID: "fresh", VisitorClass: "human",
		Timestamp: now.Add(-24 * time.Hour),
	}}
	if err := d.SinkEvents(context.Background(), events, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
	res, err := s.SuggestActivationEvents(context.Background(), projectID, now)
	if err != nil {
		t.Fatalf("SuggestActivationEvents: %v", err)
	}
	if res.State != OverviewStateNotReady {
		t.Fatalf("state = %q, want not_ready", res.State)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("candidates = %v, want none", res.Candidates)
	}
}
