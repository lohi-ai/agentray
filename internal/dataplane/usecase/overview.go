package usecase

import (
	"context"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// overview.go — the `overview` operation: one deterministic Product Overview
// read, defined once here and projected to the in-process agent tool, REST
// (/api/op/overview), CLI, and MCP (tools/call) by the shared opcore adapters.
// GET /api/overview in internal/app/overview_routes.go is a thin convenience
// adapter over this same operation — there is no second contract.
//
// Access note for the credential-split merge: this operation belongs to
// opcore.AccessAnalyticsRead. The Access field does not exist on this branch
// yet (it lands with the MCP-parity work); whoever merges second adds
// `Access: opcore.AccessAnalyticsRead` — the fail-closed default means a
// missing field denies remote callers, never over-exposes.

type overviewInput struct {
	// Period is "Nd" (N complete UTC days ending at the last UTC midnight,
	// 1-90) or "today" for the partial current day. Empty means "7d" — there
	// is no ambiguous integer zero in the contract.
	Period   string `json:"period" desc:"complete-day window: \"7d\" (default), \"Nd\" 1-90, or \"today\" (partial)"`
	Platform string `json:"platform" desc:"web | ios | android | server | unknown; empty = all platforms"`
}

func overview() opcore.Operation[overviewInput, storage.OverviewResult] {
	return opcore.Operation[overviewInput, storage.OverviewResult]{
		Name:    "overview",
		Summary: "Product overview: active/new users, sessions, retention, top pages and sources, and data status over a complete-day window.",
		Scope:   "monitor",
		Handler: func(ctx context.Context, cc opcore.CallContext, in overviewInput) (storage.OverviewResult, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.OverviewResult{}, err
			}
			return d.Repo.Overview(ctx, cc.ProjectID, in.Period, in.Platform, time.Now())
		},
	}
}
