package workloads

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/shared/cronx"
)

type lohiObserverSchedule struct {
	Name                 string `json:"name"`
	Kind                 string `json:"kind"`
	Enabled              bool   `json:"enabled"`
	Cron                 string `json:"cron"`
	CadenceHCM           string `json:"cadence_hcm"`
	Mode                 string `json:"mode"`
	MinimumCohortAgeDays int    `json:"minimum_cohort_age_days"`
	PromptTemplate       string `json:"prompt_template"`
}

type lohiObserverManifest struct {
	Version                string                 `json:"version"`
	DefinitionVersion      string                 `json:"definition_version"`
	AgentPreset            string                 `json:"agent_preset"`
	SkillSHA256            string                 `json:"skill_sha256"`
	SchedulerTimezone      string                 `json:"scheduler_timezone"`
	LocalTimezone          string                 `json:"local_timezone"`
	InitiallyEnabled       bool                   `json:"initially_enabled"`
	ActivationRequirements []string               `json:"activation_requirements"`
	Schedules              []lohiObserverSchedule `json:"schedules"`
	Finding                struct {
		ReadOperation        string   `json:"read_operation"`
		WriteOperation       string   `json:"write_operation"`
		ContextPath          string   `json:"context_path"`
		ObservationKeyFields []string `json:"observation_key_fields"`
		IdempotencyKey       string   `json:"idempotency_key"`
		IdempotencyDecision  string   `json:"idempotency_decision"`
	} `json:"finding"`
	Delivery struct {
		Operation            string `json:"operation"`
		ChannelRequired      bool   `json:"channel_required"`
		OnMissingChannel     string `json:"on_missing_channel"`
		OnDeniedFindingWrite string `json:"on_denied_finding_write"`
		Guarantee            string `json:"guarantee"`
	} `json:"delivery"`
}

func readLohiObserverManifest(t *testing.T) lohiObserverManifest {
	t.Helper()
	var manifest lohiObserverManifest
	if err := json.Unmarshal(LohiRevenueObserverManifest(), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestLohiRevenueObserverUsesExistingInsightDigestAndDisabledTriggers(t *testing.T) {
	manifest := readLohiObserverManifest(t)
	if manifest.Version != LohiRevenueObserverVersion || manifest.DefinitionVersion != LohiEvidenceVersion {
		t.Fatalf("observer identity drifted: %+v", manifest)
	}
	if manifest.AgentPreset != "insight-digest" {
		t.Fatalf("agent preset = %q, want existing insight-digest", manifest.AgentPreset)
	}
	if manifest.InitiallyEnabled {
		t.Fatal("observer schedules must remain disabled until an operator configures them")
	}
	if manifest.SchedulerTimezone != "UTC" || manifest.LocalTimezone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("timezone contract drifted: scheduler=%q local=%q", manifest.SchedulerTimezone, manifest.LocalTimezone)
	}
	if len(manifest.Schedules) != 2 {
		t.Fatalf("schedule count = %d, want daily + weekly", len(manifest.Schedules))
	}
	for _, schedule := range manifest.Schedules {
		if schedule.Kind != "schedule" || schedule.Enabled {
			t.Errorf("schedule is not a disabled existing trigger template: %+v", schedule)
		}
		for _, marker := range []string{"readiness gate", "observation-key", "deduplication", "Plans-write", "delivery"} {
			if !strings.Contains(schedule.PromptTemplate, marker) {
				t.Errorf("%s prompt missing %q", schedule.Mode, marker)
			}
		}
	}

	preset := MustBySlug(manifest.AgentPreset)
	if !preset.Scopes["monitor"] || !preset.Scopes["data_quality"] || !preset.Scopes["analyze_build"] || !preset.Scopes["growth_suggest"] {
		t.Fatalf("Insight Digest lacks existing read/Plans/delivery scopes: %+v", preset.Scopes)
	}
	installed := map[string]int{}
	for _, skill := range preset.Skills {
		installed[skill.Name]++
	}
	if installed[LohiEvidenceVersion] != 1 || installed[LohiRevenueObserverVersion] != 1 {
		t.Fatalf("Insight Digest skill wiring = %+v, want one evidence and one observer skill", installed)
	}
	if _, ok := BySlug(LohiRevenueObserverVersion); ok {
		t.Fatal("observer was registered as a new persona/runtime instead of configuring Insight Digest")
	}
}

func TestLohiRevenueObserverCronResolvesNineAMHCMToUTC(t *testing.T) {
	manifest := readLohiObserverManifest(t)
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

func TestLohiRevenueObserverDedupAndHonestyContract(t *testing.T) {
	manifest := readLohiObserverManifest(t)
	wantKeyFields := []string{"project", "definition_version", "period", "condition"}
	if manifest.Finding.ContextPath != "evidence.observation_key" || !slices.Equal(manifest.Finding.ObservationKeyFields, wantKeyFields) {
		t.Fatalf("observation key contract drifted: path=%q fields=%v", manifest.Finding.ContextPath, manifest.Finding.ObservationKeyFields)
	}
	if manifest.Finding.ReadOperation != "list_findings" || manifest.Finding.WriteOperation != "submit_recommendation" || manifest.Delivery.Operation != "send_notification" {
		t.Fatalf("observer stopped using existing read/write/delivery operations: %+v %+v", manifest.Finding, manifest.Delivery)
	}
	if !strings.Contains(manifest.Finding.IdempotencyKey, "sha256") || !strings.Contains(manifest.Finding.IdempotencyDecision, "atomically claims") || !strings.Contains(manifest.Finding.IdempotencyDecision, "not exactly-once") {
		t.Fatalf("idempotency decision is incomplete: %+v", manifest.Finding)
	}
	if manifest.Delivery.ChannelRequired || manifest.Delivery.OnMissingChannel == "" || manifest.Delivery.OnDeniedFindingWrite == "" || !strings.Contains(manifest.Delivery.Guarantee, "no exactly-once") {
		t.Fatalf("delivery honesty contract drifted: %+v", manifest.Delivery)
	}

	skill := LohiRevenueObserverSkill()
	digest := sha256.Sum256([]byte(skill.Body))
	if got := hex.EncodeToString(digest[:]); got != manifest.SkillSHA256 {
		t.Fatalf("skill sha256 = %s, manifest = %s", got, manifest.SkillSHA256)
	}
	for _, marker := range []string{
		"missing, syncing, stale, incomplete, errored", "never call this a revenue drop",
		"14 fully elapsed 24-hour", "eligible, excluded/immature, and converted",
		"page through `list_findings` until exhausted", "including settled findings",
		"identical overlap/retry requests replay", "different payload under the same key returns a conflict",
		"no configured channel", "denied `plans:write`", "paused/disabled trigger",
		"does not justify a revenue or recovery finding", "not a promise", "never invent",
	} {
		if !strings.Contains(strings.ToLower(skill.Body), strings.ToLower(marker)) {
			t.Errorf("observer skill missing honesty rule %q", marker)
		}
	}
}
