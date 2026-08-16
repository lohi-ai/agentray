package usecase

import (
	"context"
	"encoding/json"
	"errors"
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
	// byID / list back the plural reads. total is what the project HAS, which is
	// deliberately allowed to exceed len(list) so the truncation contract is
	// testable.
	byID  map[string]storage.ValidationTest
	list  []storage.ValidationTest
	total int
}

func (f *fakeValidationRepo) ValidationTestsForProject(_ context.Context, _ string, _ int) ([]storage.ValidationTest, int, error) {
	total := f.total
	if total == 0 {
		total = len(f.list)
	}
	return f.list, total, nil
}

func (f *fakeValidationRepo) ValidationTestForProject(_ context.Context, _, id string) (storage.ValidationTest, error) {
	t, ok := f.byID[id]
	if !ok {
		return storage.ValidationTest{}, errors.New("no test with that id in this project")
	}
	return t, nil
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

// --- the plural reads -------------------------------------------------------

// The whole point of test_id. With five prototypes on screen, an agent that
// silently answers about "the active one" reports a different experiment's
// numbers under the name the owner asked about — and it sounds exactly as
// confident doing it.
func TestTestStatusReadsTheTestItWasAskedFor(t *testing.T) {
	committed := time.Now().UTC().Add(-48 * time.Hour)
	asked := storage.ValidationTest{
		ID: "t2", Status: storage.TestCommitted, Hypothesis: "leaderboard pulls lapsed readers back",
		MetricEvent: "leaderboard.viewed", TargetCount: 50, WindowDays: 14, CommittedAt: &committed,
	}
	repo := &fakeValidationRepo{
		active:   &storage.ValidationTest{ID: "t1", Status: storage.TestCommitted, MetricEvent: "waitlist.joined", TargetCount: 40},
		byID:     map[string]storage.ValidationTest{"t2": asked},
		progress: storage.TestProgress{Test: asked, Metric: 9, DaysLeft: 5},
	}
	out := invokeOp(t, repo, "test_status", `{"test_id":"t2"}`)

	if out["test_id"] != "t2" {
		t.Fatalf("asked for t2, got %v", out["test_id"])
	}
	if out["metric_event"] != "leaderboard.viewed" {
		t.Errorf("answered about the wrong experiment: %v", out["metric_event"])
	}
}

// An id that resolves to nothing must NOT fall through to the active test. That
// fallback is the failure mode dressed as helpfulness: the owner asks about a
// prototype that was deleted and is told, with numbers, how a different one is
// doing.
func TestTestStatusRefusesToSubstituteAnotherTestForAnUnknownID(t *testing.T) {
	repo := &fakeValidationRepo{
		active: &storage.ValidationTest{ID: "t1", Status: storage.TestCommitted, MetricEvent: "waitlist.joined", TargetCount: 40},
		byID:   map[string]storage.ValidationTest{},
	}
	out := invokeOp(t, repo, "test_status", `{"test_id":"nope"}`)

	if out["has_test"] != false {
		t.Fatal("an unknown id must report no test, never the active one")
	}
	if out["test_id"] == "t1" || out["metric_event"] == "waitlist.joined" {
		t.Fatalf("substituted the active test for an unknown id: %v", out)
	}
	if !strings.Contains(out["note"].(string), "list_tests") {
		t.Errorf("the note must point at the repair, got %q", out["note"])
	}
}

// A closed test's verdict is what the OWNER called it. Recomputing it means a
// late-arriving event can turn a test they killed into a pass the product now
// claims — rewriting a decision after the fact, which is the exact dishonesty
// the committed threshold exists to prevent.
func TestTestStatusReportsTheOwnersDecisionNotARecount(t *testing.T) {
	committed := time.Now().UTC().Add(-30 * 24 * time.Hour)
	decided := time.Now().UTC().Add(-2 * 24 * time.Hour)
	closed := storage.ValidationTest{
		ID: "t3", Status: storage.TestFailed, MetricEvent: "notify.enabled", TargetCount: 60,
		WindowDays: 14, CommittedAt: &committed, DecidedAt: &decided, DecisionNote: "wrong audience",
	}
	repo := &fakeValidationRepo{
		byID: map[string]storage.ValidationTest{"t3": closed},
		// The counts now clear the threshold. The verdict must not follow them.
		progress: storage.TestProgress{Test: closed, Metric: 200, DaysLeft: 0},
	}
	out := invokeOp(t, repo, "test_status", `{"test_id":"t3"}`)

	if out["verdict"] != storage.TestFailed {
		t.Fatalf("a decided test keeps the owner's verdict, got %v", out["verdict"])
	}
	if !strings.Contains(out["note"].(string), "wrong audience") {
		t.Errorf("the decision note is the part worth reading later, got %q", out["note"])
	}
}

// A proposal has no window — the clock starts at commit — so its counts are
// meaningless. Reporting one beside a note saying nothing is counted is how an
// agent ends up quoting a number nobody agreed to as progress.
func TestTestStatusCountsNothingForAnUncommittedTest(t *testing.T) {
	proposed := storage.ValidationTest{ID: "t4", Status: storage.TestProposed, MetricEvent: "audio.preview_played", TargetCount: 40}
	repo := &fakeValidationRepo{
		byID:     map[string]storage.ValidationTest{"t4": proposed},
		progress: storage.TestProgress{Test: proposed, Metric: 17, Baseline: 400, DaysLeft: 9},
	}
	out := invokeOp(t, repo, "test_status", `{"test_id":"t4"}`)

	if out["metric_count"].(float64) != 0 {
		t.Errorf("an uncommitted test counts nothing, got %v", out["metric_count"])
	}
	if out["verdict"] != nil {
		t.Errorf("an uncommitted test has no verdict, got %v", out["verdict"])
	}
}

func TestListTestsNamesEveryPrototypeAndFlagsTheOnesWaitingOnTheOwner(t *testing.T) {
	repo := &fakeValidationRepo{list: []storage.ValidationTest{
		{ID: "t1", Status: storage.TestProposed, Hypothesis: "audio upsell", MetricEvent: "audio.preview_played", TargetCount: 40},
		{ID: "t2", Status: storage.TestCommitted, Hypothesis: "glossary report", MetricEvent: "waitlist.joined", TargetCount: 40},
		{ID: "t3", Status: storage.TestPassed, Hypothesis: "per-novel notifications", MetricEvent: "notify.enabled", TargetCount: 60},
	}}
	out := invokeOp(t, repo, "list_tests", `{}`)

	tests := out["tests"].([]any)
	if len(tests) != 3 {
		t.Fatalf("every prototype must be listed, got %d", len(tests))
	}
	if out["total"].(float64) != 3 || out["truncated"] != false {
		t.Errorf("a complete page must not read as truncated: %v", out)
	}
	first := tests[0].(map[string]any)
	if first["test_id"] != "t1" {
		t.Errorf("the list must carry the id test_status is called with, got %v", first)
	}
	if !strings.Contains(out["note"].(string), "PROPOSED") {
		t.Errorf("a proposal blocks on the human and the note must say so, got %q", out["note"])
	}
}

// A capped page that reads as the whole list is the lie this endpoint is most
// likely to tell: the agent says "you have three prototypes" while forty exist.
func TestListTestsSaysWhenItIsShowingAPage(t *testing.T) {
	repo := &fakeValidationRepo{
		list:  []storage.ValidationTest{{ID: "t1", Status: storage.TestCommitted, Hypothesis: "one", MetricEvent: "e", TargetCount: 1}},
		total: 40,
	}
	out := invokeOp(t, repo, "list_tests", `{}`)

	if out["truncated"] != true || out["total"].(float64) != 40 {
		t.Fatalf("a short page of a long list must be marked, got %v", out)
	}
	if !strings.Contains(out["note"].(string), "of 40") {
		t.Errorf("the note must state the real total, got %q", out["note"])
	}
}

func TestListTestsWithNothingPointsAtProposeTest(t *testing.T) {
	out := invokeOp(t, &fakeValidationRepo{}, "list_tests", `{}`)
	if out["total"].(float64) != 0 {
		t.Fatalf("empty project must report zero, got %v", out["total"])
	}
	if !strings.Contains(out["note"].(string), "propose_test") {
		t.Errorf("the empty note must name the repair, got %q", out["note"])
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
