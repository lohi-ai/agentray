// Package cronx is the platform's minimal 5-field cron matcher, shared by the
// agent scheduler and the data-connector sync engine so both clocks agree on
// what an expression means.
package cronx

import (
	"strconv"
	"strings"
	"time"
)

// Matches reports whether a 5-field cron expression (minute hour dom month
// dow) matches t. Supports '*', '*/n', 'a-b', and comma lists. This is a
// minimal matcher sufficient for daily/hourly schedules; richer cron syntax is
// a later upgrade.
func Matches(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	return fieldMatches(fields[0], t.Minute()) &&
		fieldMatches(fields[1], t.Hour()) &&
		fieldMatches(fields[2], t.Day()) &&
		fieldMatches(fields[3], int(t.Month())) &&
		fieldMatches(fields[4], int(t.Weekday()))
}

func fieldMatches(field string, val int) bool {
	for _, part := range strings.Split(field, ",") {
		if singleFieldMatches(strings.TrimSpace(part), val) {
			return true
		}
	}
	return false
}

func singleFieldMatches(part string, val int) bool {
	step := 1
	if i := strings.Index(part, "/"); i >= 0 {
		s, err := strconv.Atoi(part[i+1:])
		if err != nil || s <= 0 {
			return false
		}
		step = s
		part = part[:i]
	}
	if part == "*" || part == "" {
		return val%step == 0
	}
	if i := strings.Index(part, "-"); i >= 0 {
		lo, err1 := strconv.Atoi(part[:i])
		hi, err2 := strconv.Atoi(part[i+1:])
		if err1 != nil || err2 != nil {
			return false
		}
		return val >= lo && val <= hi && (val-lo)%step == 0
	}
	n, err := strconv.Atoi(part)
	if err != nil {
		return false
	}
	return n == val
}
