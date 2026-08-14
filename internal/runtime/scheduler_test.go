package agentruntime

import (
	"testing"
	"time"
)

// TestCronMatches exercises the 5-field matcher used to fire scheduled runs
// (§8). It must support '*', '*/n', ranges, and lists, and reject malformed
// expressions rather than firing spuriously.
func TestCronMatches(t *testing.T) {
	// Mon 2026-06-15 09:30 UTC (weekday 1).
	at := time.Date(2026, 6, 15, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		expr string
		want bool
	}{
		{"* * * * *", true},
		{"30 9 * * *", true},      // exact minute+hour
		{"30 9 15 6 1", true},     // fully specified, matching
		{"31 9 * * *", false},     // wrong minute
		{"*/15 * * * *", true},    // 30 % 15 == 0
		{"*/7 * * * *", false},    // 30 % 7 != 0
		{"0-45 9 * * *", true},    // minute in range
		{"0-29 9 * * *", false},   // minute outside range
		{"30 8,9,10 * * *", true}, // hour list
		{"30 9 * * 0,6", false},   // weekend only, but it's Monday
		{"bad expr", false},       // wrong field count
		{"30 9 * *", false},       // too few fields
	}
	for _, tc := range cases {
		if got := cronMatches(tc.expr, at); got != tc.want {
			t.Errorf("cronMatches(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}
