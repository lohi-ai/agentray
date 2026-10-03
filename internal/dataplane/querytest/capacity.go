package querytest

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	RequiredProjects     = 3
	RequiredEvents       = 10_000_000
	RequiredExternalRows = 1_000_000
	RequiredConcurrency  = 4
	RequiredColdSamples  = 30
	RequiredWarmSamples  = 100
)

type CapacitySpec struct {
	HostDescription  string
	ArtifactDir      string
	Projects         int
	EventsPerProject int
	RowsPerProject   int
	Concurrency      int
	MixedDuration    time.Duration
}

// CapacitySpecFromEnv refuses an implicit or downscaled "capacity" run. Unit
// tests skip unless AGENTRAY_QUERY_CAPACITY=1; once enabled, missing host and
// artifact declarations are errors and the approved corpus is the minimum.
func CapacitySpecFromEnv() (CapacitySpec, error) {
	spec := CapacitySpec{
		HostDescription:  os.Getenv("AGENTRAY_QUERY_CAPACITY_HOST"),
		ArtifactDir:      os.Getenv("AGENTRAY_QUERY_CAPACITY_ARTIFACT_DIR"),
		Projects:         envInt("AGENTRAY_QUERY_CAPACITY_PROJECTS", RequiredProjects),
		EventsPerProject: envInt("AGENTRAY_QUERY_CAPACITY_EVENTS", RequiredEvents),
		RowsPerProject:   envInt("AGENTRAY_QUERY_CAPACITY_EXTERNAL_ROWS", RequiredExternalRows),
		Concurrency:      envInt("AGENTRAY_QUERY_CAPACITY_CONCURRENCY", RequiredConcurrency),
		MixedDuration:    envDuration("AGENTRAY_QUERY_CAPACITY_DURATION", 10*time.Minute),
	}
	if spec.HostDescription == "" || spec.ArtifactDir == "" {
		return CapacitySpec{}, fmt.Errorf("set AGENTRAY_QUERY_CAPACITY_HOST and AGENTRAY_QUERY_CAPACITY_ARTIFACT_DIR")
	}
	if spec.Projects < RequiredProjects || spec.EventsPerProject < RequiredEvents ||
		spec.RowsPerProject < RequiredExternalRows || spec.Concurrency < RequiredConcurrency ||
		spec.MixedDuration < 10*time.Minute {
		return CapacitySpec{}, fmt.Errorf("capacity corpus may not be downscaled: projects>=%d events/project>=%d external/project>=%d concurrency>=%d duration>=10m",
			RequiredProjects, RequiredEvents, RequiredExternalRows, RequiredConcurrency)
	}
	if info, err := os.Stat(spec.ArtifactDir); err != nil || !info.IsDir() {
		return CapacitySpec{}, fmt.Errorf("capacity artifact directory must already exist: %s", spec.ArtifactDir)
	}
	return spec, nil
}

func envInt(name string, fallback int) int {
	if value := os.Getenv(name); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	if value := os.Getenv(name); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func CapacityProjectID(index int) string {
	return fmt.Sprintf("50000000-0000-4000-8000-%012d", index+1)
}

func CapacityConnectorID(index int) string {
	return fmt.Sprintf("60000000-0000-4000-8000-%012d", index+1)
}

type CapacityEvent struct {
	EventID, DistinctID string
	Timestamp           time.Time
}

func CapacityEvents(projectIndex, start, count int) []CapacityEvent {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	out := make([]CapacityEvent, count)
	for i := range count {
		row := start + i
		out[i] = CapacityEvent{
			EventID:    fmt.Sprintf("capacity-%d-%d", projectIndex, row),
			DistinctID: fmt.Sprintf("user-%d", row%1_000_000),
			Timestamp:  base.Add(time.Duration(row%(90*24*60)) * time.Minute),
		}
	}
	return out
}

type CapacityExternalRow struct{ Key, Data string }

func CapacityExternalRows(start, count int) []CapacityExternalRow {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	out := make([]CapacityExternalRow, count)
	for i := range count {
		row := start + i
		out[i] = CapacityExternalRow{
			Key: fmt.Sprintf("fact-%d", row),
			Data: fmt.Sprintf(`{"user_id":"user-%d","amount":"%d","occurred_at":"%s"}`,
				row%1_000_000, row%10_000, base.Add(time.Duration(row%(90*24*60))*time.Minute).Format(time.RFC3339)),
		}
	}
	return out
}
