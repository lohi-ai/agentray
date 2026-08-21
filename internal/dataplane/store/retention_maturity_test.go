package storage

import (
	"testing"
	"time"
)

// A period whose window has not finished elapsing has no rate. Reporting it as
// 0% is a fabricated number: on the default 24-hour range every weekly period is
// in the future, so the curve read 100% then a flat zero, and the headline
// averaged that into a confident "Avg retention 11%".
func TestRetentionPeriodMatureOnlyWhenTheWindowHasElapsed(t *testing.T) {
	to := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		cohortStart time.Time
		week        int
		want        bool
	}{
		{"cohort is a day old, week 1 is in the future", to.Add(-24 * time.Hour), 1, false},
		// The bug this pins: widening the *window* to nine weeks does not age the
		// cohort. A project with two days of events has a two-day-old cohort, and
		// every weekly period is still in the future for all of them.
		{"two days of data inside a nine-week window", to.AddDate(0, 0, -2), 1, false},
		{"cohort is 14 days old, week 1 closed", to.AddDate(0, 0, -14), 1, true},
		{"cohort is 14 days old, week 2 has not", to.AddDate(0, 0, -14), 2, false},
		{"cohort is 63 days old, week 8 closed", to.AddDate(0, 0, -63), 8, true},
		{"cohort is 62 days old, week 8 has not", to.AddDate(0, 0, -62), 8, false},
		{"no cohort at all", time.Time{}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retentionPeriodMature(tc.cohortStart, to, tc.week); got != tc.want {
				t.Errorf("retentionPeriodMature(%v, %v, %d) = %v, want %v", tc.cohortStart, to, tc.week, got, tc.want)
			}
		})
	}
	if retentionPeriodMature(to.AddDate(0, 0, -30), time.Time{}, 1) {
		t.Error("an unset To must never read as mature")
	}
}

// Maturity is monotone: if week N has elapsed, so has every week before it.
func TestRetentionMaturityIsMonotone(t *testing.T) {
	to := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{1, 7, 14, 30, 63, 120} {
		cohortStart := to.AddDate(0, 0, -days)
		sawImmature := false
		for week := 1; week <= retentionWeeks; week++ {
			mature := retentionPeriodMature(cohortStart, to, week)
			if mature && sawImmature {
				t.Errorf("%d-day range: week %d is mature after an immature earlier week", days, week)
			}
			if !mature {
				sawImmature = true
			}
		}
	}
}
