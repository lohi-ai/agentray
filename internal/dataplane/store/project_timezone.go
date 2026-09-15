package storage

import (
	"errors"
	"fmt"
	"strings"
	"time"

	_ "time/tzdata"
)

// ErrInvalidProjectTimezone means a requested timezone is not a named IANA
// location supported by the server's embedded timezone database.
var ErrInvalidProjectTimezone = errors.New("invalid project timezone")

// ErrNoProjectFields means an update carried no writable field — a client
// validation failure, not a permission denial.
var ErrNoProjectFields = errors.New("update project: at least one field is required")

// ErrInvalidProjectGoal means a goal write named something outside the fixed
// onboarding vocabulary. 'skipped' is a real stored value — it is how the
// prompt knows the owner declined, distinct from NULL (never asked).
var ErrInvalidProjectGoal = errors.New("invalid project goal")

// ErrInvalidActivationEvent means an activation-event write was not a usable
// event name. Empty is allowed — it clears the mapping.
var ErrInvalidActivationEvent = errors.New("invalid activation event")

// projectGoals is the fixed set the onboarding prompt offers. The column is
// VARCHAR(32), so every value must fit it.
var projectGoals = map[string]bool{
	"activation": true,
	"retention":  true,
	"revenue":    true,
	"traffic":    true,
	"skipped":    true,
}

// normalizeProjectGoal lowercases and validates a goal write. Empty maps to
// 'skipped' — a client clearing the goal is answering "none", not un-asking
// the question (NULL is reserved for never-asked).
func normalizeProjectGoal(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "skipped", nil
	}
	if !projectGoals[value] {
		return "", fmt.Errorf("%w: %q (activation, retention, revenue, traffic, skipped)", ErrInvalidProjectGoal, value)
	}
	return value, nil
}

// normalizeActivationEvent trims and bounds an activation-event write. It is
// not checked against the live catalog — the owner may name an event that has
// not arrived yet, and the metric reports not_ready until it does.
func normalizeActivationEvent(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 255 {
		return "", fmt.Errorf("%w: longer than 255 characters", ErrInvalidActivationEvent)
	}
	return value, nil
}

// normalizeProjectTimezone accepts the IANA location names that Go ships with,
// including UTC, but rejects process-local and empty values. Empty is reserved
// for existing nullable rows and resolves through overviewProjectTimezone.
func normalizeProjectTimezone(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "Local" {
		return "", fmt.Errorf("%w: provide an IANA timezone such as America/Los_Angeles", ErrInvalidProjectTimezone)
	}
	if _, err := time.LoadLocation(value); err != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidProjectTimezone, value)
	}
	return value, nil
}

// overviewProjectTimezone converts nullable persisted timezone truth into the
// explicit response contract. Legacy rows retain UTC behavior and say so;
// invalid non-empty rows fail rather than silently changing a reported metric.
func overviewProjectTimezone(value string) (name, source string, loc *time.Location, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "UTC", "fallback", time.UTC, nil
	}
	name, err = normalizeProjectTimezone(value)
	if err != nil {
		return "", "", nil, err
	}
	loc, err = time.LoadLocation(name)
	if err != nil {
		return "", "", nil, fmt.Errorf("load project timezone %q: %w", name, err)
	}
	return name, "project", loc, nil
}
