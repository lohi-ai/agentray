package agentcore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type providerSessionProbe struct {
	closed int32
	reset  int32
}

func (s *providerSessionProbe) Close()              { atomic.AddInt32(&s.closed, 1) }
func (s *providerSessionProbe) ResetAccountScoped() { atomic.AddInt32(&s.reset, 1) }

type endpointSessionProbe struct{ closed int32 }

func (s *endpointSessionProbe) Close() { atomic.AddInt32(&s.closed, 1) }

func TestProviderSessionCreatesOnceAndSelectiveReset(t *testing.T) {
	session := NewProviderSession()
	account := &providerSessionProbe{}
	endpoint := &endpointSessionProbe{}
	var creates int32

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := session.State("account", func() ProviderSessionState {
				atomic.AddInt32(&creates, 1)
				return account
			})
			if got != account {
				t.Errorf("State returned %T, want shared account state", got)
			}
		}()
	}
	wg.Wait()
	session.State("endpoint", func() ProviderSessionState { return endpoint })
	if got := atomic.LoadInt32(&creates); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}

	session.ResetAccountScoped()
	if got := atomic.LoadInt32(&account.reset); got != 1 {
		t.Fatalf("account resets = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&endpoint.closed); got != 0 {
		t.Fatalf("endpoint state was disturbed during account reset: close=%d", got)
	}

	session.Close()
	session.Close()
	if got := atomic.LoadInt32(&account.closed); got != 1 {
		t.Fatalf("account closes = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&endpoint.closed); got != 1 {
		t.Fatalf("endpoint closes = %d, want 1", got)
	}
	if got := session.State("late", func() ProviderSessionState { return &endpointSessionProbe{} }); got != nil {
		t.Fatalf("closed session accepted new state: %T", got)
	}
}

func TestProviderSessionRegistryRetainsAndEvictsOnlyIdle(t *testing.T) {
	now := time.Unix(100, 0)
	registry := NewProviderSessionRegistry(1, time.Minute)
	registry.now = func() time.Time { return now }
	defer registry.Close()

	a, releaseA := registry.Acquire("a")
	probeA := &endpointSessionProbe{}
	a.State("probe", func() ProviderSessionState { return probeA })
	releaseA()
	reacquiredA, releaseA2 := registry.Acquire("a")
	if reacquiredA != a {
		t.Fatal("idle session was replaced when reacquiring its existing key")
	}

	// Capacity is one, but A is leased: B may temporarily exceed the cap rather
	// than closing state a live request still owns.
	b, releaseB := registry.Acquire("b")
	if got := atomic.LoadInt32(&probeA.closed); got != 0 {
		t.Fatalf("active session A was evicted: close=%d", got)
	}
	releaseB()
	releaseA2()

	// The next acquire makes room by evicting the least-recently-used idle entry.
	now = now.Add(time.Second)
	_, releaseC := registry.Acquire("c")
	releaseC()
	if got := atomic.LoadInt32(&probeA.closed); got != 1 {
		t.Fatalf("idle session A close=%d, want 1 after bounded eviction", got)
	}

	_ = b // B may be the over-capacity idle entry evicted on release.
}

func TestProviderSessionRegistryExpiresIdleAndReleaseIsIdempotent(t *testing.T) {
	now := time.Unix(200, 0)
	registry := NewProviderSessionRegistry(8, time.Minute)
	registry.now = func() time.Time { return now }
	defer registry.Close()

	session, release := registry.Acquire("old")
	probe := &endpointSessionProbe{}
	session.State("probe", func() ProviderSessionState { return probe })
	release()
	release()
	now = now.Add(2 * time.Minute)
	_, releaseNew := registry.Acquire("new")
	releaseNew()
	if got := atomic.LoadInt32(&probe.closed); got != 1 {
		t.Fatalf("expired state closes = %d, want 1", got)
	}
}

func TestLoopThreadsLogicalProviderSessionOntoEveryRequest(t *testing.T) {
	provider := NewFauxProvider(AssistantText("done"))
	session := NewProviderSession()
	agent, err := New(Config{
		Provider: provider, Model: "m",
		ProviderSession: session, ProviderSessionID: "conversation-7",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.Recorded) != 1 {
		t.Fatalf("recorded requests = %d, want 1", len(provider.Recorded))
	}
	got := provider.Recorded[0]
	if got.ProviderSession != session || got.SessionID != "conversation-7" {
		t.Fatalf("provider session = (%p, %q), want (%p, conversation-7)", got.ProviderSession, got.SessionID, session)
	}
	if child := agent.Fork("child-session"); child.providerSession != session || child.providerSessionID != "child-session" {
		t.Fatalf("forked provider session = (%p, %q), want (%p, child-session)", child.providerSession, child.providerSessionID, session)
	}
}
