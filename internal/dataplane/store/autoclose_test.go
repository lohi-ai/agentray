package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// autoclose_test.go pins the scheduled auto-close's store contract against
// live Postgres: which tests are due, the atomic outcome+status close, the
// idempotent replay, and the committed-guard that makes a decided test
// unreachable. Skipped without a test database, like the other live store
// tests.

// commitBackdated creates a proposed test, commits it, then rewinds
// committed_at and review_date — the only way to fixture a test whose review
// date has already arrived, since CommitValidationTest stamps now().
func commitBackdated(t *testing.T, s *Store, userID, projectID string, in ValidationTest, committedAt, reviewDate time.Time) string {
	t.Helper()
	ctx := context.Background()
	in.ProjectID = projectID
	id, err := s.CreateValidationTest(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CommitValidationTest(ctx, userID, projectID, id); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE validation_tests SET committed_at = $3, review_date = $4
WHERE project_id = $1 AND id = $2`, projectID, id, committedAt, reviewDate); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	return id
}

func TestDueValidationTestsSelectsOnlyCommittedPastReview(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	now := time.Now().UTC()
	yesterday := now.Add(-24 * time.Hour)

	due := commitBackdated(t, s, userID, projectID, ValidationTest{
		Hypothesis: "due", MetricEvent: "exp.due", TargetCount: 5, WindowDays: 14,
	}, now.Add(-10*24*time.Hour), yesterday)

	// Future review date: not yet due.
	commitBackdated(t, s, userID, projectID, ValidationTest{
		Hypothesis: "later", MetricEvent: "exp.later", TargetCount: 5, WindowDays: 14,
	}, now.Add(-10*24*time.Hour), now.Add(24*time.Hour))

	// No review date: never due — the partial index's whole point.
	noReview, err := s.CreateValidationTest(ctx, ValidationTest{
		ProjectID: projectID, Hypothesis: "no review", MetricEvent: "exp.none", TargetCount: 5,
	})
	if err != nil {
		t.Fatalf("create no-review: %v", err)
	}
	if err := s.CommitValidationTest(ctx, userID, projectID, noReview); err != nil {
		t.Fatalf("commit no-review: %v", err)
	}

	// Proposed with a past review date: a plan, not a running clock.
	if _, err := s.CreateValidationTest(ctx, ValidationTest{
		ProjectID: projectID, Hypothesis: "draft", MetricEvent: "exp.draft", TargetCount: 5,
		ReviewDate: &yesterday,
	}); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	// Already decided: unreachable by construction.
	decided := commitBackdated(t, s, userID, projectID, ValidationTest{
		Hypothesis: "decided", MetricEvent: "exp.decided", TargetCount: 5, WindowDays: 14,
	}, now.Add(-10*24*time.Hour), yesterday)
	if err := s.DecideValidationTest(ctx, userID, projectID, decided, TestFailed, "missed"); err != nil {
		t.Fatalf("decide: %v", err)
	}

	tests, err := s.DueValidationTests(ctx, projectID, now)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(tests) != 1 || tests[0].ID != due {
		ids := make([]string, 0, len(tests))
		for _, tt := range tests {
			ids = append(ids, tt.ID)
		}
		t.Fatalf("due set = %v, want only %s", ids, due)
	}
}

func TestCloseValidationTestSystemIsAtomicAndIdempotent(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	now := time.Now().UTC()

	id := commitBackdated(t, s, userID, projectID, ValidationTest{
		Hypothesis: "auto-close me", MetricEvent: "exp.close", TargetCount: 5, WindowDays: 14,
	}, now.Add(-10*24*time.Hour), now.Add(-24*time.Hour))

	entry := TestOutcomeEntry{
		Value: 7, Unit: "people firing exp.close (target 5)", Window: "2026-09-05 – 2026-09-15",
		EvidenceRef: "event:exp.close@2026-09-05T00:00:00Z/2026-09-15T00:00:00Z",
		AuthorKind:  "system", AuthorID: "experiment-review",
		RecordedAt: now.Format(time.RFC3339), Note: "threshold cleared",
	}
	closed, err := s.CloseValidationTestSystemIdempotent(ctx, projectID, id, TestPassed, "auto: cleared", entry, "autoclose:"+id+":2026-09-14", "h1")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Status != TestPassed || closed.DecidedAt == nil || closed.DecisionNote != "auto: cleared" {
		t.Fatalf("closed row = status %q decided %v note %q", closed.Status, closed.DecidedAt, closed.DecisionNote)
	}
	if !strings.Contains(closed.OutcomeJSON, "exp.close") {
		t.Fatalf("outcome entry missing from %s", closed.OutcomeJSON)
	}

	// A replayed tick returns the stored receipt — no second entry.
	replay, err := s.CloseValidationTestSystemIdempotent(ctx, projectID, id, TestPassed, "auto: cleared", entry, "autoclose:"+id+":2026-09-14", "h1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.OutcomeJSON != closed.OutcomeJSON {
		t.Fatalf("replay appended again: %s", replay.OutcomeJSON)
	}

	// A different key on a decided test is the benign race, not an error path
	// that retries forever.
	_, err = s.CloseValidationTestSystemIdempotent(ctx, projectID, id, TestFailed, "again", entry, "autoclose:"+id+":other", "h2")
	if !errors.Is(err, ErrTestNoLongerCommitted) {
		t.Fatalf("re-close err = %v, want ErrTestNoLongerCommitted", err)
	}
}

func TestCloseValidationTestSystemCapsOutcomeButStillCloses(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	now := time.Now().UTC()

	id := commitBackdated(t, s, userID, projectID, ValidationTest{
		Hypothesis: "full ledger", MetricEvent: "exp.full", TargetCount: 5, WindowDays: 14,
	}, now.Add(-10*24*time.Hour), now.Add(-24*time.Hour))

	// Fill the outcome list to the cap through the normal append path.
	for i := range outcomeEntryCap {
		cur, err := s.ValidationTestForProject(ctx, projectID, id)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		_, err = s.AppendTestOutcomeIdempotent(ctx, projectID, id, TestOutcomeEntry{
			Value: float64(i), Unit: "obs", AuthorKind: "agent", AuthorID: "r",
			RecordedAt: now.Format(time.RFC3339),
		}, cur.Revision, "fill-"+string(rune('a'+i)), "fh")
		if err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}

	// The close still lands — a full evidence list must not pin a test open.
	closed, err := s.CloseValidationTestSystemIdempotent(ctx, projectID, id, TestInconclusive, "auto: window open", TestOutcomeEntry{
		Value: 1, Unit: "people", AuthorKind: "system", AuthorID: "experiment-review",
		RecordedAt: now.Format(time.RFC3339),
	}, "autoclose:"+id+":cap", "h3")
	if err != nil {
		t.Fatalf("close at cap: %v", err)
	}
	if closed.Status != TestInconclusive {
		t.Fatalf("status = %q, want inconclusive", closed.Status)
	}
	if got := strings.Count(closed.OutcomeJSON, "recorded_at"); got != outcomeEntryCap {
		t.Fatalf("outcome entries = %d, want capped at %d", got, outcomeEntryCap)
	}
}

func TestDecideValidationTestAcceptsInconclusive(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	now := time.Now().UTC()

	id := commitBackdated(t, s, userID, projectID, ValidationTest{
		Hypothesis: "manual inconclusive", MetricEvent: "exp.inc", TargetCount: 5, WindowDays: 14,
	}, now.Add(-2*24*time.Hour), now.Add(-24*time.Hour))

	if err := s.DecideValidationTest(ctx, userID, projectID, id, TestInconclusive, "review came early — window still open"); err != nil {
		t.Fatalf("decide inconclusive: %v", err)
	}
	got, err := s.ValidationTestForProject(ctx, projectID, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Status != TestInconclusive || got.DecidedAt == nil {
		t.Fatalf("status = %q decided %v", got.Status, got.DecidedAt)
	}
}
