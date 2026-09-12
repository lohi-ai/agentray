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

// ErrNoProjectFields means an update carried neither name nor timezone — a
// client validation failure, not a permission denial.
var ErrNoProjectFields = errors.New("update project: name or timezone is required")

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
