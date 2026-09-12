package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// plans_test.go pins the slice-4 resumable-experiment contract against live
// Postgres: revision-checked proposed-only edits, append-only outcomes with
// author identity and bounds, the proposed→abandoned transition, and keyset
// pagination that reaches rows older than the first page.
//
// Skipped without a test database (AGENTRAY_TEST_DATABASE_URL or the
// docker-compose default), matching the other live store tests.
func plansTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no test database (%v)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database unreachable (%v)", err)
	}
	t.Cleanup(pool.Close)
	s := &Store{pg: pool}
	if err := s.migratePostgres(ctx, config.Config{PostgresURL: url, DefaultProjectName: "plans-test", DefaultProjectAPIKey: "plans_test_key"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestUpdateValidationTestRevisionAndState(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	id, err := s.CreateValidationTest(ctx, ValidationTest{
		ProjectID: projectID, Hypothesis: "checklist lifts activation",
		MetricEvent: "signup.completed", TargetCount: 50,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Proposed edit at the right revision succeeds and bumps it.
	hyp := "checklist lifts day-1 activation"
	got, err := s.UpdateValidationTestIdempotent(ctx, projectID, id,
		ValidationTestUpdate{Hypothesis: &hyp}, 1, "k1", "h1")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Hypothesis != hyp || got.Revision != 2 {
		t.Fatalf("update = %+v", got)
	}

	// Stale revision conflicts.
	if _, err := s.UpdateValidationTestIdempotent(ctx, projectID, id,
		ValidationTestUpdate{Hypothesis: &hyp}, 1, "k2", "h2"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision: got %v, want ErrRevisionConflict", err)
	}

	// Idempotent replay returns the receipt, not a second mutation.
	replay, err := s.UpdateValidationTestIdempotent(ctx, projectID, id,
		ValidationTestUpdate{Hypothesis: &hyp}, 1, "k1", "h1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Revision != 2 {
		t.Fatalf("replay revision = %d, want 2 (receipt, not re-mutation)", replay.Revision)
	}

	// Committed tests are not editable — the owner's agreement cannot be
	// rewritten underneath them.
	if err := s.CommitValidationTest(ctx, userID, projectID, id); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := s.UpdateValidationTestIdempotent(ctx, projectID, id,
		ValidationTestUpdate{Hypothesis: &hyp}, 2, "k3", "h3"); err == nil {
		t.Fatal("update on committed test should fail — only proposed is editable")
	}
}

func TestAppendTestOutcomeContract(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	id, err := s.CreateValidationTest(ctx, ValidationTest{
		ProjectID: projectID, Hypothesis: "digest re-activates dormant users",
		MetricEvent: "session.started", TargetCount: 100,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	entry := TestOutcomeEntry{Value: 11, Unit: "weekly return %", Window: "Sep 1-8",
		EvidenceRef: "saved_query:dormant-report-users@v3", AuthorKind: "agent", AuthorID: "run-1",
		RecordedAt: time.Now().UTC().Format(time.RFC3339)}

	// Proposed tests reject outcomes — nothing to measure against yet.
	if _, err := s.AppendTestOutcomeIdempotent(ctx, projectID, id, entry, 1, "o1", "oh1"); err == nil {
		t.Fatal("outcome on proposed test should fail")
	}

	// Commit, then append.
	if err := s.CommitValidationTest(ctx, userID, projectID, id); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := s.AppendTestOutcomeIdempotent(ctx, projectID, id, entry, 1, "o1", "oh1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got.OutcomeJSON == "" || got.Revision != 2 {
		t.Fatalf("append = %+v", got)
	}

	// Replay returns the receipt — no double append.
	replay, err := s.AppendTestOutcomeIdempotent(ctx, projectID, id, entry, 1, "o1", "oh1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Revision != 2 {
		t.Fatalf("replay revision = %d, want 2", replay.Revision)
	}
}

func TestAbandonValidationTestProposedOnly(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	id, err := s.CreateValidationTest(ctx, ValidationTest{
		ProjectID: projectID, Hypothesis: "stale idea", MetricEvent: "x", TargetCount: 10,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// proposed → abandoned works.
	got, err := s.AbandonValidationTestIdempotent(ctx, projectID, id, "superseded", 1, "a1", "ah1")
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if got.Status != TestAbandoned {
		t.Fatalf("status = %q, want abandoned", got.Status)
	}

	// A committed test cannot be abandoned through this path — decide only.
	id2, err := s.CreateValidationTest(ctx, ValidationTest{
		ProjectID: projectID, Hypothesis: "committed idea", MetricEvent: "y", TargetCount: 10,
	})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if err := s.CommitValidationTest(ctx, userID, projectID, id2); err != nil {
		t.Fatalf("commit2: %v", err)
	}
	if _, err := s.AbandonValidationTestIdempotent(ctx, projectID, id2, "nope", 1, "a2", "ah2"); err == nil {
		t.Fatal("abandon on committed test should fail — decide is the only close")
	}
}

func TestListValidationTestsPageResumesOldWork(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	// More rows than one small page so the cursor must walk.
	for i := range 5 {
		if _, err := s.CreateValidationTest(ctx, ValidationTest{
			ProjectID: projectID, Hypothesis: "idea", MetricEvent: "e", TargetCount: 10,
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	page1, next, err := s.ListValidationTestsPage(ctx, projectID, "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("page1 = %d rows, next %q", len(page1), next)
	}
	page2, next2, err := s.ListValidationTestsPage(ctx, projectID, next, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 = %d rows", len(page2))
	}
	// Pages must not overlap — the cursor is a real keyset, not an offset.
	if page1[0].ID == page2[0].ID || page1[1].ID == page2[0].ID {
		t.Fatal("pages overlap — cursor is not a keyset")
	}
	_ = next2
}

func TestRecommendationForProjectExactRead(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	_, otherProject := seedConvProject(t, s)

	id, err := s.CreateRecommendation(ctx, AgentRecommendation{
		ProjectID: projectID, Category: "growth", Title: "unusual finding xyz",
		Rationale: "because", ImpactScore: 42,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.RecommendationForProject(ctx, projectID, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Title != "unusual finding xyz" {
		t.Fatalf("got %+v", got)
	}
	// Cross-project reads resolve to not-found — the project boundary holds.
	if _, err := s.RecommendationForProject(ctx, otherProject, id); !errors.Is(err, errNoSuchTest) && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project read: got %v, want not-found", err)
	}
}
