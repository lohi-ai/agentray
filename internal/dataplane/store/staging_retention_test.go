package storage

import (
	"testing"
	"time"
)

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
		"sealed":           func(g *StagingGenerationDescriptor) { g.State = "sealed" },
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
}
