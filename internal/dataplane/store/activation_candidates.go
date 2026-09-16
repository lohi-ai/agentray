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

// activationCandidatePool is how many event names the query returns ordered by
// reach before Go ranks them by lift — wide enough that a high-lift,
// lower-volume event is not truncated before its lift is measured.
const activationCandidatePool = 64

// activationCandidateCap bounds the suggestion list the picker renders.
const activationCandidateCap = 8

// activationFirstsCTE is the cohort both queries share: first-ever qualifying
// activity per stitched identity (the same `firsts` overviewActivation and
// overviewRetention build), narrowed to members whose local 7-day window has
// fully closed by the bound `now` — identical eligibility to the activation
// metric's `eligible`.
const activationFirstsCTE = `
WITH firsts AS (
	SELECT canonical_distinct_id AS cid, min("timestamp") AS first_ts
	FROM resolved_events
	WHERE project_id = ? AND ` + overviewQualifying + `
	GROUP BY cid
),
mature AS (
	SELECT cid, CAST(timezone(?, first_ts) AS DATE) AS cohort_day
	FROM firsts
	WHERE CAST(timezone(?, first_ts) AS DATE) + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)
),
returned AS (
	SELECT DISTINCT r.canonical_distinct_id AS cid
	FROM resolved_events r
	INNER JOIN mature m ON r.canonical_distinct_id = m.cid
	WHERE r.project_id = ? AND ` + overviewQualifying + `
	  AND CAST(timezone(?, r."timestamp") AS DATE) = m.cohort_day + INTERVAL '7 days'
)`

// SuggestActivationEvents ranks a project's event names as activation-event
// candidates. The cohort is the same one overviewActivation computes against —
// first-ever qualifying event per stitched identity — restricted to members
// whose 7-day window has fully closed by `now`, so every reach and lift number
// is measured over a complete window. Reach is the share of that mature cohort
// firing the event on cohort days 0–7; lift is the D7 return rate of the
// users who fired it minus the cohort's own D7 rate. Read-only; the query
// mirrors the overview's resolved_events shape and stays bounded by the
// candidate pool.
func (s *Store) SuggestActivationEvents(ctx context.Context, projectID string, now time.Time) (ActivationCandidates, error) {
	out := ActivationCandidates{State: OverviewStateNotReady, Candidates: []ActivationCandidate{}}
	timezone, err := s.projectTimezone(ctx, projectID)
	if err != nil {
		return out, err
	}

	// Cohort size and baseline D7 in one pass: baseline is the share of the
	// mature cohort with a qualifying event on local day 7 — the same day-N
	// semantics overviewRetention uses.
	var baselineReturned uint64
	err = s.duckQueryRow(ctx, activationFirstsCTE+`
SELECT (SELECT count(*) FROM mature), (SELECT count(*) FROM returned)`,
		[]any{projectID, timezone, timezone, timezone, now, projectID, timezone},
		&out.CohortSize, &baselineReturned)
	if err != nil {
		return out, err
	}
	if out.CohortSize < activationCohortFloor {
		return out, nil
	}
	baseline := float64(baselineReturned) / float64(out.CohortSize)

	// Per event: users = mature cohort members who fired it on cohort days 0–7;
	// d7 = the day-7 return rate of exactly those members (LEFT JOIN, so a
	// member who never returned still lands in the denominator). The pool is
	// ordered by reach and capped wide; Go ranks by lift after.
	err = s.duckQuery(ctx, activationFirstsCTE+`,
reached AS (
	SELECT e.event_name AS name, e.canonical_distinct_id AS cid
	FROM resolved_events e
	INNER JOIN mature m ON e.canonical_distinct_id = m.cid
	WHERE e.project_id = ? AND `+overviewQualifying+` AND e.event_name <> ''
	  AND CAST(timezone(?, e."timestamp") AS DATE) >= m.cohort_day
	  AND CAST(timezone(?, e."timestamp") AS DATE) <= m.cohort_day + INTERVAL '7 days'
	GROUP BY e.event_name, e.canonical_distinct_id
)
SELECT re.name, count(*) AS users, count(ret.cid) AS returned_d7
FROM reached re
LEFT JOIN returned ret ON ret.cid = re.cid
GROUP BY re.name
ORDER BY users DESC
LIMIT ?`,
		[]any{projectID, timezone, timezone, timezone, now, projectID, timezone, projectID, timezone, timezone, activationCandidatePool},
		func(rows *sql.Rows) error {
			var c ActivationCandidate
			var returned uint64
			if err := rows.Scan(&c.EventName, &c.Users, &returned); err != nil {
				return err
			}
			c.Reach = float64(c.Users) / float64(out.CohortSize)
			c.D7Return = float64(returned) / float64(c.Users)
			c.BaselineD7 = baseline
			c.Lift = c.D7Return - baseline
			out.Candidates = append(out.Candidates, c)
			return nil
		})
	if err != nil {
		return out, err
	}
	if len(out.Candidates) == 0 {
		return out, nil
	}

	// Rank by lift, then reach: an event that predicts day-7 return is the
	// better activation signal; reach breaks ties toward the common path.
	sort.SliceStable(out.Candidates, func(i, j int) bool {
		if out.Candidates[i].Lift != out.Candidates[j].Lift {
			return out.Candidates[i].Lift > out.Candidates[j].Lift
		}
		return out.Candidates[i].Reach > out.Candidates[j].Reach
	})
	if len(out.Candidates) > activationCandidateCap {
		out.Candidates = out.Candidates[:activationCandidateCap]
	}
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
