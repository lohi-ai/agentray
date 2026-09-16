package storage

import (
	"context"
	"database/sql"
	"sort"
	"time"
)
// ActivationCandidate is one event name ranked as a possible activation event,
// with the evidence behind the rank: how many new users reached it inside
// their first 7 days (reach), and how much likelier they were to return on
// day 7 than the cohort baseline (lift). Rates are 0–1 fractions; the client
// scales them for display.
type ActivationCandidate struct {
	EventName  string  `json:"event_name"`
	Users      uint64  `json:"users"`
	Reach      float64 `json:"reach"`
	D7Return   float64 `json:"d7_return"`
	BaselineD7 float64 `json:"baseline_d7"`
	Lift       float64 `json:"lift"`
}

// ActivationCandidates is the ranked suggestion list plus the state the
// picker needs to choose between it and the raw catalog: "ok" carries ranked
// candidates, "not_ready" means the mature 7-day cohort is too small to rank
// honestly — never an empty ok list.
type ActivationCandidates struct {
	State      string                `json:"state"` // ok | not_ready
	CohortSize uint64                `json:"cohort_size"`
	Candidates []ActivationCandidate `json:"candidates"`
}

// activationCohortFloor is the smallest mature cohort that produces a ranking
// worth showing: below it a single user's day-7 return swings lift by points,
// so the picker serves the raw catalog instead of a confident-looking rank.
const activationCohortFloor = 20

// activationCandidateCap bounds the suggestion list the picker renders.
const activationCandidateCap = 8

// SuggestActivationEvents ranks a project's event names as activation-event
// candidates. The cohort is the same one overviewActivation computes against —
// first-ever qualifying event per stitched identity — restricted to members
// whose 7-day window has fully closed by `now`, so every reach and lift number
// is measured over a complete window. Reach is the share of that mature cohort
// firing the event on cohort days 0–7; lift is the D7 return rate of the
// users who fired it minus the cohort's own D7 rate. Read-only; the query
// mirrors the overview's resolved_events shape and stays bounded by the
// candidate cap.
func (s *Store) SuggestActivationEvents(ctx context.Context, projectID string, now time.Time) (ActivationCandidates, error) {
	out := ActivationCandidates{State: OverviewStateNotReady, Candidates: []ActivationCandidate{}}
	timezone, err := s.projectTimezone(ctx, projectID)
	if err != nil {
		return out, err
	}

	// The mature cohort: first-ever qualifying activity per identity, kept only
	// when its local 7-day window has fully closed — identical eligibility to
	// overviewActivation's `eligible`.
	firsts := `
SELECT cid, CAST(timezone(?, first_ts) AS DATE) AS cohort_day
FROM (
	SELECT canonical_distinct_id AS cid, min("timestamp") AS first_ts
	FROM resolved_events
	WHERE project_id = ? AND ` + overviewQualifying + `
	GROUP BY cid
)`
	var cohortSize uint64
	err = s.duckQueryRow(ctx, `
SELECT count(*)
FROM (`+firsts+`) f
WHERE f.cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)`,
		[]any{timezone, projectID, timezone, now}, &cohortSize)
	if err != nil {
		return out, err
	}
	out.CohortSize = cohortSize
	if cohortSize < activationCohortFloor {
		return out, nil
	}

	// Baseline D7: share of the mature cohort with a qualifying event on local
	// day 7 — the same day-N semantics overviewRetention uses.
	var baselineReturned uint64
	err = s.duckQueryRow(ctx, `
SELECT count(DISTINCT e.canonical_distinct_id)
FROM resolved_events e
INNER JOIN (`+firsts+`) f ON e.canonical_distinct_id = f.cid
WHERE e.project_id = ? AND `+overviewQualifying+`
  AND CAST(timezone(?, e."timestamp") AS DATE) = f.cohort_day + INTERVAL '7 days'
  AND f.cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)`,
		[]any{timezone, projectID, projectID, timezone, timezone, now}, &baselineReturned)
	if err != nil {
		return out, err
	}
	baseline := float64(baselineReturned) / float64(cohortSize)

	// Per event: users = mature cohort members who fired it on cohort days 0–7;
	// d7 = the day-7 return rate of exactly those members (EXISTS, so a member
	// who never returned still lands in the denominator).
	err = s.duckQuery(ctx, `
SELECT e.event_name,
	count(DISTINCT e.canonical_distinct_id) AS users,
	count(DISTINCT e.canonical_distinct_id) FILTER (WHERE EXISTS (
		SELECT 1 FROM resolved_events r
		INNER JOIN (`+firsts+`) rf ON r.canonical_distinct_id = rf.cid
		WHERE r.project_id = ? AND `+overviewQualifying+`
		  AND r.canonical_distinct_id = e.canonical_distinct_id
		  AND CAST(timezone(?, r."timestamp") AS DATE) = rf.cohort_day + INTERVAL '7 days'
	)) AS returned_d7
FROM resolved_events e
INNER JOIN (`+firsts+`) f ON e.canonical_distinct_id = f.cid
WHERE e.project_id = ? AND `+overviewQualifying+`
  AND CAST(timezone(?, e."timestamp") AS DATE) >= f.cohort_day
  AND CAST(timezone(?, e."timestamp") AS DATE) <= f.cohort_day + INTERVAL '7 days'
  AND f.cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)
GROUP BY e.event_name
ORDER BY users DESC
LIMIT ?`,
		// Placeholder order follows the SQL text: the EXISTS-clause `firsts`
		// subquery binds first (timezone, projectID), then the EXISTS WHERE
		// (projectID, timezone), then the FROM-clause `firsts` (timezone,
		// projectID), then the outer WHERE (projectID, timezone, timezone,
		// timezone, now), then the cap.
		[]any{timezone, projectID, projectID, timezone, timezone, projectID, projectID, timezone, timezone, timezone, now, activationCandidateCap},
		func(rows *sql.Rows) error {
			var c ActivationCandidate
			var returned uint64
			if err := rows.Scan(&c.EventName, &c.Users, &returned); err != nil {
				return err
			}
			c.Reach = float64(c.Users) / float64(cohortSize)
			c.D7Return = float64(returned) / float64(c.Users)
			c.BaselineD7 = baseline
			c.Lift = c.D7Return - baseline
			out.Candidates = append(out.Candidates, c)
			return nil
		})
	if err != nil {
		return out, err
	}

	// Rank by lift, then reach: an event that predicts day-7 return is the
	// better activation signal; reach breaks ties toward the common path.
	sort.SliceStable(out.Candidates, func(i, j int) bool {
		if out.Candidates[i].Lift != out.Candidates[j].Lift {
			return out.Candidates[i].Lift > out.Candidates[j].Lift
		}
		return out.Candidates[i].Reach > out.Candidates[j].Reach
	})
	out.State = OverviewStateOK
	return out, nil
}

// projectTimezone resolves the project's stored timezone the way Overview does.
// Test stores have no pg pool; they get UTC, matching the fallback contract.
func (s *Store) projectTimezone(ctx context.Context, projectID string) (string, error) {
	if s.pg == nil {
		return "UTC", nil
	}
	var stored string
	if err := s.pg.QueryRow(ctx, `SELECT coalesce(timezone, '') FROM projects WHERE id = $1`, projectID).Scan(&stored); err != nil {
		return "", err
	}
	name, _, _, err := overviewProjectTimezone(stored)
	return name, err
}
