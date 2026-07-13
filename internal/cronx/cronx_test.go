package cronx

import (
	"testing"
	"time"
)

func TestMatches(t *testing.T) {
	at := time.Date(2026, 7, 14, 10, 30, 0, 0, time.UTC) // Tuesday
	cases := []struct {
		expr string
		want bool
	}{
		{"* * * * *", true},
		{"30 10 * * *", true},
		{"*/15 * * * *", true},
		{"0 * * * *", false},
		{"30 10 14 7 *", true},
		{"30 10 * * 2", true},  // Tuesday = 2
		{"30 10 * * 0", false}, // Sunday
		{"25-35 10 * * *", true},
		{"0,30 10 * * *", true},
		{"bad cron", false},
		{"", false},
	}
	for _, c := range cases {
		if got := Matches(c.expr, at); got != c.want {
			t.Errorf("Matches(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}
