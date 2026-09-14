package storage

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
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
	committedNew := mk("committed newer")
	committedOld := mk("committed older")
	decided := mk("decided")
	if err := s.CommitValidationTest(ctx, userID, projectID, committedNew); err != nil {
		t.Fatalf("commit newer: %v", err)
	}
	if err := s.CommitValidationTest(ctx, userID, projectID, committedOld); err != nil {
		t.Fatalf("commit older: %v", err)
	}
	if err := s.CommitValidationTest(ctx, userID, projectID, decided); err != nil {
		t.Fatalf("commit decided: %v", err)
	}
	if err := s.DecideValidationTest(ctx, userID, projectID, decided, TestPassed, "shipped"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	// Ages are chosen so the corrected order and the two-rank order it
	// replaced disagree: the months-old PROPOSAL is the oldest open row, and
	// the decided row carries the newest timestamp of all — only group rank,
	// not recency, keeps it last.
	setAge(oldProp, "2026-01-01T00:00:00Z")
	setAge(newProp, "2026-01-04T00:00:00Z")
	setAge(committedNew, "2026-01-03T00:00:00Z")
	setAge(committedOld, "2026-01-02T00:00:00Z")
	setAge(decided, "2026-01-05T00:00:00Z")

	// One open partition, newest first: proposed and committed interleave by
	// created_at instead of the old rank putting every proposal ahead of every
	// committed test, which buried a committed test from yesterday behind a
	// months-old proposal.
	page1, next, err := s.ListValidationTestsPage(ctx, projectID, "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("page1 = %d rows, next %q", len(page1), next)
	}
	if page1[0].ID != newProp || page1[1].ID != committedNew {
		t.Fatalf("page1 = %s,%s want new proposed then committed newer",
			page1[0].ID, page1[1].ID)
	}
	// The minted cursor is versioned: the group ranks changed meaning here, so
	// a cursor minted before the change must be refused rather than silently
	// resume the walk at the wrong boundary.
	if parts := strings.Split(next, "|"); len(parts) != 4 || parts[0] != validationCursorVersion {
		t.Fatalf("cursor %q is not %s|rank|timestamp|id", next, validationCursorVersion)
	}
	if _, _, err := s.ListValidationTestsPage(ctx, projectID,
		"1|2026-01-03T00:00:00Z|"+committedNew, 2); err == nil {
		t.Fatal("pre-change 3-part cursor was accepted instead of refused")
	}
	// Page 2 resumes inside the SAME open partition, in the same newest-first
	// order — the boundary the old rank put between proposed and committed.
	page2, next2, err := s.ListValidationTestsPage(ctx, projectID, next, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != committedOld || page2[1].ID != oldProp {
		t.Fatalf("page2 = %+v, want committed older then old proposed", page2)
	}
	if next2 == "" {
		t.Fatal("next2 empty — the decided row is still owed")
	}
	// Page 3 crosses into the decided partition: the newest timestamp overall
	// trails the whole open partition, and the walk is exhausted.
	page3, next3, err := s.ListValidationTestsPage(ctx, projectID, next2, 2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 1 || page3[0].ID != decided {
		t.Fatalf("page3 = %+v, want the decided row", page3)
	}
	if next3 != "" {
		t.Fatalf("next3 = %q, want exhausted", next3)
	}
	// The capped list shares the rank, so it cannot disagree with the page.
	capped, _, err := s.ValidationTestsForProject(ctx, projectID, 0)
	if err != nil {
		t.Fatalf("capped list: %v", err)
	}
	wantCapped := []string{newProp, committedNew, committedOld, oldProp, decided}
	if len(capped) != len(wantCapped) {
		t.Fatalf("capped list = %d rows, want %d", len(capped), len(wantCapped))
	}
	for i, want := range wantCapped {
		if capped[i].ID != want {
			t.Fatalf("capped list row %d = %s, want %s", i, capped[i].ID, want)
		}
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

	// Expected order: id descending — the ORDER BY's uuid tiebreaker.
	want := slices.Clone(ids)
	slices.Sort(want)
	slices.Reverse(want)

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

// Recurrence rewrites last_seen_at — a repeated finding folds into the row it
// repeats instead of appending a card — so the page must order and key by the
// column the write path actually moves. The order this replaced used
// created_at, which is stamped once: a finding a scheduled agent just folded
// forward stayed behind a staler finding created later, and the cursor carried
// a position the next page could not match.
func TestListRecommendationsPageLeadsWithRecurredFinding(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

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
	setCreated := func(id, ts string) {
		if _, err := s.pg.Exec(ctx,
			`UPDATE agent_recommendations SET created_at = $2::timestamptz WHERE id = $1`,
			id, ts); err != nil {
			t.Fatalf("setCreated %s: %v", id, err)
		}
	}
	recurred := mk("slow onboarding", 50)
	stale := mk("stale cart", 50)
	setCreated(recurred, "2026-01-01T00:00:00Z")
	setCreated(stale, "2026-01-08T00:00:00Z")

	// The next cycle re-derives the same finding: it folds into the existing
	// open row and moves last_seen_at forward, which is the ordering key.
	if again := mk("slow onboarding", 50); again != recurred {
		t.Fatalf("recurrence did not fold: got %s, want %s", again, recurred)
	}

	// One row per page: the ordering and the cursor must agree row for row, or
	// the walk skips or repeats the second finding.
	var got []string
	cursor := ""
	for range 3 {
		page, next, err := s.ListRecommendationsPage(ctx, projectID, cursor, 1)
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
		if parts := strings.Split(next, "|"); len(parts) != 5 || parts[0] != recommendationsCursorVersion {
			t.Fatalf("cursor %q is not %s|open|impact|last_seen_at|id", next, recommendationsCursorVersion)
		}
		cursor = next
	}
	if len(got) != 2 || got[0] != recurred || got[1] != stale {
		t.Fatalf("walk = %v, want the recurred finding first then the stale one", got)
	}
	// A cursor minted before the change carries created_at in that slot and
	// must be refused, not silently resume at the wrong row.
	if _, _, err := s.ListRecommendationsPage(ctx, projectID,
		"true|50|2026-01-08T00:00:00Z|"+stale, 1); err == nil {
		t.Fatal("pre-change 4-part cursor was accepted instead of refused")
	}
}

// The findings page is only bounded if the declared index is the page's own
// ordering key — same columns, same directions, project equality leading. The
// declaration and the query both have to change together; this asserts they
// did, including that the key is last_seen_at and not the created_at key the
// page used to carry.
func TestRecommendationsPageIndexIsTheKeysetKey(t *testing.T) {
	ddl := strings.Join(strings.Fields(recommendationsPlansPageIndexDDL), " ")
	if !strings.Contains(ddl, recommendationsPlansPageIndex) {
		t.Fatalf("declared index name %q missing from DDL: %s", recommendationsPlansPageIndex, ddl)
	}
	if !strings.Contains(ddl, "(project_id, "+recommendationsPageOrder+")") {
		t.Fatalf("declared index key is not the page ordering key %q: %s", recommendationsPageOrder, ddl)
	}
	if strings.Contains(ddl, "created_at") {
		t.Fatalf("page index still keys on created_at: %s", ddl)
	}

	sql := strings.Join(strings.Fields(recommendationsPageSQL), " ")
	if !strings.Contains(sql, "ORDER BY "+recommendationsPageOrder) {
		t.Fatalf("page SQL does not order by the shared key: %s", sql)
	}
	if !strings.Contains(sql, "((status = 'open'), impact_score, last_seen_at, id) <") {
		t.Fatalf("keyset predicate is not the shared key: %s", sql)
	}
	// created_at is still projected (the client renders it) but must not
	// decide the page: the keyset clause is everything from WHERE on.
	keyset := sql[strings.Index(sql, "WHERE "):]
	if strings.Contains(keyset, "created_at") {
		t.Fatalf("page keyset still mentions created_at (ordering drift): %s", keyset)
	}
}

// The two soft-delete consumers — dataset_preview and the scoped_external_rows
// CTE behind run_sql — must hide the same rows, and for bool_true a MISSING
// marker means LIVE. The predicate this replaced was NULL for a missing key,
// and `AND NOT (NULL)` is NULL, so every live row of a soft-delete sync
// previewed and queried as gone. Real DuckDB holds the row matrix; both
// consumers read it through their production code paths.
func TestSoftDeleteConsumersAgreeOnMissingFalseTrueAndMalformed(t *testing.T) {
	t.Setenv("AGENT_KEY_ENC_SECRET", "soft-delete-consumers-test-secret")
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	conn, err := s.CreateDataConnector(ctx, userID, projectID, "source", "postgres", "postgres://u:p@localhost:5432/db")
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	boolSync, err := s.CreateConnectorSync(ctx, userID, projectID, conn.ID, ConnectorSyncInput{
		SourceTable: "users", KeyColumn: "id", Enabled: true,
		DeletionMode: "soft_column", SoftDeleteColumn: "is_deleted", SoftDeleteSemantics: "bool_true",
	})
	if err != nil {
		t.Fatalf("create bool sync: %v", err)
	}
	nonNullSync, err := s.CreateConnectorSync(ctx, userID, projectID, conn.ID, ConnectorSyncInput{
		SourceTable: "orders", KeyColumn: "id", Enabled: true,
		DeletionMode: "soft_column", SoftDeleteColumn: "deleted_at", SoftDeleteSemantics: "non_null",
	})
	if err != nil {
		t.Fatalf("create non_null sync: %v", err)
	}

	s.duck = openTestDuckDB(t)
	s.sandboxes = newSQLSandboxPool(s.duck)
	t.Cleanup(s.sandboxes.closeAll)

	land := func(table string, rows ...connector.LandedRow) {
		t.Helper()
		if err := s.InsertExternalRows(ctx, projectID, conn.ID, table, rows, AppliedMark{}); err != nil {
			t.Fatalf("land %s: %v", table, err)
		}
	}
	row := func(key, data string) connector.LandedRow {
		return connector.LandedRow{Key: key, Cursor: key, DataJSON: data}
	}
	land("users",
		row("missing", `{"email":"missing@x"}`),                    // marker absent → live
		row("false", `{"email":"false@x","is_deleted":false}`),     // explicitly live
		row("true", `{"email":"true@x","is_deleted":true}`),        // deleted
		row("malformed", `{"email":"bad@x","is_deleted":"maybe"}`), // uncastable → live
		row("null", `{"email":"null@x","is_deleted":null}`),        // JSON null → live
	)
	land("orders",
		row("open", `{"total":1,"deleted_at":null}`),                        // null → live
		row("cancelled", `{"total":2,"deleted_at":"2026-01-01T00:00:00Z"}`), // non-null → deleted
		row("absent", `{"total":3}`),                                        // absent → live
	)

	previewUsers, err := s.DatasetPreviewForProject(ctx, projectID, boolSync.ID, 50)
	if err != nil {
		t.Fatalf("preview users: %v", err)
	}
	if got := keys(previewUsers.Rows); !slices.Equal(got, []string{"false", "malformed", "missing", "null"}) {
		t.Fatalf("bool_true preview rows = %v, want every non-deleted row (missing marker means live)", got)
	}
	if previewUsers.TotalRows != 4 {
		t.Fatalf("bool_true preview total = %d, want 4", previewUsers.TotalRows)
	}

	previewOrders, err := s.DatasetPreviewForProject(ctx, projectID, nonNullSync.ID, 50)
	if err != nil {
		t.Fatalf("preview orders: %v", err)
	}
	if got := keys(previewOrders.Rows); !slices.Equal(got, []string{"absent", "open"}) {
		t.Fatalf("non_null preview rows = %v, want absent and null-valued rows", got)
	}

	// Same rows through the SQL consumer: run_sql reads external_rows through
	// the scoped CTE that applies softDeleteCondition per connector/table, so
	// the two paths must agree exactly.
	rows, err := s.RunSQL(ctx, projectID, `SELECT row_key FROM external_rows ORDER BY row_key`)
	if err != nil {
		t.Fatalf("scoped SQL: %v", err)
	}
	viaSQL := make([]string, 0, len(rows))
	for _, r := range rows {
		viaSQL = append(viaSQL, r["row_key"].(string))
	}
	want := []string{"absent", "false", "malformed", "missing", "null", "open"}
	if !slices.Equal(viaSQL, want) {
		t.Fatalf("scoped SQL rows = %v, want %v", viaSQL, want)
	}
}

// keys is the row_key projection of a preview page.
func keys(rows []DatasetPreviewRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RowKey)
	}
	slices.Sort(out)
	return out
}
