package ingestion

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionWindow is the inactivity gap that ends a session. 30 minutes is the
// convention every analytics product uses, so "sessions" here means the same
// thing it means in the tool the customer is comparing against.
const sessionWindow = 30 * time.Minute

// sessionCacheMax bounds the tracking table. Beyond it the sweeper drops every
// entry already past the window; if that frees nothing (a burst of genuinely
// concurrent visitors) the table is allowed to exceed the cap rather than evict a
// live session and split it in two — a too-large map costs memory, a wrong
// eviction costs a wrong number.
const sessionCacheMax = 200_000

// sessionizer derives a session id for events that arrive without one.
//
// It exists because sessions were structurally zero for anyone who followed the
// documented setup: neither the quickstart snippets nor the SDKs send a session
// id, and ingest only ever read one off the payload. So "Sessions" and "Avg
// session" on Traffic, and the Sessions column on People, were permanently 0 —
// two headline tiles that could never be anything else. Sessionizing on the
// server means the customer gets them for free, and a client that *does* send its
// own id still wins (below), so nothing that already worked changes.
//
// In-process and per-instance, matching catalogGuard: at one API instance this is
// exact, and if the deploy ever fans out, the failure mode is a session split
// across instances — an undercount of session *length*, never a lost event. The
// alternative, a lookup per event against DuckDB, puts a read on the ingest
// hot path for a derived convenience field.
type sessionizer struct {
	window time.Duration

	mu   sync.Mutex
	last map[string]sessionMark
}

type sessionMark struct {
	id       string
	lastSeen time.Time
}

func newSessionizer(window time.Duration) *sessionizer {
	if window <= 0 {
		window = sessionWindow
	}
	return &sessionizer{window: window, last: map[string]sessionMark{}}
}

// sessionFor returns the session `distinctID` is currently in, opening a new one
// when the gap since its last event exceeds the window.
//
// The gap is measured on the event timestamp, not on arrival: a batch flushed an
// hour late describes an hour-old session, and dating it "now" would merge it
// into whatever the same person is doing at flush time. Out-of-order events
// inside the window join the open session and never move lastSeen backwards.
func (s *sessionizer) sessionFor(projectID, distinctID string, ts time.Time) string {
	if s == nil || projectID == "" || distinctID == "" {
		return ""
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	key := projectID + "\x00" + distinctID

	s.mu.Lock()
	defer s.mu.Unlock()

	if mark, ok := s.last[key]; ok && absDuration(ts.Sub(mark.lastSeen)) < s.window {
		if ts.After(mark.lastSeen) {
			s.last[key] = sessionMark{id: mark.id, lastSeen: ts}
		}
		return mark.id
	}

	if len(s.last) >= sessionCacheMax {
		s.sweepLocked(ts)
	}
	mark := sessionMark{id: uuid.NewString(), lastSeen: ts}
	s.last[key] = mark
	return mark.id
}

// sweepLocked drops the sessions that can no longer be extended. Callers hold mu.
func (s *sessionizer) sweepLocked(now time.Time) {
	for key, mark := range s.last {
		if absDuration(now.Sub(mark.lastSeen)) >= s.window {
			delete(s.last, key)
		}
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
