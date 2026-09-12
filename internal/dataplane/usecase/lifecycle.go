package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Lifecycle operations (redesign slice 2): dashboard update/archive with
// revision checks and idempotent retries, plus SDK verification over the
// existing bounded event read. Registered in analytics.go's Registry beside
// the analytics operations; this file owns their definitions so the metrics
// worker's analytics.go changes stay isolated.

// requestHash fingerprints the operation input for the idempotency claim — a
// reused key with a different payload conflicts instead of replaying.
func requestHash(in any) (string, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// --- verify_sdk ---

type verifySDKInput struct {
	EventName   string `json:"event_name" desc:"expected test event name (optional — any recent event verifies)"`
	WithinHours int    `json:"within_hours" desc:"how far back to look, 1-24 (default 2)"`
}

type verifySDKOutput struct {
	Found      bool   `json:"found"`
	EventName  string `json:"event_name,omitempty"`
	ReceivedAt string `json:"received_at,omitempty"`
	Platform   string `json:"platform,omitempty"`
	// IdentityLinked is true only when an explicit identify/alias link exists
	// for the event's distinct id — anonymous id shapes vary across SDKs, so
	// presence of an id alone proves nothing.
	IdentityLinked bool `json:"identity_linked"`
	// Searched discloses the bounded scan behind a not-found answer.
	Searched int      `json:"searched"`
	Warnings []string `json:"warnings"`
}

// verify_sdk answers "did my instrumentation arrive?" over the same bounded
// recent-event read the analytics surface uses — no metric SQL, no new query
// path. Capture credentials are denied by the access class; a session or an
// analytics:read management credential verifies.
func verifySDK() opcore.Operation[verifySDKInput, verifySDKOutput] {
	return opcore.Operation[verifySDKInput, verifySDKOutput]{
		Name:    "verify_sdk",
		Summary: "Verify SDK installation: check whether a recent event (optionally by name) arrived for this project, with platform and identity details.",
		Access:  opcore.AccessAnalyticsRead,
		Scope:   "monitor",
		Handler: func(ctx context.Context, cc opcore.CallContext, in verifySDKInput) (verifySDKOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return verifySDKOutput{}, err
			}
			hours := in.WithinHours
			if hours <= 0 || hours > 24 {
				hours = 2
			}
			const searchLimit = 50
			// Arrival semantics: the read filters and orders on inserted_at —
			// when the pipeline accepted the event — so a delayed/offline
			// event with a stale occurred timestamp still verifies.
			since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
			events, err := d.Repo.RecentEventsForVerification(ctx, cc.ProjectID, searchLimit, since)
			if err != nil {
				return verifySDKOutput{}, err
			}
			want := strings.TrimSpace(in.EventName)
			out := verifySDKOutput{Warnings: []string{}, Searched: len(events)}
			for _, ev := range events {
				if want != "" && ev.EventName != want {
					continue
				}
				out.Found = true
				out.EventName = ev.EventName
				if ev.InsertedAt != nil {
					out.ReceivedAt = ev.InsertedAt.UTC().Format(time.RFC3339)
				} else {
					out.Warnings = append(out.Warnings, "event has no receipt timestamp — received_at unavailable")
				}
				out.Platform = ev.Platform // persisted column, not a properties guess
				linked, lerr := d.Repo.DistinctIDLinked(ctx, cc.ProjectID, ev.DistinctID)
				if lerr != nil {
					out.Warnings = append(out.Warnings, "identity link check failed — could not verify identify()")
				} else {
					out.IdentityLinked = linked
				}
				if out.Platform == "" || out.Platform == "unknown" {
					out.Warnings = append(out.Warnings, "event has no platform — check the SDK's platform field")
				}
				if !out.IdentityLinked {
					out.Warnings = append(out.Warnings, "no identify/alias link found for this distinct id — if this event should belong to a known user, call identify()")
				}
				return out, nil
			}
			if want != "" {
				out.Warnings = append(out.Warnings, fmt.Sprintf("no event named %q arrived in the last %dh (searched %d most recent arrivals) — check the event name and that capture is reaching this project", want, hours, len(events)))
			} else {
				out.Warnings = append(out.Warnings, fmt.Sprintf("no events arrived in the last %dh (searched %d most recent arrivals) — check the SDK key and capture endpoint", hours, len(events)))
			}
			return out, nil
		},
	}
}

// --- update_dashboard ---

type updateDashboardInput struct {
	DashboardID    string  `json:"dashboard_id" required:"true" desc:"dashboard to update"`
	Name           *string `json:"name" desc:"new name — omit to keep current; empty resets to Untitled"`
	Description    *string `json:"description" desc:"new description — omit to keep current; empty clears"`
	Revision       int64   `json:"revision" required:"true" desc:"expected current revision — stale revisions conflict"`
	IdempotencyKey string  `json:"idempotency_key" desc:"retry key — a repeated identical request returns the first result"`
}

func updateDashboard() opcore.Operation[updateDashboardInput, storage.Dashboard] {
	return opcore.Operation[updateDashboardInput, storage.Dashboard]{
		Name:           "update_dashboard",
		Summary:        "Update a dashboard's name/description. Requires the current revision; a stale revision returns a conflict instead of overwriting.",
		Access:         opcore.AccessDashboardsWrite,
		Scope:          "analyze_build",
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in updateDashboardInput) (storage.Dashboard, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Dashboard{}, err
			}
			if in.Revision <= 0 {
				return storage.Dashboard{}, fmt.Errorf("revision must be the dashboard's current revision (> 0)")
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.Dashboard{}, err
			}
			return d.Repo.UpdateDashboardIdempotent(ctx, cc.ProjectID, in.DashboardID, in.Name, in.Description, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}

// --- archive_dashboard ---

type archiveDashboardInput struct {
	DashboardID    string `json:"dashboard_id" required:"true" desc:"dashboard to archive (soft — charts and data are kept)"`
	Revision       int64  `json:"revision" required:"true" desc:"expected current revision — stale revisions conflict; an already-archived dashboard returns its current state"`
	IdempotencyKey string `json:"idempotency_key" desc:"retry key — archiving twice is safe"`
}

func archiveDashboard() opcore.Operation[archiveDashboardInput, storage.Dashboard] {
	return opcore.Operation[archiveDashboardInput, storage.Dashboard]{
		Name:           "archive_dashboard",
		Summary:        "Archive a dashboard (reversible): it leaves active lists but keeps its charts and data. Repeating is idempotent.",
		Access:         opcore.AccessDashboardsWrite,
		MinSessionRole: "member",
		Scope:          "analyze_build",
		Handler: func(ctx context.Context, cc opcore.CallContext, in archiveDashboardInput) (storage.Dashboard, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Dashboard{}, err
			}
			if in.Revision <= 0 {
				return storage.Dashboard{}, fmt.Errorf("revision must be the dashboard's current revision (> 0)")
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.Dashboard{}, err
			}
			return d.Repo.ArchiveDashboardIdempotent(ctx, cc.ProjectID, in.DashboardID, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}

// --- unarchive_dashboard ---

type unarchiveDashboardInput struct {
	DashboardID    string `json:"dashboard_id" required:"true" desc:"dashboard to restore from archive"`
	Revision       int64  `json:"revision" required:"true" desc:"expected current revision — stale revisions conflict; an already-active dashboard returns its current state"`
	IdempotencyKey string `json:"idempotency_key" desc:"retry key — restoring twice is safe"`
}

func unarchiveDashboard() opcore.Operation[unarchiveDashboardInput, storage.Dashboard] {
	return opcore.Operation[unarchiveDashboardInput, storage.Dashboard]{
		Name:           "unarchive_dashboard",
		Summary:        "Restore an archived dashboard to active lists. Repeating is idempotent.",
		Access:         opcore.AccessDashboardsWrite,
		MinSessionRole: "member",
		Scope:          "analyze_build",
		Handler: func(ctx context.Context, cc opcore.CallContext, in unarchiveDashboardInput) (storage.Dashboard, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Dashboard{}, err
			}
			if in.Revision <= 0 {
				return storage.Dashboard{}, fmt.Errorf("revision must be the dashboard's current revision (> 0)")
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.Dashboard{}, err
			}
			return d.Repo.UnarchiveDashboardIdempotent(ctx, cc.ProjectID, in.DashboardID, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}
