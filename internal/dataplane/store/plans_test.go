package storage

import (
	"context"
	"errors"
	"os"
	"sort"
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

// The page is the agent's resume entry point: open states first (proposed,
// then committed), decided last, newest first inside each group — and the
// keyset has to carry that order across the group boundary without skipping
// or repeating a row.
func TestListValidationTestsPageOrdersOpenBeforeDecidedNewestFirst(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	mk := func(hyp string) string {
		id, err := s.CreateValidationTest(ctx, ValidationTest{
			ProjectID: projectID, Hypothesis: hyp, MetricEvent: "e", TargetCount: 10,
		})
		if err != nil {
			t.Fatalf("create %q: %v", hyp, err)
		}
		return id
	}
	// created_at is pinned per row — now() ties are not the ordering under
	// test here; the tie case has its own test below.
	setAge := func(id, ts string) {
		if _, err := s.pg.Exec(ctx,
			`UPDATE validation_tests SET created_at = $2::timestamptz WHERE id = $1`,
			id, ts); err != nil {
			t.Fatalf("setAge %s: %v", id, err)
		}
	}

	oldProp := mk("old proposed")
	newProp := mk("new proposed")
	committed := mk("committed")
	decided := mk("decided")
	if err := s.CommitValidationTest(ctx, userID, projectID, committed); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.CommitValidationTest(ctx, userID, projectID, decided); err != nil {
		t.Fatalf("commit decided: %v", err)
	}
	if err := s.DecideValidationTest(ctx, userID, projectID, decided, TestPassed, "shipped"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	// Give the decided row the newest timestamp of all: group rank, not
	// recency, must keep it behind every open row.
	setAge(oldProp, "2026-01-01T00:00:00Z")
	setAge(newProp, "2026-01-04T00:00:00Z")
	setAge(committed, "2026-01-03T00:00:00Z")
	setAge(decided, "2026-01-05T00:00:00Z")

	page1, next, err := s.ListValidationTestsPage(ctx, projectID, "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("page1 = %d rows, next %q", len(page1), next)
	}
	if page1[0].ID != newProp || page1[1].ID != oldProp {
		t.Fatalf("page1 = %q,%q want new proposed then old proposed",
			page1[0].Hypothesis, page1[1].Hypothesis)
	}
	// Page 2 crosses the open/decided boundary: committed (rank 1) leads,
	// then the newest row overall — decided — trails the whole list.
	page2, next2, err := s.ListValidationTestsPage(ctx, projectID, next, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != committed || page2[1].ID != decided {
		t.Fatalf("page2 = %+v, want committed then decided", page2)
	}
	if next2 != "" {
		t.Fatalf("next2 = %q, want exhausted", next2)
	}
}

// Equal created_at values are real — createValidationTest never sets the
// column, so rows inserted inside one now() tick tie. id DESC is the
// tiebreaker and the keyset must not skip or repeat a tied row.
func TestListValidationTestsPageTiesBreakByID(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	ids := make([]string, 0, 3)
	for i := range 3 {
		id, err := s.CreateValidationTest(ctx, ValidationTest{
			ProjectID: projectID, Hypothesis: "tied", MetricEvent: "e", TargetCount: 10,
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	if _, err := s.pg.Exec(ctx,
		`UPDATE validation_tests SET created_at = '2026-01-01T00:00:00Z' WHERE project_id = $1`,
		projectID); err != nil {
		t.Fatalf("tie timestamps: %v", err)
	}

	// Expected order: id::text descending — the ORDER BY's tiebreaker.
	want := append([]string(nil), ids...)
	sort.Sort(sort.Reverse(sort.StringSlice(want)))

	var got []string
	cursor := ""
	for range 4 { // 3 rows at limit 1 → 3 pages, then the loop must have stopped
		page, next, err := s.ListValidationTestsPage(ctx, projectID, cursor, 1)
		if err != nil {
			t.Fatalf("page %d: %v", len(got), err)
		}
		if len(page) != 1 {
			t.Fatalf("page %d = %d rows, want 1", len(got), len(page))
		}
		got = append(got, page[0].ID)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d rows, want %d — keyset skipped or repeated", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %s, want %s (id DESC tiebreak)", i, got[i], want[i])
		}
	}
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

// The findings page is the agent's resume entry point: open findings by impact
// first, then the settled record — and the cursor has to carry that ordering
// across the open/settled boundary, not just within one group.
func TestListRecommendationsPageOrdersOpenByImpactThenHistory(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	mk := func(title string, impact float64) string {
		id, err := s.CreateRecommendation(ctx, AgentRecommendation{
			ProjectID: projectID, Category: "growth", Title: title,
			Rationale: "r", ImpactScore: impact,
		})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return id
	}
	old := mk("old settled", 90)
	low := mk("open low", 10)
	high := mk("open high", 80)
	if err := s.AckRecommendation(ctx, userID, projectID, old, "dismissed", "done"); err != nil {
		t.Fatalf("ack: %v", err)
	}

	page1, next, err := s.ListRecommendationsPage(ctx, projectID, "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("page1 = %d rows, next %q", len(page1), next)
	}
	// Open findings lead, ordered by impact — not by age.
	if page1[0].ID != high || page1[1].ID != low {
		t.Fatalf("page1 order = %q,%q want %q,%q", page1[0].Title, page1[1].Title, "open high", "open low")
	}
	page2, next2, err := s.ListRecommendationsPage(ctx, projectID, next, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != old {
		t.Fatalf("page2 = %+v, want the settled row", page2)
	}
	if next2 != "" {
		t.Fatalf("next2 = %q, want exhausted", next2)
	}
}
