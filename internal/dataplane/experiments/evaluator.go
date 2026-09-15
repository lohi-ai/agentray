// Package experiments is the scheduled auto-close for validation tests: a
// committed test whose review_date has arrived is re-measured against the
// threshold the owner agreed to, gets an outcome entry citing the measured
// window, and lands in a terminal state — passed, failed, or inconclusive when
// the review date beat the window end. No agent, no LLM: the same
// TestProgress.Verdict() the surfaces quote decides the outcome, so the
// auto-close can never disagree with the number the owner was watching.
//
// The evaluator rides the scheduler's minute tick beside the findings scanner
// and alert evaluator (app.go); run_experiment_review drives the same pass
// for one project on demand.
package experiments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// reviewBudget bounds one scheduled pass over every due test; a pass that
// outlives it is abandoned and the still-committed tests are simply due again
// on the next tick — the idempotency keys make a retried close a replay.
const reviewBudget = 2 * time.Minute

// ProjectStore is the per-project surface a review needs. usecase.Repo already
// declares every method, so the run_experiment_review op can drive a review
// through the same Repo value every adapter injects — no extra wiring.
// *storage.Store satisfies it for the scheduled pass.
type ProjectStore interface {
	DueValidationTests(ctx context.Context, projectID string, now time.Time) ([]storage.ValidationTest, error)
	ValidationTestProgress(ctx context.Context, t storage.ValidationTest) (storage.TestProgress, error)
	MetricDefinitionByKey(ctx context.Context, key string) (storage.MetricDefinition, error)
	Overview(ctx context.Context, projectID, period, platform string, now time.Time) (storage.OverviewResult, error)
	CountEventPeople(ctx context.Context, projectID, eventName string, from, to time.Time) (int, error)
	CloseValidationTestSystemIdempotent(ctx context.Context, projectID, id, status, note string, entry storage.TestOutcomeEntry, idemKey, requestHash string) (storage.ValidationTest, error)
}

// ClosedTest is one test the pass decided, with the measurement behind the
// call — what the op reports back and what the decision note cites.
type ClosedTest struct {
	TestID       string `json:"test_id"`
	Hypothesis   string `json:"hypothesis"`
	Status       string `json:"status"` // passed | failed | inconclusive
	Metric       int    `json:"metric_count"`
	Target       int    `json:"target_count"`
	Window       string `json:"window"`
	DecisionNote string `json:"decision_note"`
}

// ReviewResult reports one pass: how many tests were due, which ones closed,
// and which ones failed to close (left committed for the next tick).
type ReviewResult struct {
	ProjectID string       `json:"project_id"`
	Due       int          `json:"due"`
	Closed    []ClosedTest `json:"closed"`
	// Errors names the tests the pass could not close — a measurement or
	// write failure leaves the test committed, so it is retried, not lost.
	Errors []string `json:"errors,omitempty"`
}

// Evaluator is the scheduled engine. It rides the scheduler's minute tick,
// admitting at most one pass at a time; the due-read, not the tick, decides
// which tests are reviewed.
type Evaluator struct {
	store ProjectStore

	mu      sync.Mutex
	running bool
}

func NewEvaluator(store ProjectStore) *Evaluator {
	return &Evaluator{store: store}
}

// Tick admits at most one review pass at a time and returns immediately — the
// pass runs on its own goroutine under its own budget, so a slow measurement
// can never hold the minute clock the alert evaluator and connector syncs
// share.
func (e *Evaluator) Tick(ctx context.Context, now time.Time) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.mu.Unlock()

	// The pass must outlive the tick that admitted it: a hook whose context
	// dies on return would abort the review it just started.
	reviewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reviewBudget)
	go func() {
		defer cancel()
		defer func() {
			e.mu.Lock()
			e.running = false
			e.mu.Unlock()
		}()
		res, err := e.review(reviewCtx, "", now.UTC())
		if err != nil {
			fmt.Printf("experiments: review pass failed: %v\n", err)
			return
		}
		for _, msg := range res.Errors {
			fmt.Printf("experiments: %s\n", msg)
		}
	}()
}

// Review runs the due-test pass over one project synchronously — the
// run_experiment_review op's path. The same code the tick runs, so a manual
// review and the schedule can never disagree about what "due" means.
func (e *Evaluator) Review(ctx context.Context, projectID string, now time.Time) (ReviewResult, error) {
	return e.review(ctx, projectID, now.UTC())
}

// ReviewProject is the package-level form for callers that hold a store but
// no Evaluator — the op uses it so a narrower Repo never has to construct one.
func ReviewProject(ctx context.Context, st ProjectStore, projectID string, now time.Time) (ReviewResult, error) {
	return (&Evaluator{store: st}).review(ctx, projectID, now.UTC())
}

func (e *Evaluator) review(ctx context.Context, projectID string, now time.Time) (ReviewResult, error) {
	res := ReviewResult{ProjectID: projectID, Closed: []ClosedTest{}}
	due, err := e.store.DueValidationTests(ctx, projectID, now)
	if err != nil {
		return res, fmt.Errorf("experiments: due read: %w", err)
	}
	res.Due = len(due)
	for _, t := range due {
		closed, err := e.closeOne(ctx, t, now)
		switch {
		case errors.Is(err, storage.ErrTestNoLongerCommitted):
			// The owner's decide or an earlier pass won the race — nothing to
			// retry, nothing to report.
		case err != nil:
			res.Errors = append(res.Errors, fmt.Sprintf("close %s: %v", t.ID, err))
		default:
			res.Closed = append(res.Closed, *closed)
		}
	}
	return res, nil
}

// closeOne re-measures one due test and writes the outcome + terminal status
// in a single idempotent transaction.
func (e *Evaluator) closeOne(ctx context.Context, t storage.ValidationTest, now time.Time) (*ClosedTest, error) {
	if t.ReviewDate == nil {
		// DueValidationTests guarantees this; the guard keeps a direct call
		// honest instead of panicking on a nil dereference.
		return nil, fmt.Errorf("test %s has no review date", t.ID)
	}
	p, err := e.store.ValidationTestProgress(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("measure: %w", err)
	}

	// The measured window opens at commit and closes at the earlier of now and
	// the committed window end — the same bounds ValidationTestProgress counts.
	from := t.CreatedAt
	if t.CommittedAt != nil {
		from = *t.CommittedAt
	}
	until := from.AddDate(0, 0, t.WindowDays)
	if now.Before(until) {
		until = now
	}
	window := fmt.Sprintf("%s – %s", from.UTC().Format("2006-01-02"), until.UTC().Format("2006-01-02"))

	// The verdict the surfaces quote is the classification: passed and failed
	// close as themselves; a still-open window at review date closes
	// inconclusive — measured, not yet answerable.
	status := p.Verdict()
	if status == storage.TestCommitted {
		status = storage.TestInconclusive
	}

	note := decisionNote(t, p, status, from, until)
	guardrail, gerr := e.guardrail(ctx, t, from, until)
	if gerr != nil {
		// A guardrail that cannot be measured is reported, not silently
		// dropped — the close still lands, the note says the check did not run.
		guardrail = fmt.Sprintf("guardrail %q could not be measured: %v", t.GuardrailMetric, gerr)
	}
	if guardrail != "" {
		note += " " + guardrail
	}

	entry := storage.TestOutcomeEntry{
		Value:       float64(p.Metric),
		Unit:        fmt.Sprintf("people firing %s (target %d)", t.MetricEvent, t.TargetCount),
		Window:      window,
		EvidenceRef: fmt.Sprintf("event:%s@%s/%s", t.MetricEvent, from.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)),
		AuthorKind:  "system",
		AuthorID:    "experiment-review",
		RecordedAt:  now.UTC().Format(time.RFC3339),
		Note:        note,
	}

	// The claim is about the review, not the measurement: hashing the test id
	// and review date means a crash between append and status write replays
	// the stored receipt instead of conflicting on a re-measured count.
	key := fmt.Sprintf("autoclose:%s:%s", t.ID, t.ReviewDate.UTC().Format("2006-01-02"))
	hash := reviewHash(t.ID, *t.ReviewDate)
	closed, err := e.store.CloseValidationTestSystemIdempotent(ctx, t.ProjectID, t.ID, status, note, entry, key, hash)
	if err != nil {
		return nil, err
	}
	return &ClosedTest{
		TestID: closed.ID, Hypothesis: closed.Hypothesis, Status: closed.Status,
		Metric: p.Metric, Target: t.TargetCount, Window: window, DecisionNote: note,
	}, nil
}

// decisionNote is the "why" a reader gets a month later: the measured number
// against the agreed number, and — for an inconclusive close — the reason the
// review fired before the window could answer.
func decisionNote(t storage.ValidationTest, p storage.TestProgress, status string, from, until time.Time) string {
	head := fmt.Sprintf("Auto-closed at review date %s: %d of %d people fired %s over %s – %s.",
		t.ReviewDate.UTC().Format("2006-01-02"), p.Metric, t.TargetCount, t.MetricEvent,
		from.UTC().Format("2006-01-02"), until.UTC().Format("2006-01-02"))
	switch status {
	case storage.TestPassed:
		return head + " The committed threshold was cleared."
	case storage.TestFailed:
		return head + " The committed window closed short of the threshold."
	default:
		windowEnd := from.AddDate(0, 0, t.WindowDays)
		return head + fmt.Sprintf(" The committed window is still open (ends %s) — closed inconclusive: measured, not yet answerable.",
			windowEnd.UTC().Format("2006-01-02"))
	}
}

// guardrail measures the test's guardrail metric over the post-commit window
// against the prior equal window, and reports the delta in one sentence. A
// catalog metric key reads through the overview (the one deterministic
// implementation, which already computes the prior-window comparison);
// anything else is treated as an event name and counted as distinct people —
// the same population the threshold is judged on. Adverse is only claimed when
// a direction is known: a declared metric target that judged the window
// off_track. Free-text metrics report the delta without a verdict.
func (e *Evaluator) guardrail(ctx context.Context, t storage.ValidationTest, from, until time.Time) (string, error) {
	key := strings.TrimSpace(t.GuardrailMetric)
	if key == "" {
		return "", nil
	}
	days := int(until.Sub(from).Hours() / 24)
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}
	span := until.Sub(from)

	def, err := e.store.MetricDefinitionByKey(ctx, key)
	if err != nil {
		if !errors.Is(err, storage.ErrMetricUnknown) {
			return "", err
		}
		// Not a catalog key — read it as an event name.
		cur, err := e.store.CountEventPeople(ctx, t.ProjectID, key, from, until)
		if err != nil {
			return "", err
		}
		prior, err := e.store.CountEventPeople(ctx, t.ProjectID, key, from.Add(-span), from)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Guardrail %s: %d people this window, %d the prior equal window (no declared direction — delta reported, not judged).",
			key, cur, prior), nil
	}

	res, err := e.store.Overview(ctx, t.ProjectID, fmt.Sprintf("%dd", days), "", until)
	if err != nil {
		return "", err
	}
	reading, err := storage.MetricReadingFor(def, res)
	if err != nil {
		return "", err
	}
	if reading.State != storage.OverviewStateOK {
		return fmt.Sprintf("Guardrail %s: %s — no reading to compare.", key, reading.State), nil
	}
	var cur, prior float64
	unit := reading.Unit
	switch {
	case reading.Rate != nil:
		cur = *reading.Rate
		// Rates carry no prior-window comparison on the reading; the prior
		// window's own read supplies it.
		priorRes, err := e.store.Overview(ctx, t.ProjectID, fmt.Sprintf("%dd", days), "", until.Add(-span))
		if err != nil {
			return "", err
		}
		priorReading, err := storage.MetricReadingFor(def, priorRes)
		if err != nil {
			return "", err
		}
		if priorReading.Rate != nil {
			prior = *priorReading.Rate
		}
	case reading.Value != nil:
		cur = float64(*reading.Value)
		if reading.Previous != nil {
			prior = float64(*reading.Previous)
		}
	default:
		return fmt.Sprintf("Guardrail %s: no comparable value in the reading.", key), nil
	}
	adverse := ""
	if reading.Target != nil && reading.Target.Verdict == storage.TargetVerdictOffTrack {
		adverse = " ADVERSE — its declared target judged this window off_track."
	}
	return fmt.Sprintf("Guardrail %s: %s %s this window vs %s the prior equal window.%s",
		key, formatNumber(cur), unit, formatNumber(prior), adverse), nil
}

func formatNumber(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}

// reviewHash is the deterministic request hash for the auto-close claim: the
// test and the review it answers, never the measured payload — a re-measurement
// after a crash must replay the receipt, not conflict with it.
func reviewHash(testID string, reviewDate time.Time) string {
	sum := sha256.Sum256([]byte(testID + "|" + reviewDate.UTC().Format(time.RFC3339)))
	return hex.EncodeToString(sum[:])
}
