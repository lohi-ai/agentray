package usecase

import (
	"context"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/experiments"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// --- Experiment auto-close (ticket 002) ---
//
// run_experiment_review drives the scheduled re-measurement pass on demand for
// one project: every committed test whose review_date has arrived is
// re-measured against its committed threshold and closed — passed, failed, or
// inconclusive when the review date beat the window end — with the measured
// outcome appended. The scheduled pass rides the scheduler tick in app.go;
// this op is the verification path and the agent surface for the same code.

type runExperimentReviewOutput struct {
	Review experiments.ReviewResult `json:"review"`
}

func runExperimentReview() opcore.Operation[struct{}, runExperimentReviewOutput] {
	return opcore.Operation[struct{}, runExperimentReviewOutput]{
		Name:           "run_experiment_review",
		Summary:        "Re-measure every committed experiment whose review date has arrived and close it with the measured outcome cited (passed / failed / inconclusive). The scheduled pass does this on its own; this runs it now.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, _ struct{}) (runExperimentReviewOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return runExperimentReviewOutput{}, err
			}
			// The Repo every adapter injects already declares the per-project
			// surface a review needs; a build whose Repo doesn't (a narrower
			// test double) reports unavailable rather than panicking.
			st, ok := d.Repo.(experiments.ProjectStore)
			if !ok {
				return runExperimentReviewOutput{}, errBadInput("experiment review is not available on this surface")
			}
			res, err := experiments.ReviewProject(ctx, st, cc.ProjectID, time.Now().UTC())
			if err != nil {
				return runExperimentReviewOutput{}, err
			}
			return runExperimentReviewOutput{Review: res}, nil
		},
	}
}
