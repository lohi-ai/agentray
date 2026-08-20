package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// validation.go — the pre-product half of the dataplane.
//
// Every other table here answers a question about a product that exists. These
// two exist for the phase before that, where the owner has an idea and the event
// store is empty by design, and they close the two holes that made phase one
// unanswerable:
//
//   - validation_tests — the kill/keep threshold, agreed BEFORE the result. A
//     number agreed in chat leaves no row, so nothing could ever read it back:
//     the owner re-remembered it, or (much more often) re-derived it after
//     seeing the data, which is not a threshold at all. A committed row is a
//     commitment the product can hold the owner to.
//   - waitlist_signups — the only pre-product demand signal that costs the
//     visitor something. A pageview is attention; an address is intent. It lives
//     in Postgres rather than the event store because it is a contact list: it
//     must be exportable, deletable and deduped per person, none of which a
//     1-year-TTL append-only MergeTree does. The matching EVENT is still written
//     to ClickHouse, so funnels, persons and every agent tool see it without a
//     new read path.

// ValidationTest is one falsifiable experiment: a hypothesis, the event that
// counts as success, how many of them mean "keep", and by when.
type ValidationTest struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id,omitempty"`
	// Hypothesis is the claim in the owner's words, e.g. "solo Shopify sellers
	// will give an email for a weekly restock alert".
	Hypothesis string `json:"hypothesis"`
	// MetricEvent is the success event name (waitlist.joined, signup.completed).
	MetricEvent string `json:"metric_event"`
	// BaselineEvent is the denominator (usually user.pageview). Empty means the
	// test is judged on the raw count alone — legitimate when traffic is not
	// measurable, and the readout then shows no rate.
	BaselineEvent string     `json:"baseline_event"`
	TargetCount   int        `json:"target_count"`
	WindowDays    int        `json:"window_days"`
	Status        string     `json:"status"`
	CommittedAt   *time.Time `json:"committed_at,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	DecisionNote  string     `json:"decision_note"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Test lifecycle. `proposed` is the agent's draft; `committed` is the owner
// having agreed to it in advance, which is the whole point of the row; the
// three terminal states are the decision.
const (
	TestProposed  = "proposed"
	TestCommitted = "committed"
	TestPassed    = "passed"
	TestFailed    = "failed"
	TestAbandoned = "abandoned"
)

// WaitlistSignup is one person who asked to hear when the product ships.
type WaitlistSignup struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Email     string `json:"email"`
	Source    string `json:"source"`
	Referrer  string `json:"referrer"`
	// DistinctID ties the signup to the same person the event store knows, so a
	// waitlist row and its pageviews are one person and not two.
	DistinctID  string    `json:"distinct_id"`
	ConsentText string    `json:"consent_text"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	// UnsubscribeToken is deliberately not serialised: it is handed to the
	// person who signed up, once, in the response to their own submission.
	// Listing a waitlist must never emit tokens that let one visitor remove
	// another.
	UnsubscribeToken string `json:"-"`
}

// waitlistCap bounds one project's list. A pre-product waitlist that reaches
// five figures has outgrown this table's purpose, and an uncapped public write
// endpoint holding email addresses is a liability, not a feature.
const waitlistCap = 50000

// migrateValidation provisions both tables. Idempotent CREATE TABLE IF NOT
// EXISTS per the repo's inline-migrate convention; both are new, so there is no
// backfill and no rewrite of an existing table.
func (s *Store) migrateValidation(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS validation_tests (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	run_id UUID REFERENCES agent_runs(id) ON DELETE SET NULL,
	hypothesis TEXT NOT NULL,
	metric_event VARCHAR(128) NOT NULL,
	baseline_event VARCHAR(128) NOT NULL DEFAULT '',
	target_count INTEGER NOT NULL DEFAULT 0,
	window_days INTEGER NOT NULL DEFAULT 14,
	status VARCHAR(16) NOT NULL DEFAULT 'proposed',
	committed_at TIMESTAMPTZ,
	decided_at TIMESTAMPTZ,
	decision_note TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS validation_tests_project_idx ON validation_tests (project_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS waitlist_signups (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	email TEXT NOT NULL,
	email_key TEXT NOT NULL,
	source VARCHAR(128) NOT NULL DEFAULT '',
	referrer TEXT NOT NULL DEFAULT '',
	distinct_id VARCHAR(128) NOT NULL DEFAULT '',
	consent_text TEXT NOT NULL DEFAULT '',
	status VARCHAR(16) NOT NULL DEFAULT 'subscribed',
	unsubscribe_token VARCHAR(64) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (project_id, email_key)
)`,
		`CREATE INDEX IF NOT EXISTS waitlist_signups_project_idx ON waitlist_signups (project_id, created_at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS waitlist_signups_token_idx ON waitlist_signups (unsubscribe_token)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// --- validation tests -------------------------------------------------------

// CreateValidationTest records a proposed threshold. It lands as `proposed`
// whoever writes it: an agent may design the test, but only the owner can agree
// to be bound by it, and that agreement is CommitValidationTest.
func (s *Store) CreateValidationTest(ctx context.Context, t ValidationTest) (string, error) {
	if strings.TrimSpace(t.Hypothesis) == "" {
		return "", errors.New("hypothesis is required")
	}
	if strings.TrimSpace(t.MetricEvent) == "" {
		return "", errors.New("metric_event is required")
	}
	if t.TargetCount <= 0 {
		return "", errors.New("target_count must be greater than zero")
	}
	if t.WindowDays <= 0 {
		t.WindowDays = 14
	}
	var runArg any
	if t.RunID != "" {
		runArg = t.RunID
	}
	var id string
	err := s.pg.QueryRow(ctx, `
INSERT INTO validation_tests (project_id, run_id, hypothesis, metric_event, baseline_event, target_count, window_days)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id::text`,
		t.ProjectID, runArg, strings.TrimSpace(t.Hypothesis), strings.TrimSpace(t.MetricEvent),
		strings.TrimSpace(t.BaselineEvent), t.TargetCount, t.WindowDays).Scan(&id)
	return id, err
}

// ActiveValidationTest returns the test the project is currently running: the
// newest committed one, else the newest proposal awaiting agreement. Decided
// tests are history and never come back as active.
func (s *Store) ActiveValidationTest(ctx context.Context, projectID string) (*ValidationTest, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, coalesce(run_id::text,''), hypothesis, metric_event, baseline_event,
       target_count, window_days, status, committed_at, decided_at, decision_note, created_at
FROM validation_tests
WHERE project_id = $1 AND status IN ('committed','proposed')
ORDER BY (status = 'committed') DESC, created_at DESC
LIMIT 1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	t, err := scanValidationTest(rows)
	if err != nil {
		return nil, err
	}
	return &t, rows.Err()
}

// validationListCap bounds one read of the list.
//
// It is not a display preference. Every row that has been committed is measured
// against the event store to produce its counts, and that is one ClickHouse
// aggregation per row — so an uncapped list is a page whose cost grows with the
// project's whole history of ideas rather than with what is on screen. The cap
// is paired with the total in the response, so a project past it is TOLD it is
// seeing a page rather than being quietly handed a fraction of its own record.
const validationListCap = 25

// ListValidationTests returns a page of the project's tests: the open ones
// (proposed, then committed) before the decided ones, each group newest first.
// The second return value is how many exist in total, so the caller can say so
// when the page is short of it.
//
// The ordering is the screen's, not the table's: a proposal nobody has agreed to
// is the one state where the product is blocked on the human, and burying it
// under a year of decided history is how it stays unanswered.
func (s *Store) ListValidationTests(ctx context.Context, userID, projectID string, limit int) ([]ValidationTest, int, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, 0, err
	}
	return s.ValidationTestsForProject(ctx, project.ID, limit)
}

// ValidationTestsForProject is the same read without a requesting user, for the
// agent path — a run is already bound to one project by the runtime, exactly as
// ActiveValidationTest and CountWaitlistSignups are.
func (s *Store) ValidationTestsForProject(ctx context.Context, projectID string, limit int) ([]ValidationTest, int, error) {
	if limit <= 0 || limit > validationListCap {
		limit = validationListCap
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, coalesce(run_id::text,''), hypothesis, metric_event, baseline_event,
       target_count, window_days, status, committed_at, decided_at, decision_note, created_at,
       count(*) OVER () AS total
FROM validation_tests
WHERE project_id = $1
ORDER BY CASE status WHEN 'proposed' THEN 0 WHEN 'committed' THEN 1 ELSE 2 END, created_at DESC
LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []ValidationTest{}
	total := 0
	for rows.Next() {
		var t ValidationTest
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.RunID, &t.Hypothesis, &t.MetricEvent, &t.BaselineEvent,
			&t.TargetCount, &t.WindowDays, &t.Status, &t.CommittedAt, &t.DecidedAt, &t.DecisionNote, &t.CreatedAt,
			&total); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// ValidationTestByID reads one test the user can see. It is what makes a
// prototype addressable: without it every reader — the detail page and the
// agent alike — is stuck with whichever one ActiveValidationTest happens to
// pick, which is the whole bug the plural surface exists to fix.
func (s *Store) ValidationTestByID(ctx context.Context, userID, projectID, id string) (ValidationTest, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return ValidationTest{}, err
	}
	return s.ValidationTestForProject(ctx, project.ID, id)
}

// ValidationTestForProject is ValidationTestByID without a requesting user, for
// the agent path. An id from another project resolves to not-found, so the op
// cannot be talked into reading across the project boundary.
func (s *Store) ValidationTestForProject(ctx context.Context, projectID, id string) (ValidationTest, error) {
	if strings.TrimSpace(id) == "" {
		return ValidationTest{}, errNoSuchTest
	}
	// A malformed id would otherwise reach Postgres as a bad uuid cast and come
	// back as a 500 for what is really "no such test".
	if !looksLikeUUID(id) {
		return ValidationTest{}, errNoSuchTest
	}
	row := s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, coalesce(run_id::text,''), hypothesis, metric_event, baseline_event,
       target_count, window_days, status, committed_at, decided_at, decision_note, created_at
FROM validation_tests WHERE id = $1 AND project_id = $2`, id, projectID)
	t, err := scanValidationTest(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ValidationTest{}, errNoSuchTest
	}
	if err != nil {
		return ValidationTest{}, err
	}
	return t, nil
}

var errNoSuchTest = errors.New("no test with that id in this project")

// looksLikeUUID is a shape check so a junk id — an agent inventing one, a stale
// link — answers "no such test" instead of a database cast error dressed up as a
// server fault.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// ErrNoSuchValidationTest reports whether err is the not-found from
// ValidationTestByID, so the HTTP layer can answer 404 instead of 500.
func ErrNoSuchValidationTest(err error) bool { return errors.Is(err, errNoSuchTest) }

// MeasuredTest is one test as every plural reader wants it: the row, plus what
// the event store says about it, plus the single verdict both the page and the
// agent must quote.
//
// Measured is false for a proposal. A threshold nobody has agreed to counts
// nothing, and reporting "0 of 40" against a number the owner never accepted
// reads as a failing test rather than an unanswered question.
type MeasuredTest struct {
	ValidationTest
	Measured      bool `json:"measured"`
	MetricCount   int  `json:"metric_count"`
	BaselineCount int  `json:"baseline_count"`
	DaysElapsed   int  `json:"days_elapsed"`
	DaysLeft      int  `json:"days_left"`
	// Verdict is passed / failed / committed (still running) for a live test,
	// and the owner's own decision for a decided one — a closed test's verdict is
	// what the human called it, never a recomputation that could now disagree.
	Verdict string `json:"verdict,omitempty"`
}

// MeasureValidationTest attaches the measurement and the verdict to one test.
func (s *Store) MeasureValidationTest(ctx context.Context, t ValidationTest) (MeasuredTest, error) {
	out := MeasuredTest{ValidationTest: t}
	if t.CommittedAt == nil {
		return out, nil
	}
	p, err := s.ValidationTestProgress(ctx, t)
	if err != nil {
		return MeasuredTest{}, err
	}
	out.Measured = true
	out.MetricCount, out.BaselineCount = p.Metric, p.Baseline
	out.DaysElapsed, out.DaysLeft = p.DaysElapsed, p.DaysLeft
	if t.Status == TestCommitted {
		out.Verdict = p.Verdict()
	} else {
		out.Verdict = t.Status
	}
	return out, nil
}

// MeasureValidationTests measures a page of tests. One event-store aggregation
// per committed row — bounded by validationListCap, which is why that cap
// exists.
func (s *Store) MeasureValidationTests(ctx context.Context, tests []ValidationTest) ([]MeasuredTest, error) {
	out := make([]MeasuredTest, 0, len(tests))
	for _, t := range tests {
		m, err := s.MeasureValidationTest(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows, so the column list above
// is written once and cannot drift between the single-row and list reads.
type rowScanner interface{ Scan(dest ...any) error }

func scanValidationTest(row rowScanner) (ValidationTest, error) {
	var t ValidationTest
	err := row.Scan(&t.ID, &t.ProjectID, &t.RunID, &t.Hypothesis, &t.MetricEvent, &t.BaselineEvent,
		&t.TargetCount, &t.WindowDays, &t.Status, &t.CommittedAt, &t.DecidedAt, &t.DecisionNote, &t.CreatedAt)
	return t, err
}

// CommitValidationTest is the owner agreeing to the number in advance. It only
// moves a proposal forward — re-committing a decided test would rewrite history
// after the fact, which is the exact dishonesty this table exists to prevent.
func (s *Store) CommitValidationTest(ctx context.Context, userID, projectID, id string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	tag, err := s.pg.Exec(ctx, `
UPDATE validation_tests SET status = 'committed', committed_at = now()
WHERE id = $1 AND project_id = $2 AND status = 'proposed'`, id, project.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("no proposed test with that id")
	}
	return nil
}

// DecideValidationTest closes a committed test. The note is kept because "why"
// is the part worth reading a month later.
func (s *Store) DecideValidationTest(ctx context.Context, userID, projectID, id, status, note string) error {
	if status != TestPassed && status != TestFailed && status != TestAbandoned {
		return errors.New("status must be passed, failed or abandoned")
	}
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	tag, err := s.pg.Exec(ctx, `
UPDATE validation_tests SET status = $3, decided_at = now(), decision_note = $4
WHERE id = $1 AND project_id = $2 AND status = 'committed'`, id, project.ID, status, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("no committed test with that id")
	}
	return nil
}

// TestProgress is a committed test measured against reality.
type TestProgress struct {
	Test ValidationTest `json:"test"`
	// Metric is how many success events have landed inside the window, counted
	// as distinct PEOPLE rather than raw events: forty pageviews from one
	// visitor is one interested person, and a threshold of "40 signups" that a
	// single enthusiastic reloader can clear measures nothing.
	Metric int `json:"metric_count"`
	// Baseline is the denominator over the same window, also per person. Zero
	// when the test names none.
	Baseline int `json:"baseline_count"`
	// DaysElapsed / DaysLeft are whole days against the committed window.
	//
	// DaysLeft rounds UP: any time still on the clock has to read as at least
	// one day, because Verdict() keys `failed` off it reaching zero. Truncating
	// would call a test dead while twenty-three hours of its window were still
	// open — and a signup arriving in that last day is exactly the one a
	// borderline test is decided by.
	DaysElapsed int `json:"days_elapsed"`
	DaysLeft    int `json:"days_left"`
}

// ValidationTestProgress counts the test's metric and baseline over its window.
// The window opens at commit time, not creation time — evidence gathered before
// the owner agreed to the number cannot count toward it, or the threshold is
// being fitted to data that already exists.
func (s *Store) ValidationTestProgress(ctx context.Context, t ValidationTest) (TestProgress, error) {
	p := TestProgress{Test: t}
	from := t.CreatedAt
	if t.CommittedAt != nil {
		from = *t.CommittedAt
	}
	until := from.AddDate(0, 0, t.WindowDays)
	now := time.Now().UTC()
	p.DaysElapsed = int(now.Sub(from).Hours() / 24)
	if p.DaysElapsed < 0 {
		p.DaysElapsed = 0
	}
	p.DaysLeft = int(math.Ceil(until.Sub(now).Hours() / 24))
	if p.DaysLeft < 0 {
		p.DaysLeft = 0
	}
	if s.ch == nil {
		return p, nil
	}
	names := []string{t.MetricEvent}
	if t.BaselineEvent != "" {
		names = append(names, t.BaselineEvent)
	}
	rows, err := s.ch.Query(ctx, `
SELECT event_name, uniqExact(distinct_id) AS people
FROM events
WHERE project_id = ? AND event_name IN (?) AND timestamp >= ? AND timestamp < ?
GROUP BY event_name`, t.ProjectID, names, from, until)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var people uint64
		if err := rows.Scan(&name, &people); err != nil {
			return p, err
		}
		switch name {
		case t.MetricEvent:
			p.Metric = int(people)
		case t.BaselineEvent:
			p.Baseline = int(people)
		}
	}
	return p, rows.Err()
}

// Verdict reads a progress as the decision it implies, and is the reason the
// threshold row exists at all: with the number agreed in advance, "did it work"
// stops being a conversation and becomes a lookup.
//
// It never returns `failed` while the window is still open — a test that has not
// finished has not failed, and calling it early is how a good idea gets killed
// by a slow first week.
func (p TestProgress) Verdict() string {
	if p.Metric >= p.Test.TargetCount {
		return TestPassed
	}
	if p.DaysLeft <= 0 {
		return TestFailed
	}
	return TestCommitted
}

// --- waitlist ---------------------------------------------------------------

// AddWaitlistSignup records one address, idempotently. A second submit of the
// same address is the same person pressing the button twice, not demand for two
// — it updates the row's context and returns the original, so the count stays a
// count of people.
//
// It returns the row and whether this submit was a JOIN — a first insert, or a
// return after unsubscribing — because only a join deserves an event: counting a
// double-click as two would inflate the very number the threshold is judged
// against, and not counting a genuine return would lose a real signup the
// subscriber count has already gained.
func (s *Store) AddWaitlistSignup(ctx context.Context, sn WaitlistSignup) (WaitlistSignup, bool, error) {
	email := strings.TrimSpace(sn.Email)
	key := strings.ToLower(email)
	if !plausibleEmail(key) {
		return WaitlistSignup{}, false, errors.New("a valid email address is required")
	}
	// The cap bounds NEW addresses only: someone already on the list may always
	// re-submit (that path writes no new row), because refusing them would look
	// to them like the form is broken.
	//
	// It counts SUBSCRIBED rows, the same population CountWaitlistSignups
	// reports, so the cap and the readout can never disagree: a project whose
	// list emptied itself through unsubscribes must not read as full while the
	// owner's scoreboard shows zero. The probe stops at the cap rather than
	// aggregating the whole table, because this runs on a public endpoint.
	var count int
	if err := s.pg.QueryRow(ctx, `
SELECT count(*) FROM (
	SELECT 1 FROM waitlist_signups WHERE project_id = $1 AND status = 'subscribed' LIMIT $2
) capped`, sn.ProjectID, waitlistCap).Scan(&count); err != nil {
		return WaitlistSignup{}, false, err
	}
	if count >= waitlistCap {
		var known bool
		if err := s.pg.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM waitlist_signups WHERE project_id = $1 AND email_key = $2)`,
			sn.ProjectID, key).Scan(&known); err != nil {
			return WaitlistSignup{}, false, err
		}
		if !known {
			return WaitlistSignup{}, false, fmt.Errorf("waitlist is full (%d addresses)", waitlistCap)
		}
	}
	token, err := randomToken()
	if err != nil {
		return WaitlistSignup{}, false, err
	}
	var out WaitlistSignup
	var joined bool
	// `prev` is read from the snapshot taken before the upsert runs, which is the
	// only way to see the status the row had on arrival — a RETURNING clause on
	// DO UPDATE reports the row as it now is. It is what makes "did this person
	// just join" answerable: a first insert and a return after unsubscribing are
	// both joins, a double-click on a live subscription is not.
	err = s.pg.QueryRow(ctx, `
WITH prev AS (
	SELECT status FROM waitlist_signups WHERE project_id = $1 AND email_key = $3
), up AS (
	INSERT INTO waitlist_signups (project_id, email, email_key, source, referrer, distinct_id, consent_text, unsubscribe_token)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (project_id, email_key) DO UPDATE SET
		source = CASE WHEN excluded.source <> '' THEN excluded.source ELSE waitlist_signups.source END,
		referrer = CASE WHEN excluded.referrer <> '' THEN excluded.referrer ELSE waitlist_signups.referrer END,
		distinct_id = CASE WHEN excluded.distinct_id <> '' THEN excluded.distinct_id ELSE waitlist_signups.distinct_id END,
		-- Re-submitting the form is a subscribe. Without this, someone who left and
		-- came back stays 'unsubscribed' forever while the page tells them they are
		-- on the list — the one state where the product's answer and its data
		-- disagree, and the owner's count silently loses a real signup.
		status = 'subscribed'
	RETURNING id::text AS id, project_id::text AS project_id, email, source, referrer, distinct_id,
		consent_text, status, created_at, (xmax = 0) AS inserted, unsubscribe_token
)
SELECT up.id, up.project_id, up.email, up.source, up.referrer, up.distinct_id, up.consent_text,
	up.status, up.created_at,
	(up.inserted OR COALESCE(prev.status, '') = 'unsubscribed') AS joined,
	up.unsubscribe_token
FROM up LEFT JOIN prev ON true`,
		sn.ProjectID, email, key, sn.Source, sn.Referrer, sn.DistinctID, sn.ConsentText, token).
		Scan(&out.ID, &out.ProjectID, &out.Email, &out.Source, &out.Referrer, &out.DistinctID,
			&out.ConsentText, &out.Status, &out.CreatedAt, &joined, &token)
	if err != nil {
		return WaitlistSignup{}, false, err
	}
	out.UnsubscribeToken = token
	return out, joined, nil
}

// UnsubscribeWaitlist honours a removal request from the address itself, via the
// token handed back at signup. Token-only (no project id, no auth) because the
// person unsubscribing is not a user of this product and never will be.
func (s *Store) UnsubscribeWaitlist(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("token is required")
	}
	tag, err := s.pg.Exec(ctx, `
UPDATE waitlist_signups SET status = 'unsubscribed' WHERE unsubscribe_token = $1`, token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("unknown or already-used unsubscribe link")
	}
	return nil
}

// ListWaitlistSignups returns the project's list, newest first, bounded.
func (s *Store) ListWaitlistSignups(ctx context.Context, userID, projectID string, limit int) ([]WaitlistSignup, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, email, source, referrer, distinct_id, consent_text, status, created_at
FROM waitlist_signups WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2`, project.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WaitlistSignup{}
	for rows.Next() {
		var sn WaitlistSignup
		if err := rows.Scan(&sn.ID, &sn.ProjectID, &sn.Email, &sn.Source, &sn.Referrer,
			&sn.DistinctID, &sn.ConsentText, &sn.Status, &sn.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// ExportWaitlistSignups walks the WHOLE list, oldest first, in keyset pages.
//
// The listing above is a screenful and is right to be capped. An export is the
// opposite promise: an owner who mails a launch announcement to the file this
// produces has to be mailing all of it. A silent LIMIT there would hand them a
// slice of their own contacts with nothing on the page or in the file saying so
// — and they would find out from the people who never heard from them.
//
// It carries the unsubscribe token, because the owner is the one who sends the
// mail this list is for and every message needs a way out. That is the token's
// only exit from the server: an authenticated request from the account that
// owns the row, never the public form response.
func (s *Store) ExportWaitlistSignups(ctx context.Context, userID, projectID string, yield func(WaitlistSignup) error) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	const page = 500
	var cursor time.Time
	var cursorID string
	for {
		rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, email, source, referrer, distinct_id, consent_text, status, created_at, unsubscribe_token
FROM waitlist_signups
WHERE project_id = $1 AND ($2::timestamptz IS NULL OR (created_at, id::text) > ($2, $3))
ORDER BY created_at, id::text LIMIT $4`, project.ID, nullableTime(cursor), cursorID, page)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			var sn WaitlistSignup
			if err := rows.Scan(&sn.ID, &sn.ProjectID, &sn.Email, &sn.Source, &sn.Referrer,
				&sn.DistinctID, &sn.ConsentText, &sn.Status, &sn.CreatedAt, &sn.UnsubscribeToken); err != nil {
				rows.Close()
				return err
			}
			if err := yield(sn); err != nil {
				rows.Close()
				return err
			}
			cursor, cursorID = sn.CreatedAt, sn.ID
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if n < page {
			return nil
		}
	}
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// CountWaitlistSignups counts subscribed addresses — the number the threshold is
// judged against. Unsubscribes are excluded: someone who left is not demand.
func (s *Store) CountWaitlistSignups(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.pg.QueryRow(ctx, `
SELECT count(*) FROM waitlist_signups WHERE project_id = $1 AND status = 'subscribed'`, projectID).Scan(&n)
	return n, err
}

// DeleteWaitlistSignup erases one address on request. A real delete, not a flag:
// "remove my data" has to mean the row is gone.
func (s *Store) DeleteWaitlistSignup(ctx context.Context, userID, projectID, id string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	_, err = s.pg.Exec(ctx, `DELETE FROM waitlist_signups WHERE id = $1 AND project_id = $2`, id, project.ID)
	return err
}

// plausibleEmail is a shape check, not a validity check — nothing short of
// delivery proves an address, and a stricter regex mostly rejects real people.
func plausibleEmail(s string) bool {
	if len(s) < 6 || len(s) > 254 || strings.ContainsAny(s, " \t\r\n,;<>\"") {
		return false
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	domain := s[at+1:]
	dot := strings.LastIndex(domain, ".")
	return dot > 0 && dot < len(domain)-1
}
