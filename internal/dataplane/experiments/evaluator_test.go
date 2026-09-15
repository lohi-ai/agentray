package experiments

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// fakeStore answers the review's reads from canned fixtures and records the
// closes it is asked to write.
type fakeStore struct {
	due      []storage.ValidationTest
	progress map[string]storage.TestProgress
	people   map[string]int // "event|from|to" -> count
	metric   storage.MetricDefinition
	metricOK bool
	overview storage.OverviewResult
	closeErr error
	closed   []closeCall
}

type closeCall struct {
	id, status, note, key string
	entry                 storage.TestOutcomeEntry
}

func (f *fakeStore) DueValidationTests(_ context.Context, projectID string, _ time.Time) ([]storage.ValidationTest, error) {
	if projectID == "" {
		return f.due, nil
	}
	out := []storage.ValidationTest{}
	for _, t := range f.due {
		if t.ProjectID == projectID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) ValidationTestProgress(_ context.Context, t storage.ValidationTest) (storage.TestProgress, error) {
	p := f.progress[t.ID]
	p.Test = t
	return p, nil
}

func (f *fakeStore) MetricDefinitionByKey(_ context.Context, key string) (storage.MetricDefinition, error) {
	if f.metricOK && f.metric.Key == key {
		return f.metric, nil
	}
	return storage.MetricDefinition{}, fmt.Errorf("%w: %q", storage.ErrMetricUnknown, key)
}

func (f *fakeStore) Overview(_ context.Context, _, _, _ string, _ time.Time) (storage.OverviewResult, error) {
	return f.overview, nil
}

func (f *fakeStore) CountEventPeople(_ context.Context, _, eventName string, from, to time.Time) (int, error) {
	return f.people[fmt.Sprintf("%s|%s|%s", eventName, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))], nil
}

func (f *fakeStore) CloseValidationTestSystemIdempotent(_ context.Context, projectID, id, status, note string, entry storage.TestOutcomeEntry, idemKey, _ string) (storage.ValidationTest, error) {
	if f.closeErr != nil {
		return storage.ValidationTest{}, f.closeErr
	}
	f.closed = append(f.closed, closeCall{id: id, status: status, note: note, key: idemKey, entry: entry})
	for i, t := range f.due {
		if t.ID == id {
			f.due[i].Status = status
			return f.due[i], nil
		}
	}
	return storage.ValidationTest{}, storage.ErrTestNoLongerCommitted
}

var (
	reviewNow     = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	reviewPast    = reviewNow.Add(-24 * time.Hour)
	committedAgo  = reviewNow.Add(-20 * 24 * time.Hour) // window still open at review
	committedLong = reviewNow.Add(-30 * 24 * time.Hour) // window closed before review
)

func dueTest(id, metric string, target int, committedAt time.Time) storage.ValidationTest {
	return storage.ValidationTest{
		ID: id, ProjectID: "p1", Hypothesis: "h " + id, MetricEvent: metric,
		TargetCount: target, WindowDays: 14, Status: storage.TestCommitted,
		CommittedAt: &committedAt, CreatedAt: committedAt, ReviewDate: &reviewPast,
	}
}

func TestReviewClosesDueTestsByVerdict(t *testing.T) {
	pass := dueTest("t-pass", "exp.pass", 5, committedAgo)
	fail := dueTest("t-fail", "exp.fail", 5, committedLong)
	inc := dueTest("t-inc", "exp.inc", 5, committedAgo)
	st := &fakeStore{
		due: []storage.ValidationTest{pass, fail, inc},
		progress: map[string]storage.TestProgress{
			"t-pass": {Metric: 7, DaysLeft: 0},
			"t-fail": {Metric: 2, DaysLeft: 0},
			"t-inc":  {Metric: 2, DaysLeft: 4},
		},
	}
	res, err := ReviewProject(context.Background(), st, "p1", reviewNow)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if res.Due != 3 || len(res.Closed) != 3 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	want := map[string]string{"t-pass": storage.TestPassed, "t-fail": storage.TestFailed, "t-inc": storage.TestInconclusive}
	for _, c := range st.closed {
		if c.status != want[c.id] {
			t.Fatalf("%s closed as %q, want %q", c.id, c.status, want[c.id])
		}
		if c.entry.AuthorKind != "system" || c.entry.EvidenceRef == "" || c.entry.Note == "" {
			t.Fatalf("%s entry missing evidence: %+v", c.id, c.entry)
		}
		if !strings.HasPrefix(c.key, "autoclose:"+c.id+":") {
			t.Fatalf("%s key = %q, want deterministic autoclose key", c.id, c.key)
		}
	}
	// The inconclusive note says why: the review beat the window end.
	for _, c := range st.closed {
		if c.id == "t-inc" && !strings.Contains(c.note, "still open") {
			t.Fatalf("inconclusive note = %q, want the open window cited", c.note)
		}
	}
}

func TestReviewSkipsDecidedAndReportsFailures(t *testing.T) {
	won := dueTest("t-won", "exp.won", 5, committedAgo)
	broken := dueTest("t-broken", "exp.broken", 5, committedAgo)
	st := &fakeStore{
		due: []storage.ValidationTest{won, broken},
		progress: map[string]storage.TestProgress{
			"t-won":    {Metric: 9, DaysLeft: 0},
			"t-broken": {Metric: 1, DaysLeft: 0},
		},
	}
	// t-won was decided between the due-read and the write: benign race.
	race := &raceStore{fakeStore: st}
	res, err := ReviewProject(context.Background(), race, "p1", reviewNow)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(res.Closed) != 1 || res.Closed[0].TestID != "t-broken" {
		t.Fatalf("closed = %+v, want only t-broken", res.Closed)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("a lost race must not be an error: %v", res.Errors)
	}
}

// raceStore loses the close race for one named test.
type raceStore struct {
	*fakeStore
	raced bool
}

func (r *raceStore) CloseValidationTestSystemIdempotent(ctx context.Context, projectID, id, status, note string, entry storage.TestOutcomeEntry, idemKey, hash string) (storage.ValidationTest, error) {
	if id == "t-won" && !r.raced {
		r.raced = true
		return storage.ValidationTest{}, storage.ErrTestNoLongerCommitted
	}
	return r.fakeStore.CloseValidationTestSystemIdempotent(ctx, projectID, id, status, note, entry, idemKey, hash)
}

func TestReviewReportsCloseFailures(t *testing.T) {
	due := dueTest("t-err", "exp.err", 5, committedAgo)
	st := &fakeStore{
		due:      []storage.ValidationTest{due},
		progress: map[string]storage.TestProgress{"t-err": {Metric: 9, DaysLeft: 0}},
		closeErr: errors.New("db down"),
	}
	res, err := ReviewProject(context.Background(), st, "p1", reviewNow)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(res.Closed) != 0 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "t-err") {
		t.Fatalf("result = %+v, want one named error and no close", res)
	}
}

func TestGuardrailEventDeltaIsReported(t *testing.T) {
	due := dueTest("t-guard", "exp.metric", 5, committedAgo)
	due.GuardrailMetric = "errors.raised"
	from := committedAgo
	until := committedAgo.AddDate(0, 0, 14) // window closed before the review
	st := &fakeStore{
		due:      []storage.ValidationTest{due},
		progress: map[string]storage.TestProgress{"t-guard": {Metric: 9, DaysLeft: 0}},
		people: map[string]int{
			fmt.Sprintf("errors.raised|%s|%s", from.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)):                      12,
			fmt.Sprintf("errors.raised|%s|%s", from.Add(-until.Sub(from)).UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339)): 4,
		},
	}
	_, err := ReviewProject(context.Background(), st, "p1", reviewNow)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(st.closed) != 1 {
		t.Fatalf("closed = %+v", st.closed)
	}
	note := st.closed[0].note
	if !strings.Contains(note, "errors.raised") || !strings.Contains(note, "12") || !strings.Contains(note, "4") {
		t.Fatalf("guardrail delta missing from note: %q", note)
	}
	// No declared direction → reported, never judged adverse.
	if strings.Contains(note, "ADVERSE") {
		t.Fatalf("free-text guardrail was judged: %q", note)
	}
}

// --- live fixture: the acceptance path end to end -------------------------

// openLiveStore opens a real Postgres + DuckDB store, skipping without a test
// database — the same contract the store package's live tests use.
func openLiveStore(t *testing.T) (*storage.Store, *pgxpool.Pool) {
	t.Helper()
	pgURL := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if pgURL == "" {
		pgURL = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, config.Config{
		PostgresURL:          pgURL,
		DuckDBPath:           filepath.Join(t.TempDir(), "exp-review.duckdb"),
		DefaultProjectName:   "exp-review-test",
		DefaultProjectAPIKey: "exp_review_" + uuid.NewString(),
	})
	if err != nil {
		t.Skipf("test store unavailable (%v)", err)
	}
	t.Cleanup(s.Close)
	pg, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Skipf("fixture pool unavailable (%v)", err)
	}
	t.Cleanup(pg.Close)
	return s, pg
}

func seedEvent(projectID, event, person string, at time.Time) storage.Event {
	return storage.Event{
		ProjectID: projectID, EventID: uuid.NewString(), EventName: event,
		EventType: "user", DistinctID: person, Properties: "{}",
		VisitorClass: "human", Timestamp: at,
	}
}

// The acceptance fixture: a committed test past its review date is re-measured
// and lands decided with the measured delta cited; no-review and decided tests
// are untouched; a replayed pass appends nothing.
func TestScheduledReviewClosesTheDueTest(t *testing.T) {
	s, pg := openLiveStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	acct, err := s.CreateAccount(ctx, fmt.Sprintf("exp-review-%d@test.local", now.UnixNano()), "Review", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	projectID := acct.Project.ID

	mk := func(hypothesis, metric string, target, windowDays int, committedAt time.Time, reviewDate *time.Time) string {
		t.Helper()
		id, err := s.CreateValidationTest(ctx, storage.ValidationTest{
			ProjectID: projectID, Hypothesis: hypothesis, MetricEvent: metric,
			TargetCount: target, WindowDays: windowDays, ReviewDate: reviewDate,
		})
		if err != nil {
			t.Fatalf("create %s: %v", hypothesis, err)
		}
		if err := s.CommitValidationTest(ctx, acct.User.ID, projectID, id); err != nil {
			t.Fatalf("commit %s: %v", hypothesis, err)
		}
		if _, err := pg.Exec(ctx, `
UPDATE validation_tests SET committed_at = $3, review_date = $4
WHERE project_id = $1 AND id = $2`, projectID, id, committedAt, reviewDate); err != nil {
			t.Fatalf("backdate %s: %v", hypothesis, err)
		}
		return id
	}

	past := now.Add(-24 * time.Hour)
	committedAt := now.Add(-20 * 24 * time.Hour)

	// The due test: window closed, 6 of 5 people fired — a pass.
	dueID := mk("digest brings people back", "exp.signup", 5, 14, committedAt, &past)
	// Committed with no review date: never touched.
	noReviewID := mk("no review date", "exp.other", 5, 14, committedAt, nil)
	// Committed, review date in the future: not yet due.
	future := now.Add(48 * time.Hour)
	futureID := mk("not yet due", "exp.future", 5, 14, committedAt, &future)
	// Already decided: never re-closed.
	decidedID := mk("already decided", "exp.decided", 5, 14, committedAt, &past)
	if err := s.DecideValidationTest(ctx, acct.User.ID, projectID, decidedID, storage.TestFailed, "called it early"); err != nil {
		t.Fatalf("decide: %v", err)
	}

	// Six people fire the metric event inside the due test's window.
	events := make([]storage.Event, 0, 6)
	for i := range 6 {
		events = append(events, seedEvent(projectID, "exp.signup", fmt.Sprintf("person-%d", i), committedAt.Add(48*time.Hour)))
	}
	if err := s.InsertEvents(ctx, events); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	res, err := NewEvaluator(s).Review(ctx, projectID, now)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if res.Due != 1 || len(res.Closed) != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v, want due=1 closed=1", res)
	}
	closed := res.Closed[0]
	if closed.TestID != dueID || closed.Status != storage.TestPassed || closed.Metric != 6 {
		t.Fatalf("closed = %+v, want %s passed at 6", closed, dueID)
	}
	if !strings.Contains(closed.DecisionNote, "6 of 5") {
		t.Fatalf("decision note must cite the measurement, got %q", closed.DecisionNote)
	}

	// The row carries the outcome entry and the terminal state.
	got, err := s.ValidationTestForProject(ctx, projectID, dueID)
	if err != nil {
		t.Fatalf("read closed: %v", err)
	}
	if got.Status != storage.TestPassed || got.DecidedAt == nil {
		t.Fatalf("status = %q decided %v", got.Status, got.DecidedAt)
	}
	if !strings.Contains(got.OutcomeJSON, "exp.signup") || !strings.Contains(got.OutcomeJSON, `"author_kind"`) || !strings.Contains(got.OutcomeJSON, `"system"`) {
		t.Fatalf("outcome entry missing: %s", got.OutcomeJSON)
	}

	// The untouched set: no-review stays committed, future stays committed,
	// decided keeps the owner's call.
	for id, want := range map[string]string{
		noReviewID: storage.TestCommitted,
		futureID:   storage.TestCommitted,
		decidedID:  storage.TestFailed,
	} {
		row, err := s.ValidationTestForProject(ctx, projectID, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if row.Status != want {
			t.Fatalf("%s status = %q, want %q", id, row.Status, want)
		}
		if row.OutcomeJSON != "" && id != decidedID {
			t.Fatalf("%s gained an outcome entry: %s", id, row.OutcomeJSON)
		}
	}

	// A replayed pass appends nothing and closes nothing new.
	res2, err := NewEvaluator(s).Review(ctx, projectID, now)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res2.Due != 0 || len(res2.Closed) != 0 {
		t.Fatalf("replay result = %+v, want nothing due", res2)
	}
	again, err := s.ValidationTestForProject(ctx, projectID, dueID)
	if err != nil {
		t.Fatalf("read replayed: %v", err)
	}
	if again.OutcomeJSON != got.OutcomeJSON || again.Revision != got.Revision {
		t.Fatalf("replay mutated the row: %s rev %d", again.OutcomeJSON, again.Revision)
	}
}
