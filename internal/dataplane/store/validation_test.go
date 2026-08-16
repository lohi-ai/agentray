package storage

import (
	"testing"
	"time"
)

// DaysLeft is what Verdict() keys `failed` off, so how it rounds decides
// whether a test dies a day early. Truncating integer division reads twenty
// hours of remaining window as zero days — and the last day of a validation
// window is exactly when a borderline test is decided.
func TestDaysLeftDoesNotCloseTheWindowEarly(t *testing.T) {
	committed := time.Now().UTC().Add(-13*24*time.Hour - 4*time.Hour) // 13d 4h ago
	test := ValidationTest{TargetCount: 40, WindowDays: 14, CommittedAt: &committed}

	s := &Store{}
	p, err := s.ValidationTestProgress(t.Context(), test)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if p.DaysLeft < 1 {
		t.Fatalf("20 hours of window left reads as %d days left, so the test is already dead", p.DaysLeft)
	}
	if got := p.Verdict(); got != TestCommitted {
		t.Fatalf("a window with 20 hours still on it reads as %q", got)
	}
}

// The verdict is the only reason the threshold row exists: with a number agreed
// in advance, "did it work" becomes a lookup instead of an argument. These are
// the three readings it must never get wrong.
func TestProgressVerdict(t *testing.T) {
	test := ValidationTest{TargetCount: 40, WindowDays: 14}

	t.Run("target cleared is a pass, even early", func(t *testing.T) {
		p := TestProgress{Test: test, Metric: 41, DaysLeft: 9}
		if got := p.Verdict(); got != TestPassed {
			t.Fatalf("41 of 40 with time to spare is a pass, got %q", got)
		}
	})

	// The failure this guards is a founder killing a good idea in week one. A
	// test that has not finished has not failed, and the product must not offer
	// the word.
	t.Run("short of target with the window open is not a failure", func(t *testing.T) {
		p := TestProgress{Test: test, Metric: 3, DaysLeft: 11}
		if got := p.Verdict(); got != TestCommitted {
			t.Fatalf("an open window must read as still running, got %q", got)
		}
	})

	t.Run("short of target with the window closed is a failure", func(t *testing.T) {
		p := TestProgress{Test: test, Metric: 39, DaysLeft: 0}
		if got := p.Verdict(); got != TestFailed {
			t.Fatalf("39 of 40 at the deadline is a miss, got %q", got)
		}
	})

	// Exactly on target passes. A threshold the owner agreed to is a promise, and
	// moving it by one at the finish line is the dishonesty this table prevents.
	t.Run("exactly on target passes", func(t *testing.T) {
		p := TestProgress{Test: test, Metric: 40, DaysLeft: 0}
		if got := p.Verdict(); got != TestPassed {
			t.Fatalf("40 of 40 is the number they agreed to, got %q", got)
		}
	})
}

func TestPlausibleEmail(t *testing.T) {
	good := []string{"a@b.co", "someone@example.com", "first.last+tag@sub.example.co.uk"}
	for _, s := range good {
		if !plausibleEmail(s) {
			t.Errorf("%q is a real address shape and must be accepted", s)
		}
	}
	// Rejects are shape failures only. Nothing short of delivery proves an
	// address, so a stricter rule here would mostly reject real people.
	bad := []string{"", "nope", "no-at-sign.com", "a@b", "a@.com", "two@addresses.com, other@x.com", "has space@x.com"}
	for _, s := range bad {
		if plausibleEmail(s) {
			t.Errorf("%q is not an address shape and must be rejected", s)
		}
	}
}
