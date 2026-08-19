package storage

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRecommendationDedupeLive exercises the recommendation de-duplication path
// against live Postgres. A scheduled agent re-derives its findings every cycle
// and words them slightly differently each time; before this folded, a single
// workspace had accumulated 165 open recommendations of which 80 restated an
// earlier one, and the owner had acted on exactly one of the 171 filed. This
// test pins the two halves of the behaviour that matter: a reworded repeat must
// fold into the row it repeats, and a genuinely different finding must not.
//
// Skipped unless AGENTRAY_LIVE_PG is set, matching TestAliasDictionaryLive, so
// CI and the default `go test` never touch a real cluster.
//
// Run with the docker-compose stack up:
//
//	AGENTRAY_LIVE_PG=postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable \
//	go test ./internal/dataplane/store/ -run TestRecommendationDedupeLive -v
func TestRecommendationDedupeLive(t *testing.T) {
	dsn := os.Getenv("AGENTRAY_LIVE_PG")
	if dsn == "" {
		t.Skip("set AGENTRAY_LIVE_PG to run the live recommendation de-dupe test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	s := &Store{pg: pool}

	// A throwaway project so the assertions cannot be perturbed by, or perturb,
	// the workspace's real recommendations. Dropped on the way out.
	var projectID string
	if err := pool.QueryRow(ctx, `
INSERT INTO projects (name, api_key) VALUES ('dedupe-live-test', 'dedupe-live-test-' || gen_random_uuid()::text)
RETURNING id::text`).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, `DELETE FROM projects WHERE id = $1::uuid`, projectID); err != nil {
			t.Errorf("cleanup project: %v", err)
		}
	}()

	// The two titles below are verbatim from the live workspace: one ingestion
	// problem, filed twice by consecutive runs of the same schedule.
	const (
		first  = "Investigate 24-hour ingestion gap and unplanned subscription replay"
		reword = "Investigate 24h ingestion gap and subscription-event replay"
		other  = "Fix production URL and pageview/session metadata"
	)

	firstID, err := s.CreateRecommendation(ctx, AgentRecommendation{
		ProjectID: projectID, Category: "data", Title: first, ImpactScore: 92,
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}

	rewordID, err := s.CreateRecommendation(ctx, AgentRecommendation{
		ProjectID: projectID, Category: "data", Title: reword, ImpactScore: 90,
	})
	if err != nil {
		t.Fatalf("create reworded repeat: %v", err)
	}
	if rewordID != firstID {
		t.Errorf("a reworded repeat opened a new card (%s) instead of folding into %s", rewordID, firstID)
	}

	otherID, err := s.CreateRecommendation(ctx, AgentRecommendation{
		ProjectID: projectID, Category: "data", Title: other, ImpactScore: 40,
	})
	if err != nil {
		t.Fatalf("create unrelated: %v", err)
	}
	if otherID == firstID {
		t.Errorf("an unrelated finding folded into %s — the threshold is swallowing real findings", firstID)
	}

	var seen int
	var impact float64
	var title string
	if err := pool.QueryRow(ctx, `
SELECT seen_count, impact_score, title FROM agent_recommendations WHERE id = $1::uuid`, firstID).
		Scan(&seen, &impact, &title); err != nil {
		t.Fatalf("read folded row: %v", err)
	}
	if seen != 2 {
		t.Errorf("seen_count = %d, want 2 — the repeat did not record that it was seen again", seen)
	}
	// The fold keeps the highest impact observed, so a repeat can never quietly
	// demote a finding down the readout's ranking.
	if impact != 92 {
		t.Errorf("impact_score = %v, want 92 (the highest observed)", impact)
	}
	// …and takes the newest wording, so the card reflects the latest run.
	if title != reword {
		t.Errorf("title = %q, want the newest wording %q", title, reword)
	}

	var open int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM agent_recommendations WHERE project_id = $1::uuid AND status = 'open'`, projectID).
		Scan(&open); err != nil {
		t.Fatalf("count: %v", err)
	}
	if open != 2 {
		t.Errorf("three filings produced %d cards, want 2 (one folded)", open)
	}
}
