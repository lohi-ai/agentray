package storage

import (
	"context"
	"testing"
	"time"
)

// findings_test.go pins the ticket-001 substrate against live Postgres: the
// scan claim's once-per-interval semantics, the funnel-watch declaration's
// dedupe, and the dedupe_key fold that turns a re-firing condition into
// seen_count instead of a second card. Skipped without a test database,
// matching plans_test.go.

func TestClaimFindingScanOncePerInterval(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	now := time.Now().UTC()
	due := now.Add(-23 * time.Hour)

	claimed, err := s.ClaimFindingScan(ctx, projectID, now, due)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v; want claimed", claimed, err)
	}
	// A second claim inside the interval loses — the scheduled pass and a
	// racing replica cannot both scan.
	claimed, err = s.ClaimFindingScan(ctx, projectID, now.Add(time.Minute), due)
	if err != nil || claimed {
		t.Fatalf("re-claim inside interval = %v, %v; want not claimed", claimed, err)
	}
	// Once the interval has elapsed the project is due again — a crashed scan
	// is retried, never locked out.
	claimed, err = s.ClaimFindingScan(ctx, projectID, now.Add(24*time.Hour), now.Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim after interval = %v, %v; want claimed", claimed, err)
	}
}

func TestFindingScanProjectsExcludesDemo(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	all, err := s.FindingScanProjects(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, id := range all {
		if id == projectID {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded project missing from scan list")
	}
	excl, err := s.FindingScanProjects(ctx, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, id := range excl {
		if id == projectID {
			t.Fatalf("excluded project still listed")
		}
	}
}

func TestFunnelWatchDeclareDedupesOnSteps(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	w, err := s.CreateFunnelWatch(ctx, projectID, "signup", []string{"user.pageview", " user.signup "})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.Name != "signup" || len(w.Steps) != 2 || w.Steps[1] != "user.signup" {
		t.Fatalf("watch = %+v, want trimmed steps", w)
	}
	// Same ordered steps returns the existing watch — two watches over one
	// sequence would double every finding.
	again, err := s.CreateFunnelWatch(ctx, projectID, "other name", []string{"user.pageview", "user.signup"})
	if err != nil {
		t.Fatalf("re-declare: %v", err)
	}
	if again.ID != w.ID {
		t.Fatalf("re-declare created a second watch: %s vs %s", again.ID, w.ID)
	}
	if _, err := s.CreateFunnelWatch(ctx, projectID, "one", []string{"only"}); err == nil {
		t.Fatalf("single-step watch accepted")
	}
	watches, err := s.FunnelWatchesForProject(ctx, projectID)
	if err != nil || len(watches) != 1 {
		t.Fatalf("list = %+v, %v", watches, err)
	}
}

func TestEngineRecommendationFoldsOnDedupeKey(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	rec := AgentRecommendation{
		ProjectID: projectID, Category: "growth", Title: "Active people down 40% week over week",
		Rationale: "first wording", Source: "engine", DedupeKey: "wow:active_users", ImpactScore: 60,
	}
	id1, err := s.CreateRecommendation(ctx, rec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The same condition re-firing — different wording, same key — folds into
	// the open card instead of filing a second one.
	rec.Title = "Active people fell 40% WoW"
	rec.Rationale = "second wording"
	rec.ImpactScore = 70
	id2, err := s.CreateRecommendation(ctx, rec)
	if err != nil {
		t.Fatalf("re-fire: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("re-fire opened a second card: %s vs %s", id2, id1)
	}
	got, err := s.RecommendationForProject(ctx, projectID, id1)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.SeenCount != 2 || got.Source != "engine" || got.DedupeKey != "wow:active_users" || got.ImpactScore != 70 {
		t.Fatalf("folded row = %+v", got)
	}

	// Cooldown: a dismissed finding still folds — the condition persisting
	// after the owner settled the card must not re-file it.
	if err := s.AckRecommendation(ctx, userID, projectID, id1, "dismissed", "known"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	id3, err := s.CreateRecommendation(ctx, rec)
	if err != nil {
		t.Fatalf("post-dismiss fire: %v", err)
	}
	if id3 != id1 {
		t.Fatalf("dismissed condition re-filed: %s vs %s", id3, id1)
	}
	got, err = s.RecommendationForProject(ctx, projectID, id1)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Status != "dismissed" || got.SeenCount != 3 {
		t.Fatalf("dismissed fold = status %q seen %d, want dismissed/3", got.Status, got.SeenCount)
	}

	// Agent findings keep the trigram path and default provenance.
	agentID, err := s.CreateRecommendation(ctx, AgentRecommendation{
		ProjectID: projectID, Category: "growth", Title: "A distinct agent finding",
	})
	if err != nil {
		t.Fatalf("agent create: %v", err)
	}
	agent, err := s.RecommendationForProject(ctx, projectID, agentID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if agent.Source != "agent" || agent.DedupeKey != "" {
		t.Fatalf("agent row = source %q dedupe %q, want agent/''", agent.Source, agent.DedupeKey)
	}
}
