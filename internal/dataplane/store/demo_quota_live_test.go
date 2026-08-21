package storage

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// The half of the demo budget only a real Postgres can prove: that the ledger
// counts per user, resets with the day, and cannot be raced past. Skipped
// unless AGENTRAY_LIVE_PG is set, matching the other *_live_test.go files.
//
//	AGENTRAY_LIVE_PG=postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable \
//	go test ./internal/dataplane/store/ -run DemoQuota -v

func quotaTestUser(t *testing.T, s *Store, ctx context.Context) string {
	t.Helper()
	var id string
	email := "quota-" + uuid.NewString() + "@example.com"
	if err := s.pg.QueryRow(ctx, `
INSERT INTO users (email, name, password_hash) VALUES ($1, $1, 'x') RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.pg.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, id); err != nil {
			t.Errorf("cleanup user %s: %v", id, err)
		}
	})
	return id
}

func TestDemoQuotaCountsPerUser(t *testing.T) {
	s, ctx := demoTestStore(t)
	alice, bob := quotaTestUser(t, s, ctx), quotaTestUser(t, s, ctx)

	const limit = 3
	for i := 1; i <= limit; i++ {
		quota, err := s.ConsumeDemoAgentRun(ctx, alice, limit)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !quota.Allowed || quota.Used != i {
			t.Fatalf("run %d: allowed=%v used=%d, want true/%d", i, quota.Allowed, quota.Used, i)
		}
	}
	quota, err := s.ConsumeDemoAgentRun(ctx, alice, limit)
	if err != nil {
		t.Fatalf("run %d: %v", limit+1, err)
	}
	if quota.Allowed {
		t.Fatalf("run %d was allowed past a cap of %d", limit+1, limit)
	}
	// The refused attempt must not have incremented anything.
	if used, err := s.DemoAgentRunsUsed(ctx, alice); err != nil || used != limit {
		t.Errorf("after a refusal, used = %d (err %v), want %d — a rejected attempt must not count", used, err, limit)
	}

	// Bob's budget is his own.
	if quota, err := s.ConsumeDemoAgentRun(ctx, bob, limit); err != nil || !quota.Allowed || quota.Used != 1 {
		t.Errorf("second user: allowed=%v used=%d err=%v, want true/1/nil", quota.Allowed, quota.Used, err)
	}
}

// Yesterday's spending does not come out of today's budget, and today's does
// not survive into tomorrow. The row is keyed by day, so this is asserted by
// planting a full day's worth of runs on another date.
func TestDemoQuotaIsPerDay(t *testing.T) {
	s, ctx := demoTestStore(t)
	user := quotaTestUser(t, s, ctx)

	const limit = 2
	if _, err := s.pg.Exec(ctx, `
INSERT INTO demo_agent_run_quota (user_id, day, runs)
VALUES ($1::uuid, (now() AT TIME ZONE 'utc')::date - 1, $2)`, user, limit); err != nil {
		t.Fatalf("plant yesterday: %v", err)
	}
	quota, err := s.ConsumeDemoAgentRun(ctx, user, limit)
	if err != nil {
		t.Fatalf("today's first run: %v", err)
	}
	if !quota.Allowed || quota.Used != 1 {
		t.Fatalf("yesterday's spending leaked into today: allowed=%v used=%d", quota.Allowed, quota.Used)
	}
}

// The whole reason the increment and the ceiling test are one statement: twenty
// questions fired at once must not all observe used < limit and all proceed.
func TestDemoQuotaHoldsUnderConcurrency(t *testing.T) {
	s, ctx := demoTestStore(t)
	user := quotaTestUser(t, s, ctx)

	const limit = 5
	const attempts = 40
	var wg sync.WaitGroup
	granted := make(chan struct{}, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			quota, err := s.ConsumeDemoAgentRun(ctx, user, limit)
			if err != nil {
				t.Errorf("concurrent run: %v", err)
				return
			}
			if quota.Allowed {
				granted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(granted)
	if got := len(granted); got != limit {
		t.Fatalf("%d of %d concurrent questions were granted, want exactly %d", got, attempts, limit)
	}
	if used, err := s.DemoAgentRunsUsed(ctx, user); err != nil || used != limit {
		t.Errorf("ledger says %d (err %v), want %d", used, err, limit)
	}
}
