package storage

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeProjectTimezone(t *testing.T) {
	zone, err := normalizeProjectTimezone(" America/Los_Angeles ")
	if err != nil || zone != "America/Los_Angeles" {
		t.Fatalf("valid timezone = %q, %v", zone, err)
	}
	for _, value := range []string{"", "Local", "America/Nope"} {
		if _, err := normalizeProjectTimezone(value); !errors.Is(err, ErrInvalidProjectTimezone) {
			t.Fatalf("%q error = %v, want ErrInvalidProjectTimezone", value, err)
		}
	}
}

func TestOverviewProjectTimezoneFallback(t *testing.T) {
	name, source, loc, err := overviewProjectTimezone("")
	if err != nil || name != "UTC" || source != "fallback" || loc.String() != "UTC" {
		t.Fatalf("legacy fallback = %q %q %v %v", name, source, loc, err)
	}
	name, source, loc, err = overviewProjectTimezone("Asia/Ho_Chi_Minh")
	if err != nil || name != "Asia/Ho_Chi_Minh" || source != "project" || loc.String() != "Asia/Ho_Chi_Minh" {
		t.Fatalf("project timezone = %q %q %v %v", name, source, loc, err)
	}
}

func TestProjectTimezonePersistsThroughOwnerUpdate(t *testing.T) {
	s := openConvTestStore(t)
	userID, projectID := seedConvProject(t, s)
	zone := "Asia/Ho_Chi_Minh"
	updated, err := s.UpdateProjectForUser(context.Background(), userID, projectID, nil, &zone)
	if err != nil {
		t.Fatalf("update timezone: %v", err)
	}
	if updated.Timezone != zone {
		t.Fatalf("updated timezone = %q, want %q", updated.Timezone, zone)
	}
	reloaded, err := s.ProjectByIDForUser(context.Background(), userID, projectID)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if reloaded.Timezone != zone {
		t.Fatalf("reloaded timezone = %q, want %q", reloaded.Timezone, zone)
	}
	invalid := "Local"
	if _, err := s.UpdateProjectForUser(context.Background(), userID, projectID, nil, &invalid); !errors.Is(err, ErrInvalidProjectTimezone) {
		t.Fatalf("invalid update error = %v, want ErrInvalidProjectTimezone", err)
	}
}
