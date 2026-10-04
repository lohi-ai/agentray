package storage

import (
	"context"
	"sync"
	"time"
)

const (
	stagingDeleteChunk = 50_000
	stagingPassBudget  = 5 * time.Minute
)

// StagingGenerationDescriptor is the frozen C1 eligibility handoff. C2 never
// guesses these booleans from age, a failed run, or an absent lease.
type StagingGenerationDescriptor struct {
	Generation           string
	State                string
	TerminalAt           *time.Time
	Resumable            bool
	IsActiveOnThisStore  bool
	HasUnpublishedOutbox bool
}

func EligibleForStagingCleanup(g StagingGenerationDescriptor, cutoff time.Time) bool {
	// A sealed generation is terminal too. Once a newer promotion supersedes it
	// on this serving store and every outbox item is published, its full staging
	// payload has no remaining local authority and must be retired by the same
	// bounded path as failed/cancelled attempts.
	if g.State != "sealed" && g.State != "failed" && g.State != "cancelled" {
		return false
	}
	if g.TerminalAt == nil || g.TerminalAt.After(cutoff) {
		return false
	}
	return !g.Resumable && !g.IsActiveOnThisStore && !g.HasUnpublishedOutbox
}

// StagingRetentionBackend is implemented by C1's generation store. Delete
// must re-check all eligibility predicates inside the same transaction as the
// bounded deletion; a preceding list is never deletion authority.
type StagingRetentionBackend interface {
	ListStagingGenerations(context.Context, time.Time, int) ([]StagingGenerationDescriptor, error)
	DeleteEligibleStagingChunk(context.Context, string, time.Time, int) (deleted int, stillEligible bool, err error)
}

type StagingRetention struct {
	backend StagingRetentionBackend
	ttl     time.Duration
	now     func() time.Time
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	running bool
}

func NewStagingRetention(backend StagingRetentionBackend, ttl time.Duration) *StagingRetention {
	return &StagingRetention{backend: backend, ttl: ttl, now: time.Now}
}

// Tick admits at most one bounded pass. A zero TTL or unavailable C1 backend
// disables cleanup rather than guessing eligibility.
func (r *StagingRetention) Tick(parent context.Context, _ time.Time) {
	if r == nil || r.backend == nil || r.ttl <= 0 {
		return
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	r.running, r.cancel = true, cancel
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer func() { r.mu.Lock(); r.running = false; r.cancel = nil; r.mu.Unlock() }()
		r.sweep(ctx)
	}()
}

func (r *StagingRetention) sweep(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, stagingPassBudget)
	defer cancel()
	cutoff := r.now().UTC().Add(-r.ttl)
	candidates, err := r.backend.ListStagingGenerations(ctx, cutoff, 256)
	if err != nil {
		return
	}
	for _, candidate := range candidates {
		if !EligibleForStagingCleanup(candidate, cutoff) {
			continue
		}
		for {
			deleted, eligible, err := r.backend.DeleteEligibleStagingChunk(ctx, candidate.Generation, cutoff, stagingDeleteChunk)
			if err != nil || !eligible || deleted < stagingDeleteChunk {
				break
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (r *StagingRetention) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
