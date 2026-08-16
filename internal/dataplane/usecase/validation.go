package usecase

import (
	"context"
	"fmt"
	"strings"

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
		Name:    "propose_test",
		Summary: "Propose a validation test: the success event, how many distinct people must fire it, and by when. The owner commits to it before the data arrives.",
		Scope:   "growth_suggest",
		Handler: func(ctx context.Context, cc opcore.CallContext, in proposeTestInput) (proposeTestOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return proposeTestOutput{}, err
			}
			id, err := d.Repo.CreateValidationTest(ctx, storage.ValidationTest{
				ProjectID:     cc.ProjectID,
				RunID:         cc.RunID,
				Hypothesis:    in.Hypothesis,
				MetricEvent:   in.MetricEvent,
				BaselineEvent: in.BaselineEvent,
				TargetCount:   in.TargetCount,
				WindowDays:    in.WindowDays,
			})
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

type testStatusInput struct{}

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
}

// testStatus reads the live test against its committed threshold. This is a
// READ of project data — classified as evidence in runtime/policy.go, so an
// agent that answers "how is the test doing" from it is not nudged for
// unbacked figures, and one that answers without it is.
func testStatus() opcore.Operation[testStatusInput, testStatusOutput] {
	return opcore.Operation[testStatusInput, testStatusOutput]{
		Name:    "test_status",
		Summary: "Read the project's active validation test and how it is doing against the threshold agreed in advance.",
		Scope:   "growth_suggest",
		Handler: func(ctx context.Context, cc opcore.CallContext, _ testStatusInput) (testStatusOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return testStatusOutput{}, err
			}
			test, err := d.Repo.ActiveValidationTest(ctx, cc.ProjectID)
			if err != nil {
				return testStatusOutput{}, err
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
			}
			if test.BaselineEvent != "" && p.Baseline > 0 {
				out.ConversionPct = float64(p.Metric) / float64(p.Baseline) * 100
			}
			if test.Status == storage.TestProposed {
				out.Note = "This test is still only PROPOSED — the owner has not committed to the number, " +
					"so nothing is being counted yet. Ask them to commit it on /start?job=validate."
				return out, nil
			}
			out.Verdict = p.Verdict()
			out.Note = verdictNote(out)
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
