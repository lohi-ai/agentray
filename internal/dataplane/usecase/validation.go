package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// validation.go — the two operations that make a pre-product test readable.
//
// They are generic dataplane capabilities, not agent code: any pack with
// growth_suggest can propose an experiment and read how it is doing, exactly as
// any pack can file a recommendation. What makes them necessary rather than a
// skill is that a skill can only produce TEXT — and the entire failure this
// closes is that a threshold agreed in prose leaves no row, so nothing can ever
// read it back and the number gets quietly re-derived once the result is in.

type proposeTestInput struct {
	Hypothesis    string `json:"hypothesis" desc:"the claim being tested, in the owner's words" required:"true"`
	MetricEvent   string `json:"metric_event" desc:"event name that counts as success, e.g. waitlist.joined" required:"true"`
	TargetCount   int    `json:"target_count" desc:"how many DISTINCT people must fire it for the idea to be worth building" required:"true"`
	BaselineEvent string `json:"baseline_event" desc:"denominator event, usually user.pageview (optional)"`
	WindowDays    int    `json:"window_days" desc:"how long the test runs, in days (default 14)"`
	// Slice-4 resumable-experiment fields — all optional, all persisted so an
	// agent with no chat history can resume the experiment from reads alone.
	ObservationID   string  `json:"observation_id" desc:"finding id this experiment answers"`
	Evidence        string  `json:"evidence" desc:"typed envelope JSON object: {query_ref, metric_version, dataset_version, range, filters, timezone, watermark, warnings}"`
	BaselineValue   float64 `json:"baseline_value" desc:"measured baseline value (0 = not recorded)"`
	BaselineUnit    string  `json:"baseline_unit" desc:"e.g. 'weekly return %', 'signups'"`
	BaselineWindow  string  `json:"baseline_window" desc:"baseline measurement window"`
	Audience        string  `json:"audience" desc:"segment or cohort the experiment targets"`
	Owner           string  `json:"owner" desc:"responsible user or agent"`
	SuccessMetric   string  `json:"success_metric" desc:"win condition"`
	GuardrailMetric string  `json:"guardrail_metric" desc:"do-no-harm bound"`
	ReviewDate      string  `json:"review_date" desc:"RFC3339 decision due date"`
	IdempotencyKey  string  `json:"idempotency_key" desc:"retry-safe write key"`
}

type proposeTestOutput struct {
	TestID string `json:"test_id"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

// proposeTest records a falsifiable threshold. It is deliberately NOT terminal:
// designing the test is the middle of a conversation, and ending the run here
// would leave the owner holding a row nobody explained.
//
// It always lands as `proposed`. An agent can design a test; only the owner can
// agree to be bound by one, and that agreement is a separate, human act — which
// is what makes the number a commitment rather than a suggestion.
func proposeTest() opcore.Operation[proposeTestInput, proposeTestOutput] {
	return opcore.Operation[proposeTestInput, proposeTestOutput]{
		Name:           "propose_test",
		Summary:        "Propose a validation test: the success event, how many distinct people must fire it, and by when. The owner commits to it before the data arrives.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in proposeTestInput) (proposeTestOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return proposeTestOutput{}, err
			}
			var reviewDate *time.Time
			if in.ReviewDate != "" {
				t, perr := time.Parse(time.RFC3339, in.ReviewDate)
				if perr != nil {
					return proposeTestOutput{}, errBadInput("review_date must be RFC3339")
				}
				reviewDate = &t
			}
			if verr := validateEvidenceEnvelope(in.Evidence); verr != nil {
				return proposeTestOutput{}, verr
			}
			if rerr := annotationRefsInEnvelope(ctx, d, cc.ProjectID, in.Evidence); rerr != nil {
				return proposeTestOutput{}, rerr
			}
			var baselineValue *float64
			if in.BaselineValue != 0 {
				baselineValue = &in.BaselineValue
			}
			t := storage.ValidationTest{
				ProjectID:       cc.ProjectID,
				RunID:           cc.RunID,
				Hypothesis:      in.Hypothesis,
				MetricEvent:     in.MetricEvent,
				BaselineEvent:   in.BaselineEvent,
				TargetCount:     in.TargetCount,
				WindowDays:      in.WindowDays,
				ObservationID:   in.ObservationID,
				EvidenceJSON:    in.Evidence,
				BaselineValue:   baselineValue,
				BaselineUnit:    in.BaselineUnit,
				BaselineWindow:  in.BaselineWindow,
				Audience:        in.Audience,
				Owner:           in.Owner,
				SuccessMetric:   in.SuccessMetric,
				GuardrailMetric: in.GuardrailMetric,
				ReviewDate:      reviewDate,
			}
			hash, err := requestHash(in)
			if err != nil {
				return proposeTestOutput{}, err
			}
			id, err := d.Repo.CreateValidationTestIdempotent(ctx, t, strings.TrimSpace(in.IdempotencyKey), hash)
			if err != nil {
				return proposeTestOutput{}, err
			}
			return proposeTestOutput{
				TestID: id,
				Status: storage.TestProposed,
				Note: "Proposed. Tell the owner it is waiting for them to commit on /start?job=validate — " +
					"a threshold they have not agreed to is not a threshold.",
			}, nil
		},
	}
}

type testStatusInput struct {
	// TestID names WHICH test to read. Empty keeps the original behaviour — the
	// project's active one — so every existing prompt and pack still works. It
	// exists because the owner can now have five prototypes open on one screen,
	// and an agent that can only ever read "the" test will confidently answer
	// about a different one than the one they asked about.
	TestID string `json:"test_id" desc:"id of the prototype to read (from list_tests). Omit for the project's active test."`
}

type testStatusOutput struct {
	// HasTest is false for a project that has never proposed one. The agent must
	// read this before anything else: reporting "0 of 0" as a result is worse
	// than saying there is no test.
	HasTest       bool   `json:"has_test"`
	TestID        string `json:"test_id,omitempty"`
	Hypothesis    string `json:"hypothesis,omitempty"`
	Status        string `json:"status,omitempty"`
	MetricEvent   string `json:"metric_event,omitempty"`
	MetricCount   int    `json:"metric_count"`
	TargetCount   int    `json:"target_count"`
	BaselineEvent string `json:"baseline_event,omitempty"`
	BaselineCount int    `json:"baseline_count"`
	// ConversionPct is metric/baseline as a percentage, or -1 when the test
	// names no baseline. -1 rather than 0 so an agent cannot report a real zero
	// conversion rate for a test that never measured one.
	ConversionPct float64 `json:"conversion_pct"`
	DaysElapsed   int     `json:"days_elapsed"`
	DaysLeft      int     `json:"days_left"`
	WaitlistCount int     `json:"waitlist_count"`
	// Verdict is what the agreed number says right now: passed, failed, or
	// committed (= still running, too early to call).
	Verdict string `json:"verdict,omitempty"`
	Note    string `json:"note"`
	// Slice-4 resumable fields — an agent with no chat history reconstructs
	// the full experiment state from this read alone.
	ObservationID   string   `json:"observation_id,omitempty"`
	EvidenceJSON    string   `json:"evidence_json,omitempty"`
	BaselineValue   *float64 `json:"baseline_value,omitempty"`
	BaselineUnit    string   `json:"baseline_unit,omitempty"`
	BaselineWindow  string   `json:"baseline_window,omitempty"`
	Audience        string   `json:"audience,omitempty"`
	Owner           string   `json:"owner,omitempty"`
	SuccessMetric   string   `json:"success_metric,omitempty"`
	GuardrailMetric string   `json:"guardrail_metric,omitempty"`
	ReviewDate      string   `json:"review_date,omitempty"`
	OutcomeJSON     string   `json:"outcome_json,omitempty"`
	Revision        int64    `json:"revision"`
}

// testStatus reads the live test against its committed threshold. This is a
// READ of project data — classified as evidence in runtime/policy.go, so an
// agent that answers "how is the test doing" from it is not nudged for
// unbacked figures, and one that answers without it is.
func testStatus() opcore.Operation[testStatusInput, testStatusOutput] {
	return opcore.Operation[testStatusInput, testStatusOutput]{
		Name:    "test_status",
		Summary: "Read one validation test — by id, or the project's active one — and how it is doing against the threshold agreed in advance.",
		Scope:   "growth_suggest",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in testStatusInput) (testStatusOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return testStatusOutput{}, err
			}
			var test *storage.ValidationTest
			if id := strings.TrimSpace(in.TestID); id != "" {
				found, fErr := d.Repo.ValidationTestForProject(ctx, cc.ProjectID, id)
				if fErr != nil {
					// A named test that does not exist is not "no test": answering
					// with the active one would report a different experiment's
					// numbers under the name the owner asked about. Infrastructure
					// failures remain failures; only the repository's typed
					// project-scoped not-found result becomes this useful reply.
					if !storage.ErrNoSuchValidationTest(fErr) && !errors.Is(fErr, pgx.ErrNoRows) {
						return testStatusOutput{}, fErr
					}
					return testStatusOutput{
						HasTest: false,
						Note: "No prototype with id " + id + " in this project. Call list_tests and use an id from it — " +
							"do not answer about a different test than the one you were asked about.",
					}, nil
				}
				test = &found
			} else {
				test, err = d.Repo.ActiveValidationTest(ctx, cc.ProjectID)
				if err != nil {
					return testStatusOutput{}, err
				}
			}
			count, err := d.Repo.CountWaitlistSignups(ctx, cc.ProjectID)
			if err != nil {
				return testStatusOutput{}, err
			}
			if test == nil {
				return testStatusOutput{
					HasTest:       false,
					WaitlistCount: count,
					Note: "No validation test exists yet. Design one and record it with propose_test — " +
						"otherwise there is no number to judge the result against.",
				}, nil
			}
			p, err := d.Repo.ValidationTestProgress(ctx, *test)
			if err != nil {
				return testStatusOutput{}, err
			}
			out := testStatusOutput{
				HasTest:       true,
				TestID:        test.ID,
				Hypothesis:    test.Hypothesis,
				Status:        test.Status,
				MetricEvent:   test.MetricEvent,
				MetricCount:   p.Metric,
				TargetCount:   test.TargetCount,
				BaselineEvent: test.BaselineEvent,
				BaselineCount: p.Baseline,
				ConversionPct: -1,
				DaysElapsed:   p.DaysElapsed,
				DaysLeft:      p.DaysLeft,
				WaitlistCount: count,
				// Slice-4 resumable fields — an agent with no chat history
				// reconstructs the full experiment state from this read alone.
				ObservationID:   test.ObservationID,
				EvidenceJSON:    test.EvidenceJSON,
				BaselineValue:   test.BaselineValue,
				BaselineUnit:    test.BaselineUnit,
				BaselineWindow:  test.BaselineWindow,
				Audience:        test.Audience,
				Owner:           test.Owner,
				SuccessMetric:   test.SuccessMetric,
				GuardrailMetric: test.GuardrailMetric,
				OutcomeJSON:     test.OutcomeJSON,
				Revision:        test.Revision,
			}
			if test.ReviewDate != nil {
				out.ReviewDate = test.ReviewDate.UTC().Format(time.RFC3339)
			}
			if test.BaselineEvent != "" && p.Baseline > 0 {
				out.ConversionPct = float64(p.Metric) / float64(p.Baseline) * 100
			}
			if test.Status == storage.TestProposed {
				// The window opens at COMMIT, so a proposal has no window and its
				// counts are meaningless. Zero them rather than report a figure
				// beside a note that says nothing is being counted — the two
				// disagreeing is how an agent ends up quoting a number nobody
				// agreed to as if it were progress.
				out.MetricCount, out.BaselineCount = 0, 0
				out.ConversionPct = -1
				out.DaysElapsed, out.DaysLeft = 0, 0
				out.Note = "This test is still only PROPOSED — the owner has not committed to the number, " +
					"so nothing is being counted yet. Ask them to commit it on /plans."
				return out, nil
			}
			if test.Status != storage.TestCommitted {
				// Already decided. The verdict is what the owner CALLED it, never a
				// recomputation — a late-arriving event must not turn a test the
				// owner closed as failed into a pass the product now claims.
				out.Verdict = test.Status
				out.Note = "The owner already decided this one: " + test.Status + "." +
					decisionNoteSuffix(test.DecisionNote) +
					" Report the decision as it stands; do not re-judge it from the counts."
				return out, nil
			}
			out.Verdict = p.Verdict()
			out.Note = verdictNote(out)
			return out, nil
		},
	}
}

// decisionNoteSuffix renders the owner's own words when they left any. "Why" is
// the part worth reading a month later, and it is the only part of a closed test
// the agent cannot re-derive.
func decisionNoteSuffix(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return " They wrote: " + strings.TrimSpace(note) + "."
}

type listTestsInput struct {
	// Cursor pages the full history — the fixed newest-25 list cannot resume
	// work older than the first page. Empty keeps the original capped read.
	Cursor string `json:"cursor" desc:"keyset cursor from a previous page"`
	Limit  int    `json:"limit" desc:"page size, max 50"`
}

// listedTest is deliberately thin: enough to name each prototype and say which
// one needs the owner, not enough to answer "how is it doing". Measuring every
// row is one event-store aggregation each, and an agent that wants the numbers
// for one of them should call test_status with its id.
type listedTest struct {
	TestID      string `json:"test_id"`
	Hypothesis  string `json:"hypothesis"`
	Status      string `json:"status"`
	MetricEvent string `json:"metric_event"`
	TargetCount int    `json:"target_count"`
	WindowDays  int    `json:"window_days"`
	CreatedAt   string `json:"created_at"`
}

type listTestsOutput struct {
	Tests []listedTest `json:"tests"`
	// NextCursor continues the full-history walk when set.
	NextCursor string `json:"next_cursor,omitempty"`
	// Total is how many exist; Tests is a capped page of them. Reported so an
	// agent never says "you have three prototypes" while looking at page one of
	// forty.
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note"`
}

// listTests is the op that makes the plural surface answerable in chat. Without
// it, the only validation read an agent has is "the active test", so an owner
// who asks "how are my prototypes doing?" gets one of them and no hint that the
// other four exist.
func listTests() opcore.Operation[listTestsInput, listTestsOutput] {
	return opcore.Operation[listTestsInput, listTestsOutput]{
		Name:    "list_tests",
		Summary: "List the project's validation tests (prototypes) — open ones first — with the id each one is read by.",
		Scope:   "growth_suggest",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in listTestsInput) (listTestsOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return listTestsOutput{}, err
			}
			if in.Cursor != "" || in.Limit > 0 {
				page, next, perr := d.Repo.ListValidationTestsPage(ctx, cc.ProjectID, in.Cursor, in.Limit)
				if perr != nil {
					return listTestsOutput{}, perr
				}
				// Total is the authoritative window count from the same capped
				// read the unpaged list uses, so a page can never report a
				// number the list contract would not; Truncated is whether a
				// continuation was minted, not a guess from the page length.
				_, total, terr := d.Repo.ValidationTestsForProject(ctx, cc.ProjectID, 1)
				if terr != nil {
					return listTestsOutput{}, terr
				}
				out := listTestsOutput{Tests: make([]listedTest, 0, len(page)), NextCursor: next,
					Total: total, Truncated: next != ""}
				for _, t := range page {
					out.Tests = append(out.Tests, listedTest{
						TestID: t.ID, Hypothesis: t.Hypothesis, Status: t.Status, MetricEvent: t.MetricEvent,
						TargetCount: t.TargetCount, WindowDays: t.WindowDays, CreatedAt: t.CreatedAt.Format(time.RFC3339),
					})
				}
				return out, nil
			}
			tests, total, err := d.Repo.ValidationTestsForProject(ctx, cc.ProjectID, 0)
			if err != nil {
				return listTestsOutput{}, err
			}
			out := listTestsOutput{Tests: make([]listedTest, 0, len(tests)), Total: total, Truncated: total > len(tests)}
			waiting := 0
			for _, t := range tests {
				if t.Status == storage.TestProposed {
					waiting++
				}
				out.Tests = append(out.Tests, listedTest{
					TestID: t.ID, Hypothesis: t.Hypothesis, Status: t.Status, MetricEvent: t.MetricEvent,
					TargetCount: t.TargetCount, WindowDays: t.WindowDays, CreatedAt: t.CreatedAt.Format(time.RFC3339),
				})
			}
			switch {
			case total == 0:
				out.Note = "No prototypes yet. Design one with propose_test — one idea, one number, agreed before the data arrives."
			case waiting > 0:
				out.Note = fmt.Sprintf("%d of these are still only PROPOSED — the owner has not agreed to the number, "+
					"so nothing is being counted for them. Say which ones, and that they commit on /plans. "+
					"Call test_status with a test_id for the numbers on any one of them.", waiting)
			default:
				out.Note = "Call test_status with a test_id for how any one of these is doing. " +
					"Do not compare them by hypothesis alone — each has its own threshold."
			}
			if out.Truncated {
				out.Note += fmt.Sprintf(" Showing the %d most relevant of %d; say so rather than implying this is all of them.", len(out.Tests), total)
			}
			return out, nil
		},
	}
}

// verdictNote states what the number means in the owner's terms, including the
// two ways a count is not yet an answer: too early, and too little traffic to
// have tested anything.
func verdictNote(o testStatusOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d people have fired %s", o.MetricCount, o.TargetCount, o.MetricEvent)
	if o.BaselineCount > 0 {
		fmt.Fprintf(&b, " out of %d who saw the page", o.BaselineCount)
	}
	fmt.Fprintf(&b, "; %d days left. ", o.DaysLeft)
	switch o.Verdict {
	case storage.TestPassed:
		b.WriteString("The threshold the owner agreed to has been cleared — say so plainly and move to building.")
	case storage.TestFailed:
		b.WriteString("The window has closed short of the threshold. Before calling the idea dead, " +
			"say which of the three it was: no demand, wrong message, or too small a sample — " +
			"a test that reached few people has not tested the idea.")
	default:
		b.WriteString("Still running: too early to call, and you must not call it.")
	}
	return b.String()
}
