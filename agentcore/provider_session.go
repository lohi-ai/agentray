package agentcore

import (
	"sync"
	"time"
)

// ProviderSessionState is one provider-owned mutable record retained for the
// lifetime of a logical agent conversation. Implementations must make Close
// idempotent: a registry eviction and an explicit shutdown may race at process
// teardown, and neither may leak a transport or panic the host.
//
// The interface intentionally says nothing about serialization. Provider state
// commonly contains live sockets, server-side response handles, or learned
// endpoint quirks; keeping it an optimization rather than durable truth lets the
// same provider run in a laptop process and across stateless server replicas.
type ProviderSessionState interface {
	Close()
}

// AccountScopedProviderState is the optional, narrow reset seam for state that
// belongs to the credential which created it (for example a server-side response
// id). Endpoint-scoped lessons should not implement this interface, so an OAuth
// account rotation preserves them.
type AccountScopedProviderState interface {
	ProviderSessionState
	ResetAccountScoped()
}

// ProviderSession is a concurrency-safe bag of provider-private state for one
// logical conversation. Providers namespace their own keys and own the concrete
// value types; agentcore owns only lifecycle and account-rotation semantics.
type ProviderSession struct {
	mu     sync.RWMutex
	states map[string]ProviderSessionState
	closed bool
}

// NewProviderSession creates an unregistered session. Embedded consumers that
// already own conversation lifetime can use this directly; server runtimes
// normally lease one from ProviderSessionRegistry instead.
func NewProviderSession() *ProviderSession {
	return &ProviderSession{states: make(map[string]ProviderSessionState)}
}

// State returns the value at key, creating it exactly once. A nil factory, an
// empty key, a nil factory result, or a closed session returns nil. The factory
// runs under the session lock so two first requests cannot create parallel live
// transports and discard one without closing it.
func (s *ProviderSession) State(key string, create func() ProviderSessionState) ProviderSessionState {
	if s == nil || key == "" || create == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if state := s.states[key]; state != nil {
		return state
	}
	state := create()
	if state != nil {
		s.states[key] = state
	}
	return state
}

// ResetAccountScoped resets only records that explicitly declare themselves
// credential-bound. Endpoint and conversation lessons remain warm.
func (s *ProviderSession) ResetAccountScoped() {
	if s == nil {
		return
	}
	s.mu.RLock()
	states := make([]AccountScopedProviderState, 0, len(s.states))
	if !s.closed {
		for _, state := range s.states {
			if scoped, ok := state.(AccountScopedProviderState); ok {
				states = append(states, scoped)
			}
		}
	}
	s.mu.RUnlock()
	for _, state := range states {
		state.ResetAccountScoped()
	}
}

// Close releases every provider-owned record exactly once and makes the session
// reject future state creation. Calls into provider code happen outside the map
// lock so a provider's Close implementation cannot deadlock by re-entering its
// own bookkeeping.
func (s *ProviderSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	states := make([]ProviderSessionState, 0, len(s.states))
	for _, state := range s.states {
		states = append(states, state)
	}
	s.states = nil
	s.mu.Unlock()
	for _, state := range states {
		state.Close()
	}
}

const (
	defaultProviderSessionCapacity = 1024
	defaultProviderSessionIdleTTL  = 30 * time.Minute
)

type providerSessionEntry struct {
	session  *ProviderSession
	refs     int
	lastUsed time.Time
}

// ProviderSessionRegistry retains conversation state across short-lived Agent
// instances. It is bounded and lease-aware: only idle sessions are evicted, so
// pressure may temporarily exceed capacity rather than closing state a live
// request is using. A cache miss merely makes a provider relearn an optimization;
// request correctness never depends on process affinity.
type ProviderSessionRegistry struct {
	mu       sync.Mutex
	entries  map[string]*providerSessionEntry
	capacity int
	idleTTL  time.Duration
	now      func() time.Time
	closed   bool
}

// NewProviderSessionRegistry builds a bounded registry. Non-positive values use
// conservative defaults suitable for both a desktop daemon and a server worker.
func NewProviderSessionRegistry(capacity int, idleTTL time.Duration) *ProviderSessionRegistry {
	if capacity <= 0 {
		capacity = defaultProviderSessionCapacity
	}
	if idleTTL <= 0 {
		idleTTL = defaultProviderSessionIdleTTL
	}
	return &ProviderSessionRegistry{
		entries: make(map[string]*providerSessionEntry), capacity: capacity,
		idleTTL: idleTTL, now: time.Now,
	}
}

// Acquire leases the state for key. The release function is idempotent. An
// empty key gets an ephemeral session that is closed on release, which keeps
// one-off runs isolated without growing the registry.
func (r *ProviderSessionRegistry) Acquire(key string) (*ProviderSession, func()) {
	if r == nil || key == "" {
		session := NewProviderSession()
		var once sync.Once
		return session, func() { once.Do(session.Close) }
	}

	now := time.Now()
	r.mu.Lock()
	if r.now != nil {
		now = r.now()
	}
	if r.closed {
		r.mu.Unlock()
		session := NewProviderSession()
		var once sync.Once
		return session, func() { once.Do(session.Close) }
	}
	// Expire stale entries first, but do not reserve capacity until we know this
	// is a cache miss. Reserving before the lookup would evict the very idle entry
	// being reacquired when capacity is one.
	toClose := r.pruneLocked(now, false)
	entry := r.entries[key]
	if entry == nil {
		toClose = append(toClose, r.pruneLocked(now, true)...)
		entry = &providerSessionEntry{session: NewProviderSession(), lastUsed: now}
		r.entries[key] = entry
	}
	entry.refs++
	entry.lastUsed = now
	r.mu.Unlock()
	closeProviderSessions(toClose)

	var once sync.Once
	return entry.session, func() {
		once.Do(func() { r.release(key, entry) })
	}
}

func (r *ProviderSessionRegistry) release(key string, want *providerSessionEntry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	entry := r.entries[key]
	if entry != want {
		r.mu.Unlock()
		return
	}
	if entry.refs > 0 {
		entry.refs--
	}
	if r.now != nil {
		entry.lastUsed = r.now()
	} else {
		entry.lastUsed = time.Now()
	}
	toClose := r.pruneLocked(entry.lastUsed, false)
	r.mu.Unlock()
	closeProviderSessions(toClose)
}

// pruneLocked removes expired idle entries and, when reserve is true, enough
// least-recently-used idle entries to make room for one new key. Active entries
// are never candidates.
func (r *ProviderSessionRegistry) pruneLocked(now time.Time, reserve bool) []*ProviderSession {
	var removed []*ProviderSession
	for key, entry := range r.entries {
		if entry.refs == 0 && now.Sub(entry.lastUsed) >= r.idleTTL {
			delete(r.entries, key)
			removed = append(removed, entry.session)
		}
	}
	target := r.capacity
	if reserve {
		target--
	}
	for len(r.entries) > target {
		var oldestKey string
		var oldest *providerSessionEntry
		for key, entry := range r.entries {
			if entry.refs != 0 || (oldest != nil && !entry.lastUsed.Before(oldest.lastUsed)) {
				continue
			}
			oldestKey, oldest = key, entry
		}
		if oldest == nil {
			break
		}
		delete(r.entries, oldestKey)
		removed = append(removed, oldest.session)
	}
	return removed
}

// Close releases every retained idle or active session. Hosts should call it
// only after their requests have stopped; it is idempotent.
func (r *ProviderSessionRegistry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	sessions := make([]*ProviderSession, 0, len(r.entries))
	for _, entry := range r.entries {
		sessions = append(sessions, entry.session)
	}
	r.entries = nil
	r.mu.Unlock()
	closeProviderSessions(sessions)
}

func closeProviderSessions(sessions []*ProviderSession) {
	for _, session := range sessions {
		session.Close()
	}
}
