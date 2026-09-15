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
	updated, err := s.UpdateProjectForUser(context.Background(), userID, projectID, ProjectUpdate{Timezone: &zone})
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
	if _, err := s.UpdateProjectForUser(context.Background(), userID, projectID, ProjectUpdate{Timezone: &invalid}); !errors.Is(err, ErrInvalidProjectTimezone) {
		t.Fatalf("invalid update error = %v, want ErrInvalidProjectTimezone", err)
	}
}

func TestProjectGoalAndActivationEventPersist(t *testing.T) {
	s := openConvTestStore(t)
	userID, projectID := seedConvProject(t, s)

	// Initial: goal is nil (never asked), activation_event is empty
	initial, err := s.ProjectByIDForUser(context.Background(), userID, projectID)
	if err != nil {
		t.Fatalf("initial project: %v", err)
	}
	if initial.Goal != nil {
		t.Fatalf("initial goal = %v, want nil", initial.Goal)
	}
	if initial.ActivationEvent != "" {
		t.Fatalf("initial activation_event = %q, want empty", initial.ActivationEvent)
	}

	// Update goal to activation and set activation event
	goal := "activation"
	actEvent := "onboarding.completed"
	updated, err := s.UpdateProjectForUser(context.Background(), userID, projectID, ProjectUpdate{
		Goal:            &goal,
		ActivationEvent: &actEvent,
	})
	if err != nil {
		t.Fatalf("update goal/event: %v", err)
	}
	if updated.Goal == nil || *updated.Goal != "activation" {
		t.Fatalf("updated goal = %v, want 'activation'", updated.Goal)
	}
	if updated.ActivationEvent != actEvent {
		t.Fatalf("updated activation_event = %q, want %q", updated.ActivationEvent, actEvent)
	}

	// Reload and verify
	reloaded, err := s.ProjectByIDForUser(context.Background(), userID, projectID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Goal == nil || *reloaded.Goal != "activation" {
		t.Fatalf("reloaded goal = %v, want 'activation'", reloaded.Goal)
	}
	if reloaded.ActivationEvent != actEvent {
		t.Fatalf("reloaded activation_event = %q, want %q", reloaded.ActivationEvent, actEvent)
	}

	// Test invalid goal rejected
	badGoal := "make_billions"
	if _, err := s.UpdateProjectForUser(context.Background(), userID, projectID, ProjectUpdate{Goal: &badGoal}); !errors.Is(err, ErrInvalidProjectGoal) {
		t.Fatalf("bad goal error = %v, want ErrInvalidProjectGoal", err)
	}

	// Test skip goal
	emptyGoal := ""
	skipped, err := s.UpdateProjectForUser(context.Background(), userID, projectID, ProjectUpdate{Goal: &emptyGoal})
	if err != nil {
		t.Fatalf("skip goal: %v", err)
	}
	if skipped.Goal == nil || *skipped.Goal != "skipped" {
		t.Fatalf("skipped goal = %v, want 'skipped'", skipped.Goal)
	}
}
