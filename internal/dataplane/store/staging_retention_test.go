package storage

import (
	"context"
	"testing"
	"time"
)

var _ StagingRetentionBackend = (*Store)(nil)

func TestStagingRetentionRequiresAuthoritativeTerminalEligibility(t *testing.T) {
	old := time.Now().Add(-8 * 24 * time.Hour)
	base := StagingGenerationDescriptor{Generation: "g", State: "failed", TerminalAt: &old}
	if !EligibleForStagingCleanup(base, time.Now().Add(-7*24*time.Hour)) {
		t.Fatal("eligible terminal generation refused")
	}
	for name, mutate := range map[string]func(*StagingGenerationDescriptor){
		"resumable":        func(g *StagingGenerationDescriptor) { g.Resumable = true },
		"active":           func(g *StagingGenerationDescriptor) { g.IsActiveOnThisStore = true },
		"outbox":           func(g *StagingGenerationDescriptor) { g.HasUnpublishedOutbox = true },
		"capturing":        func(g *StagingGenerationDescriptor) { g.State = "capturing" },
		"unknown terminal": func(g *StagingGenerationDescriptor) { g.TerminalAt = nil },
	} {
		t.Run(name, func(t *testing.T) {
			g := base
			mutate(&g)
			if EligibleForStagingCleanup(g, time.Now().Add(-7*24*time.Hour)) {
				t.Fatalf("unsafe eligibility: %+v", g)
			}
		})
	}
	sealed := base
	sealed.State = "sealed"
	if EligibleForStagingCleanup(sealed, time.Now().Add(-7*24*time.Hour)) {
		t.Fatal("non-superseded sealed generation was eligible")
	}
	sealed.IsSupersededOnThisStore = true
	if !EligibleForStagingCleanup(sealed, time.Now().Add(-7*24*time.Hour)) {
		t.Fatal("superseded sealed generation was not eligible")
	}
}

type stagingBackendProbe struct {
	deleted chan string
}

func (b *stagingBackendProbe) ListStagingGenerations(_ context.Context, cutoff time.Time, _ int) ([]StagingGenerationDescriptor, error) {
	terminal := cutoff.Add(-time.Hour)
	return []StagingGenerationDescriptor{{Generation: "expired", State: "failed", TerminalAt: &terminal}}, nil
}
func (b *stagingBackendProbe) DeleteEligibleStagingChunk(_ context.Context, generation string, _ time.Time, _ int) (int, bool, error) {
	b.deleted <- generation
	return 1, true, nil
}

func TestStagingRetentionRunsConfiguredBackendAndStops(t *testing.T) {
	backend := &stagingBackendProbe{deleted: make(chan string, 1)}
	r := NewStagingRetention(backend, 7*24*time.Hour)
	r.Tick(context.Background(), time.Now())
	select {
	case generation := <-backend.deleted:
		if generation != "expired" {
			t.Fatalf("deleted generation = %q", generation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("configured staging retention never invoked its backend")
	}
	r.Stop()
}
