// Package findings is the deterministic findings engine: a scheduled,
// rule-based detector pass that writes agent_recommendations rows through the
// same createRecommendation path submit_recommendation uses — no LLM, no agent
// required. Detectors are pure reads over the store's existing deterministic
// surfaces (the overview read, the funnel engine, the data-status contract);
// every finding carries source='engine' and a dedupe_key so a condition that
// keeps holding folds into its existing card instead of re-filing.
package findings

import (
	"context"
	"fmt"
	"sync"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// Tuning constants live together so 003/005 can adjust the engine's
// sensitivity in one place. They are judgment calls, not derived values.
const (
	// ScanInterval is how often one project earns a scheduled pass. 23h (not
	// 24h) keeps a daily scan from drifting later every day.
	ScanInterval = 23 * time.Hour
	// scanBudget bounds one scheduled pass over all projects; a scan that
	// outlives it is abandoned and retried by claim expiry, not resumed.
	scanBudget = 5 * time.Minute

	// WoWDeltaThreshold is the week-over-week move (fraction) that makes a
	// headline metric a finding. WoWDeltaMinCount is the noise floor on the
	// PRIOR window: a percentage swing computed off a tiny baseline is noise,
	// not news (10 → 200 reads as "×20" and says nothing).
	WoWDeltaThreshold = 0.25
	WoWDeltaMinCount  = 50

	// SourceShiftSharePP is the share move (percentage points) in one
	// referrer channel that counts as a mix shift; a change in the top
	// channel always counts. SourceShiftMinPageviews is the floor on both
	// windows' totals.
	SourceShiftSharePP       = 15.0
	SourceShiftMinPageviews  = 50

	// StaleSourceAfter is how long a configured, enabled sync may go without
	// a success before the source is reported stale.
	StaleSourceAfter = 48 * time.Hour

	// FunnelDropPP is the step-conversion fall (percentage points) vs the
	// prior window that fires a watch; FunnelMinUsers is the floor on the
	// step's user count in either window.
	FunnelDropPP   = 15.0
	FunnelMinUsers = 20
)

// ProjectStore is the per-project surface a scan needs. usecase.Repo already
// declares every method, so the run_findings_scan op can drive a scan through
// the same Repo value every adapter injects — no extra wiring.
type ProjectStore interface {
	Overview(ctx context.Context, projectID, period, platform string, now time.Time) (storage.OverviewResult, error)
	RunInsight(ctx context.Context, projectID, insightType, metric string, steps []string, filter storage.EventFilter) (storage.InsightResult, error)
	FunnelWatchesForProject(ctx context.Context, projectID string) ([]storage.FunnelWatch, error)
	CreateRecommendation(ctx context.Context, rec storage.AgentRecommendation) (string, error)
}

// Store is the fleet-level surface the scheduled pass needs on top of
// ProjectStore: the project list, the per-project claim, and the demo id to
// exclude. *storage.Store satisfies it.
type Store interface {
	ProjectStore
	FindingScanProjects(ctx context.Context, excludeID string) ([]string, error)
	ClaimFindingScan(ctx context.Context, projectID string, now, due time.Time) (bool, error)
	DemoProjectID() string
}

// ScanResult reports one project scan: which detectors ran and which finding
// ids were written (or folded into — a repeat returns the existing row's id).
type ScanResult struct {
	ProjectID string   `json:"project_id"`
	Detectors []string `json:"detectors"`
	Findings  []string `json:"findings"`
}

// Scanner is the scheduled engine. It rides the scheduler's minute tick beside
// the alert evaluator and the connector engine, claiming each non-demo project
// once per ScanInterval through finding_scan_state.
type Scanner struct {
	store Store

	mu      sync.Mutex
	running bool
}

func NewScanner(store Store) *Scanner {
	return &Scanner{store: store}
}

// Tick admits at most one scan pass at a time and returns immediately — the
// pass runs on its own goroutine under its own budget, so a slow project can
// never hold the minute clock the alert evaluator and connector syncs share.
// The claim row, not the tick, decides which projects are due.
func (s *Scanner) Tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	// The pass must outlive the tick that admitted it: a hook whose context
	// dies on return would abort the scan it just started. The budget, not
	// the caller, bounds the work.
	scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scanBudget)
	go func() {
		defer cancel()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		s.scanAll(scanCtx, now.UTC())
	}()
}

// Scan runs one project's detector pass synchronously — the run_findings_scan
// op's path. It does not claim the project: a manual run is allowed to see
// what the detectors would say right now, and the dedupe keys keep a manual
// scan followed by a scheduled one from double-filing.
func (s *Scanner) Scan(ctx context.Context, projectID string, now time.Time) (ScanResult, error) {
	return ScanProject(ctx, s.store, projectID, now)
}

func (s *Scanner) scanAll(ctx context.Context, now time.Time) {
	projects, err := s.store.FindingScanProjects(ctx, s.store.DemoProjectID())
	if err != nil {
		fmt.Printf("findings: project list failed: %v\n", err)
		return
	}
	due := now.Add(-ScanInterval)
	for _, projectID := range projects {
		claimed, err := s.store.ClaimFindingScan(ctx, projectID, now, due)
		if err != nil {
			fmt.Printf("findings: claim %s failed: %v\n", projectID, err)
			continue
		}
		if !claimed {
			continue
		}
		if _, err := ScanProject(ctx, s.store, projectID, now); err != nil {
			fmt.Printf("findings: scan %s failed: %v\n", projectID, err)
		}
	}
}

// ScanProject runs every detector over one project and writes each firing
// through CreateRecommendation. Detectors share the two overview reads
// (current and prior 7d windows); the funnel detector additionally re-runs
// each declared watch over both windows.
func ScanProject(ctx context.Context, st ProjectStore, projectID string, now time.Time) (ScanResult, error) {
	res := ScanResult{ProjectID: projectID, Detectors: []string{"wow_delta", "target_off_track", "source_shift", "stale_data", "funnel_drop"}, Findings: []string{}}

	cur, err := st.Overview(ctx, projectID, "7d", "", now)
	if err != nil {
		return res, fmt.Errorf("findings: overview: %w", err)
	}
	prior, err := st.Overview(ctx, projectID, "7d", "", now.Add(-7*24*time.Hour))
	if err != nil {
		return res, fmt.Errorf("findings: prior overview: %w", err)
	}

	found := []finding{}
	found = append(found, detectWoWDeltas(cur)...)
	found = append(found, detectOffTrack(cur)...)
	found = append(found, detectSourceShift(cur, prior)...)
	found = append(found, detectStaleData(cur, now)...)

	watches, err := st.FunnelWatchesForProject(ctx, projectID)
	if err != nil {
		return res, fmt.Errorf("findings: funnel watches: %w", err)
	}
	for _, w := range watches {
		// One unreadable watch must not sink the pass — its finding is
		// skipped, the rest still land.
		if f, err := detectFunnelDrop(ctx, st, projectID, w, now); err == nil && f != nil {
			found = append(found, *f)
		}
	}

	for _, f := range found {
		id, err := st.CreateRecommendation(ctx, storage.AgentRecommendation{
			ProjectID:    projectID,
			Category:     f.category,
			Title:        f.title,
			Rationale:    f.rationale,
			EvidenceJSON: f.evidenceJSON(),
			ImpactScore:  f.impact,
			Source:       "engine",
			DedupeKey:    f.dedupeKey,
		})
		if err != nil {
			return res, fmt.Errorf("findings: write %q: %w", f.dedupeKey, err)
		}
		res.Findings = append(res.Findings, id)
	}
	return res, nil
}
