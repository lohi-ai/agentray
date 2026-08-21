package ingestion

import (
	"testing"
	"time"
)

func TestSessionizer(t *testing.T) {
	base := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)

	t.Run("consecutive events inside the window share a session", func(t *testing.T) {
		s := newSessionizer(30 * time.Minute)
		first := s.sessionFor("p1", "alice", base)
		second := s.sessionFor("p1", "alice", base.Add(5*time.Minute))
		if first == "" {
			t.Fatal("first event got no session")
		}
		if first != second {
			t.Fatalf("session split inside the window: %q then %q", first, second)
		}
	})

	t.Run("a gap past the window opens a new session", func(t *testing.T) {
		s := newSessionizer(30 * time.Minute)
		first := s.sessionFor("p1", "alice", base)
		second := s.sessionFor("p1", "alice", base.Add(31*time.Minute))
		if first == second {
			t.Fatalf("session survived a %v gap", 31*time.Minute)
		}
	})

	t.Run("the window slides with activity", func(t *testing.T) {
		// 25 minutes apart each, an hour of continuous reading. A fixed bucket
		// would cut this in two; an inactivity window must not.
		s := newSessionizer(30 * time.Minute)
		want := s.sessionFor("p1", "alice", base)
		for i := 1; i <= 3; i++ {
			at := base.Add(time.Duration(i) * 25 * time.Minute)
			if got := s.sessionFor("p1", "alice", at); got != want {
				t.Fatalf("event at +%v started a new session", at.Sub(base))
			}
		}
	})

	t.Run("people and projects are separate", func(t *testing.T) {
		s := newSessionizer(30 * time.Minute)
		alice := s.sessionFor("p1", "alice", base)
		bob := s.sessionFor("p1", "bob", base)
		otherProject := s.sessionFor("p2", "alice", base)
		if alice == bob {
			t.Fatal("two people share one session")
		}
		if alice == otherProject {
			t.Fatal("one distinct_id shares a session across projects")
		}
	})

	t.Run("a late event inside the window joins, and does not rewind the window", func(t *testing.T) {
		// Batched SDKs flush out of order. The older event belongs to the open
		// session; it must not drag lastSeen backwards, or the next live event
		// would measure its gap from the wrong point and split the session.
		s := newSessionizer(30 * time.Minute)
		want := s.sessionFor("p1", "alice", base.Add(20*time.Minute))
		if got := s.sessionFor("p1", "alice", base.Add(10*time.Minute)); got != want {
			t.Fatal("a backdated event inside the window started a new session")
		}
		if got := s.sessionFor("p1", "alice", base.Add(45*time.Minute)); got != want {
			t.Fatal("the backdated event rewound the window and split the session")
		}
	})

	t.Run("no id without a project and a person", func(t *testing.T) {
		s := newSessionizer(0)
		if got := s.sessionFor("", "alice", base); got != "" {
			t.Fatalf("minted a session with no project: %q", got)
		}
		if got := s.sessionFor("p1", "", base); got != "" {
			t.Fatalf("minted a session with no distinct_id: %q", got)
		}
	})

	t.Run("a zero handler never panics", func(t *testing.T) {
		var s *sessionizer
		if got := s.sessionFor("p1", "alice", base); got != "" {
			t.Fatalf("nil sessionizer returned %q", got)
		}
	})

	t.Run("the sweep drops only sessions past the window", func(t *testing.T) {
		s := newSessionizer(30 * time.Minute)
		live := s.sessionFor("p1", "live", base.Add(50*time.Minute))
		s.sessionFor("p1", "stale", base)
		s.mu.Lock()
		s.sweepLocked(base.Add(60 * time.Minute))
		remaining := len(s.last)
		s.mu.Unlock()
		if remaining != 1 {
			t.Fatalf("sweep left %d entries, want 1", remaining)
		}
		if got := s.sessionFor("p1", "live", base.Add(55*time.Minute)); got != live {
			t.Fatal("sweep evicted a session that was still open")
		}
	})
}
