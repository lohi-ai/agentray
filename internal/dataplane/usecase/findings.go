package usecase

import (
	"context"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/findings"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// --- Deterministic findings engine ops (ticket 001) ---
//
// run_findings_scan drives the rule-based detector pass on demand; the
// scheduled pass rides the scheduler tick in app.go. watch_funnel declares a
// funnel for the drop-off detector — there is no saved-funnel entity, so the
// watch row is the durable "this sequence matters" the detector compares
// windows against.

type runFindingsScanOutput struct {
	Scan findings.ScanResult `json:"scan"`
}

func runFindingsScan() opcore.Operation[struct{}, runFindingsScanOutput] {
	return opcore.Operation[struct{}, runFindingsScanOutput]{
		Name:           "run_findings_scan",
		Summary:        "Run the deterministic findings detectors over this project now and write any firings as findings. No agent or LLM involved.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, _ struct{}) (runFindingsScanOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return runFindingsScanOutput{}, err
			}
			// The Repo every adapter injects already declares the per-project
			// surface a scan needs; a build whose Repo doesn't (a narrower
			// test double) reports unavailable rather than panicking.
			st, ok := d.Repo.(findings.ProjectStore)
			if !ok {
				return runFindingsScanOutput{}, errBadInput("findings scan is not available on this surface")
			}
			res, err := findings.ScanProject(ctx, st, cc.ProjectID, time.Now().UTC())
			if err != nil {
				return runFindingsScanOutput{}, err
			}
			return runFindingsScanOutput{Scan: res}, nil
		},
	}
}

type watchFunnelInput struct {
	Name  string   `json:"name" desc:"label for the watch (defaults to the step list)"`
	Steps []string `json:"steps" desc:"ordered event names, at least 2" required:"true"`
}

func watchFunnel() opcore.Operation[watchFunnelInput, storage.FunnelWatch] {
	return opcore.Operation[watchFunnelInput, storage.FunnelWatch]{
		Name:           "watch_funnel",
		Summary:        "Declare an ordered event sequence for the findings engine to watch: each scan re-runs it over the current and prior 7-day windows and files a finding when a step's conversion drops.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in watchFunnelInput) (storage.FunnelWatch, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.FunnelWatch{}, err
			}
			w, err := d.Repo.CreateFunnelWatch(ctx, cc.ProjectID, in.Name, in.Steps)
			if err != nil {
				return storage.FunnelWatch{}, err
			}
			return w, nil
		},
	}
}

func listFunnelWatches() opcore.Operation[struct{}, []storage.FunnelWatch] {
	return opcore.Operation[struct{}, []storage.FunnelWatch]{
		Name:    "list_funnel_watches",
		Summary: "List the funnels the findings engine watches for conversion drops.",
		Scope:   "monitor",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, _ struct{}) ([]storage.FunnelWatch, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return nil, err
			}
			return d.Repo.FunnelWatchesForProject(ctx, cc.ProjectID)
		},
	}
}
