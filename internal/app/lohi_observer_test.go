package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/shared/cronx"
	"github.com/lohi-ai/agentray/internal/workloads"
)

func TestLohiRevenueObserverCronResolvesNineAMHCMToUTC(t *testing.T) {
	var manifest struct {
		LocalTimezone string `json:"local_timezone"`
		Schedules     []struct {
			Cron                 string `json:"cron"`
			Mode                 string `json:"mode"`
			MinimumCohortAgeDays int    `json:"minimum_cohort_age_days"`
		} `json:"schedules"`
	}
	if err := json.Unmarshal(workloads.LohiRevenueObserverManifest(), &manifest); err != nil {
		t.Fatal(err)
	}
	hcm, err := time.LoadLocation(manifest.LocalTimezone)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]struct {
		cron    string
		weekday time.Weekday
		minAge  int
	}{
		"daily_completeness":   {cron: "0 2 * * *", weekday: time.Tuesday},
		"weekly_mature_cohort": {cron: "0 2 * * 1", weekday: time.Monday, minAge: 14},
	}
	for _, schedule := range manifest.Schedules {
		expect, ok := want[schedule.Mode]
		if !ok {
			t.Fatalf("unexpected mode %q", schedule.Mode)
		}
		if schedule.Cron != expect.cron || schedule.MinimumCohortAgeDays != expect.minAge {
			t.Errorf("%s = cron %q age %d, want %q/%d", schedule.Mode, schedule.Cron, schedule.MinimumCohortAgeDays, expect.cron, expect.minAge)
		}
		// 02:00 UTC is 09:00 HCM. Monday is chosen for the weekly check;
		// Tuesday is an arbitrary ordinary day proving the daily expression.
		day := 5
		if expect.weekday == time.Tuesday {
			day = 6
		}
		utc := time.Date(2026, time.October, day, 2, 0, 0, 0, time.UTC)
		local := utc.In(hcm)
		if local.Hour() != 9 || local.Minute() != 0 || local.Weekday() != expect.weekday {
			t.Fatalf("%s local resolution = %s", schedule.Mode, local)
		}
		if !cronx.Matches(schedule.Cron, utc) {
			t.Errorf("%s cron %q does not fire at %s", schedule.Mode, schedule.Cron, utc)
		}
		if cronx.Matches(schedule.Cron, utc.Add(-time.Hour)) {
			t.Errorf("%s cron %q also fires at 08:00 HCM", schedule.Mode, schedule.Cron)
		}
	}
}
