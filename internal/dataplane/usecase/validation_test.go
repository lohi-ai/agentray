package usecase

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// fakeValidationRepo answers the two validation ops and nothing else — the
// embedded nil Repo makes any other call panic, so a test proves exactly which
// data an operation touched.
type fakeValidationRepo struct {
	Repo
	created  storage.ValidationTest
	active   *storage.ValidationTest
	progress storage.TestProgress
	waitlist int
}

func (f *fakeValidationRepo) CreateValidationTest(_ context.Context, t storage.ValidationTest) (string, error) {
	f.created = t
	return "test-1", nil
}

func (f *fakeValidationRepo) ActiveValidationTest(context.Context, string) (*storage.ValidationTest, error) {
	return f.active, nil
}

func (f *fakeValidationRepo) ValidationTestProgress(context.Context, storage.ValidationTest) (storage.TestProgress, error) {
	return f.progress, nil
}

func (f *fakeValidationRepo) CountWaitlistSignups(context.Context, string) (int, error) {
	return f.waitlist, nil
}

func invokeOp(t *testing.T, repo Repo, name, args string) map[string]any {
	t.Helper()
	reg := Registry()
	spec, ok := reg.Get(name)
	if !ok {
		t.Fatalf("registry missing %q", name)
	}
	out, err := spec.OpInvoke(context.Background(), opcore.CallContext{
		ProjectID: "p1", Deps: &Deps{Repo: repo},
	}, args)
	if err != nil {
		t.Fatalf("%s invoke: %v", name, err)
	}
	// OpInvoke returns the operation's rendered JSON — the exact bytes the model
	// is shown — so asserting on it is asserting on what the agent actually
	// reads, not on an in-process struct it never sees.
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("unmarshal %s output %q: %v", name, out, err)
	}
	return m
}

// An agent may DESIGN a test; only the owner can agree to be bound by one. If
// propose_test could create a committed test, the threshold would be something
// the product asserted rather than something the owner promised — and the whole
// point of writing it down is that the owner cannot move it later.
func TestProposeTestNeverCommitsOnTheOwnersBehalf(t *testing.T) {
	repo := &fakeValidationRepo{}
	out := invokeOp(t, repo, "propose_test",
		`{"hypothesis":"solo sellers want restock alerts","metric_event":"waitlist.joined","target_count":40,"baseline_event":"user.pageview","window_days":14}`)

	if got := out["status"]; got != storage.TestProposed {
		t.Errorf("a proposed test must land as %q, got %v", storage.TestProposed, got)
	}
	if repo.created.TargetCount != 40 || repo.created.MetricEvent != "waitlist.joined" {
		t.Errorf("threshold not passed through: %+v", repo.created)
	}
	if repo.created.WindowDays != 14 {
		t.Errorf("window not passed through: %d", repo.created.WindowDays)
	}
}

// propose_test must not be terminal. Designing the test is the middle of a
// conversation; ending the run there leaves the owner holding a row nobody
// explained and no chance to push back on the number.
func TestProposeTestDoesNotEndTheRun(t *testing.T) {
	spec, _ := Registry().Get("propose_test")
	if spec.OpTerminal() {
		t.Error("propose_test must not be terminal — the owner still has to be told what was proposed")
	}
}

func TestTestStatusWithNoTestSaysSoInsteadOfReportingZero(t *testing.T) {
	out := invokeOp(t, &fakeValidationRepo{waitlist: 3}, "test_status", `{}`)
	if out["has_test"] != false {
		t.Fatal("has_test must be false when the project has never proposed one")
	}
	if out["waitlist_count"].(float64) != 3 {
		t.Errorf("waitlist count must be reported even with no test, got %v", out["waitlist_count"])
	}
	if !strings.Contains(out["note"].(string), "propose_test") {
		t.Errorf("the note must point at the repair, got %q", out["note"])
	}
}

// A test still awaiting the owner's commit is counting nothing. Reporting
// progress on it would show a number the owner never agreed to.
func TestTestStatusFlagsAnUncommittedTest(t *testing.T) {
	repo := &fakeValidationRepo{active: &storage.ValidationTest{
		ID: "t1", Status: storage.TestProposed, MetricEvent: "waitlist.joined", TargetCount: 40,
	}}
	out := invokeOp(t, repo, "test_status", `{}`)
	if out["verdict"] != nil {
		t.Errorf("a proposed test has no verdict, got %v", out["verdict"])
	}
	if !strings.Contains(out["note"].(string), "PROPOSED") {
		t.Errorf("the note must say the number is not yet agreed, got %q", out["note"])
	}
}

func TestTestStatusReportsProgressAgainstTheAgreedNumber(t *testing.T) {
	committed := time.Now().UTC().Add(-72 * time.Hour)
	test := storage.ValidationTest{
		ID: "t1", Status: storage.TestCommitted, MetricEvent: "waitlist.joined",
		BaselineEvent: "user.pageview", TargetCount: 40, WindowDays: 14, CommittedAt: &committed,
	}
	repo := &fakeValidationRepo{
		active:   &test,
		progress: storage.TestProgress{Test: test, Metric: 12, Baseline: 300, DaysElapsed: 3, DaysLeft: 11},
		waitlist: 12,
	}
	out := invokeOp(t, repo, "test_status", `{}`)

	if out["metric_count"].(float64) != 12 || out["target_count"].(float64) != 40 {
		t.Errorf("counts wrong: %v of %v", out["metric_count"], out["target_count"])
	}
	if pct := out["conversion_pct"].(float64); pct < 3.99 || pct > 4.01 {
		t.Errorf("12/300 is 4%%, got %v", pct)
	}
	// Still inside the window and short of target: the honest answer is "too
	// early", and an agent that calls it either way is calling it on noise.
	if out["verdict"] != storage.TestCommitted {
		t.Errorf("an open window must not be decided, got %v", out["verdict"])
	}
	if !strings.Contains(out["note"].(string), "too early") {
		t.Errorf("the note must forbid an early call, got %q", out["note"])
	}
}

// A test with no denominator reports -1, not 0. A real 0%% conversion and "we
// never measured traffic" are opposite findings, and collapsing them lets an
// agent report a catastrophic-looking rate for a test that measured nothing.
func TestTestStatusDistinguishesNoBaselineFromZeroConversion(t *testing.T) {
	committed := time.Now().UTC().Add(-24 * time.Hour)
	test := storage.ValidationTest{
		ID: "t1", Status: storage.TestCommitted, MetricEvent: "waitlist.joined",
		TargetCount: 40, WindowDays: 14, CommittedAt: &committed,
	}
	repo := &fakeValidationRepo{
		active:   &test,
		progress: storage.TestProgress{Test: test, Metric: 5, Baseline: 0, DaysLeft: 13},
	}
	out := invokeOp(t, repo, "test_status", `{}`)
	if out["conversion_pct"].(float64) != -1 {
		t.Errorf("no baseline must report -1, got %v", out["conversion_pct"])
	}
}

// The failed note is the one place the product can stop a founder killing a good
// idea on a bad tweet: a missed threshold has three causes and only one of them
// is "nobody wants this".
func TestFailedVerdictNoteNamesTheThreeCauses(t *testing.T) {
	note := verdictNote(testStatusOutput{
		Verdict: storage.TestFailed, MetricEvent: "waitlist.joined",
		MetricCount: 4, TargetCount: 40, BaselineCount: 90,
	})
	for _, want := range []string{"no demand", "wrong message", "too small a sample"} {
		if !strings.Contains(note, want) {
			t.Errorf("a failed test must not be read as dead demand by default; missing %q in %q", want, note)
		}
	}
}
