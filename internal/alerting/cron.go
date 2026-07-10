package alerting

import (
	"strconv"
	"strings"
	"time"
)

// CronMatches reports whether a 5-field cron expression (minute hour dom month
// dow) matches t. Supports '*', '*/n', 'a-b', and comma lists — the same minimal
// matcher the agent scheduler uses for watchdog schedules. Kept as a small local
// copy so this package does not depend on agentruntime (which depends on storage,
// which this package also uses).
func CronMatches(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	return cronField(fields[0], t.Minute()) &&
		cronField(fields[1], t.Hour()) &&
		cronField(fields[2], t.Day()) &&
		cronField(fields[3], int(t.Month())) &&
		cronField(fields[4], int(t.Weekday()))
}

func cronField(field string, val int) bool {
	for _, part := range strings.Split(field, ",") {
		if cronSingle(strings.TrimSpace(part), val) {
			return true
		}
	}
	return false
}

func cronSingle(part string, val int) bool {
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
