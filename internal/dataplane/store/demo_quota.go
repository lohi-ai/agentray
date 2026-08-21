package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// The demo's spend control.
//
// An agent run started from inside the shared demo bills the INSTANCE owner's
// model key — the visitor is a guest on someone else's account — so "ask the
// agent anything" has to carry a ceiling, or the demo is an unbounded bill
// payable by whoever runs the instance. The ceiling is
// AGENTRAY_DEMO_AGENT_RUNS_PER_USER_PER_DAY (config.DemoAgentRunsPerUserPerDay).
//
// agent_runs cannot answer "how many did THIS person start today": it is keyed
// by project and agent and has no user column at all (a scheduled run has no
// user to record). Widening that table for one feature would put a nullable
// column on the hottest write path in the product, so the quota is its own
// two-column ledger instead.

// DemoRunQuota is the outcome of asking the ledger for one demo agent run.
// Used counts the runs the user has already started today INCLUDING the one
// just granted, so Used == Limit on the last allowed question.
type DemoRunQuota struct {
	Allowed  bool
	Used     int
	Limit    int
	ResetsAt time.Time
}

// ConsumeDemoAgentRun claims one run against the caller's daily demo budget and
// reports whether they may proceed.
//
// It counts the run when it STARTS, not when it finishes, and does the
// increment and the ceiling test in ONE statement. Both halves of that matter:
// a counter that only moves on completion lets someone fire twenty concurrent
// questions before the first of them lands, and a read-then-write pair lets two
// concurrent requests both observe used < limit and both proceed. The
// conditional ON CONFLICT gives the database the last word.
//
// The bucket is a UTC calendar day rather than a trailing 24h window because
// the refusal has to promise something a person can act on ("tomorrow"), and
// because a sliding window needs a row per run to compute.
//
// A limit of zero or less is NOT "unlimited": it is an operator who set the
// ceiling to nothing, and the honest reading of that is a demo whose agent does
// not answer visitors. Fail closed.
func (s *Store) ConsumeDemoAgentRun(ctx context.Context, userID string, limit int) (DemoRunQuota, error) {
	now := time.Now().UTC()
	quota := DemoRunQuota{Limit: limit, ResetsAt: nextUTCDay(now)}
	if limit <= 0 {
		return quota, nil
	}
	if _, err := uuid.Parse(userID); err != nil {
		// No caller, no budget to spend. Refusing here keeps a malformed id from
		// reaching the ledger as a cast error the caller would read as a 500.
		return quota, nil
	}
	err := s.pg.QueryRow(ctx, `
INSERT INTO demo_agent_run_quota (user_id, day, runs)
VALUES ($1::uuid, $2::date, 1)
ON CONFLICT (user_id, day) DO UPDATE
	SET runs = demo_agent_run_quota.runs + 1
	WHERE demo_agent_run_quota.runs < $3
RETURNING runs`, userID, now, limit).Scan(&quota.Used)
	// No row came back: the conditional update did not fire, which is the
	// ceiling refusing. pgx.ErrNoRows wraps sql.ErrNoRows, so one check covers
	// both.
	if errors.Is(err, sql.ErrNoRows) {
		quota.Used = limit
		return quota, nil
	}
	if err != nil {
		return quota, err
	}
	quota.Allowed = true
	return quota, nil
}

// DemoAgentRunsUsed reports how many demo runs a user has started today without
// claiming one. Read-only companion to ConsumeDemoAgentRun, for surfaces that
// want to show the remaining budget before the user spends it.
func (s *Store) DemoAgentRunsUsed(ctx context.Context, userID string) (int, error) {
	if _, err := uuid.Parse(userID); err != nil {
		return 0, nil
	}
	var used int
	err := s.pg.QueryRow(ctx, `
SELECT runs FROM demo_agent_run_quota
WHERE user_id = $1::uuid AND day = $2::date`, userID, time.Now().UTC()).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
}

// RefundDemoAgentRun gives back a claim that bought nothing.
//
// The budget is claimed at run START, before the model is called, because that
// is the only point where a concurrent claim can be refused (see
// ConsumeDemoAgentRun). The cost of doing it there is that a run which never
// reaches the provider — a bad model alias, an expired upstream token, a
// 429 from the router — still spends one of the visitor's few daily asks. On an
// instance whose model key is misconfigured that is the whole budget burned on
// errors, and the visitor is locked out for the day having never seen an answer.
//
// This is not a hole in the cap. The cap exists to bound what the INSTANCE
// OWNER pays for, and a call the provider refused costs the owner nothing, so
// refunding it takes nothing back from the thing being protected. The caller
// must only refund a run that reported zero token usage; a run that answered
// badly, or was cancelled after burning tokens, is spent and stays spent.
//
// GREATEST(runs - 1, 0) rather than runs - 1: the row is shared by every run
// the user started today, and a refund must never make the ledger negative and
// hand out a free extra ask.
func (s *Store) RefundDemoAgentRun(ctx context.Context, userID string) error {
	if _, err := uuid.Parse(userID); err != nil {
		return nil
	}
	_, err := s.pg.Exec(ctx, `
UPDATE demo_agent_run_quota
SET runs = GREATEST(runs - 1, 0)
WHERE user_id = $1::uuid AND day = $2::date`, userID, time.Now().UTC())
	return err
}

// nextUTCDay is the instant the caller's demo budget refills.
func nextUTCDay(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}
