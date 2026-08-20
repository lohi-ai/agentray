package storage

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAgentMemoryFoldInLive exercises memory hygiene against live Postgres: the
// write-time fold (a re-derived fact folds into the row it repeats instead of
// appending a paraphrase of it) and the soft supersede (a retracted memory is
// filtered out of both recall paths without leaving the store).
//
// A store that only appends is not merely untidy: the reflection pass re-derives
// the same learning every run and words it differently each time, so the
// 500-row vector-candidate cap eventually holds nothing but restatements and
// starves older, better memories out of recall entirely.
//
// Skipped unless AGENTRAY_LIVE_PG is set, matching TestRecommendationDedupeLive,
// so CI and the default `go test` never touch a real cluster.
//
// Run with the docker-compose stack up:
//
//	AGENTRAY_LIVE_PG=postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable \
//	go test ./internal/dataplane/store/ -run TestAgentMemoryFoldInLive -v
func TestAgentMemoryFoldInLive(t *testing.T) {
	dsn := os.Getenv("AGENTRAY_LIVE_PG")
	if dsn == "" {
		t.Skip("set AGENTRAY_LIVE_PG to run the live agent-memory hygiene test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	s := &Store{pg: pool}

	// A throwaway scope so the assertions cannot be perturbed by, or perturb, a
	// real agent's memory. agent_memory.scope_id carries no FK, so a bare uuid
	// is enough; the rows are deleted on the way out.
	var scopeID string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&scopeID); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, `DELETE FROM agent_memory WHERE scope_id = $1::uuid`, scopeID); err != nil {
			t.Errorf("cleanup memory: %v", err)
		}
	}()

	// One learning, derived twice by consecutive runs of the same schedule and
	// worded differently each time — the exact shape the reflection pass emits.
	const (
		first  = "The signup funnel loses most users at the email verification step, not at the payment step."
		reword = "The signup funnel loses most users at the email-verification step rather than at payment."
		other  = "Mobile accounts for roughly 80 percent of all reading sessions."
	)

	remember := func(kind, content string, confidence float64) {
		t.Helper()
		if err := s.RememberAgentMemory(ctx, AgentMemoryRow{
			ScopeID: scopeID, Kind: kind, Content: content, Confidence: confidence,
		}); err != nil {
			t.Fatalf("remember %q: %v", content, err)
		}
	}

	remember("learning", first, 0.6)
	remember("learning", reword, 0.7)

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_memory WHERE scope_id = $1::uuid`, scopeID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("two wordings of one learning produced %d rows, want 1 (one folded)", rows)
	}

	var (
		seen       int
		content    string
		confidence float64
	)
	if err := pool.QueryRow(ctx, `
SELECT seen_count, content, confidence FROM agent_memory WHERE scope_id = $1::uuid`, scopeID).
		Scan(&seen, &content, &confidence); err != nil {
		t.Fatalf("read folded row: %v", err)
	}
	if seen != 2 {
		t.Errorf("seen_count = %d, want 2 — the repeat did not record that it was seen again", seen)
	}
	// The newest wording wins, so the row reflects the latest run…
	if content != reword {
		t.Errorf("content = %q, want the newest wording %q", content, reword)
	}
	// …and the highest confidence observed is kept, so a lower-confidence
	// restatement can never quietly demote a fact the model asserted outright.
	if confidence != 0.7 {
		t.Errorf("confidence = %v, want 0.7 (the highest observed)", confidence)
	}

	// A genuinely different fact must still earn its own row: the threshold has
	// to be conservative, because a wrong fold overwrites the older wording.
	remember("learning", other, 0.6)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_memory WHERE scope_id = $1::uuid`, scopeID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2 {
		t.Fatalf("an unrelated fact produced %d rows in total, want 2 — the threshold is swallowing real memories", rows)
	}

	// The fold is kind-scoped: the same words filed as a different kind of
	// memory are a different claim about the world, not a restatement.
	remember("fact", first, 0.7)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_memory WHERE scope_id = $1::uuid`, scopeID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 3 {
		t.Fatalf("the same wording under a different kind produced %d rows in total, want 3", rows)
	}

	// Both recall paths see all three before any retraction. The keyword path is
	// asked for a term only the funnel memories carry.
	got, err := s.RecallAgentMemory(ctx, scopeID, "signup funnel verification", 8)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("keyword recall returned %d rows, want the 2 funnel memories", len(got))
	}

	// Retract the learning in favour of the fact. It stays in the table — the
	// agent did hold that belief — but neither recall path may return it again.
	var learningID, factID string
	if err := pool.QueryRow(ctx, `
SELECT id::text FROM agent_memory WHERE scope_id = $1::uuid AND kind = 'learning' AND content = $2`,
		scopeID, reword).Scan(&learningID); err != nil {
		t.Fatalf("find learning: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT id::text FROM agent_memory WHERE scope_id = $1::uuid AND kind = 'fact'`, scopeID).Scan(&factID); err != nil {
		t.Fatalf("find fact: %v", err)
	}
	if err := s.SupersedeAgentMemory(ctx, scopeID, learningID, factID); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_memory WHERE scope_id = $1::uuid`, scopeID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 3 {
		t.Errorf("supersede deleted a row (%d left, want 3) — the retraction must be soft", rows)
	}

	got, err = s.RecallAgentMemory(ctx, scopeID, "signup funnel verification", 8)
	if err != nil {
		t.Fatalf("recall after supersede: %v", err)
	}
	for _, m := range got {
		if m.ID == learningID {
			t.Fatalf("keyword recall returned the superseded memory %s", learningID)
		}
	}
	// The recency fallback (empty query) must filter it too.
	recent, err := s.RecallAgentMemory(ctx, scopeID, "", 8)
	if err != nil {
		t.Fatalf("recency recall: %v", err)
	}
	if len(recent) != 2 {
		t.Errorf("recency recall returned %d rows, want 2 (the superseded one filtered)", len(recent))
	}
	for _, m := range recent {
		if m.ID == learningID {
			t.Fatalf("recency recall returned the superseded memory %s", learningID)
		}
		if m.SeenCount < 1 || m.LastSeenAt.IsZero() {
			t.Errorf("row %s came back with seen_count=%d last_seen_at=%v — the hygiene columns are not being read", m.ID, m.SeenCount, m.LastSeenAt)
		}
	}

	// A superseded row must not resurrect as a fold target either: re-deriving
	// the retracted learning has to open a fresh row, not silently un-retract.
	remember("learning", reword, 0.6)
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM agent_memory WHERE scope_id = $1::uuid AND kind = 'learning' AND superseded_by IS NULL`,
		scopeID).Scan(&rows); err != nil {
		t.Fatalf("count live learnings: %v", err)
	}
	if rows != 2 {
		t.Errorf("re-deriving a retracted learning left %d live learning rows, want 2 (the unrelated one plus a fresh row)", rows)
	}

	// The vector-candidate path filters superseded rows as well. These rows have
	// no embedding, so the query must simply come back empty rather than
	// leaking a retracted row into ranking.
	cands, err := s.RecallAgentMemoryCandidates(ctx, scopeID, 100)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	for _, m := range cands {
		if m.ID == learningID {
			t.Fatalf("vector candidates returned the superseded memory %s", learningID)
		}
	}
}
