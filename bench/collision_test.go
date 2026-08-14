package bench_test

// Live benchmark #2: an agentcore agent builds a two-airplane collision
// simulator web page (PROBLEM2.md) at the PRODUCTION context budget (120k
// tokens, default keep-recent window). A short build alone never fills 120k,
// so the test seeds what a real long-running session looks like: a durable
// session log carrying ~460 KB of prior design discussion, resumed via
// Config.ResumeSession. The rebuilt history puts the run right at the budget,
// so in-loop compaction MUST summarize the old span mid-run. The test then
// proves the property compaction exists for: the run keeps working *after*
// its history was summarized, the original goal survives into the last
// request the model saw, and the finished page still satisfies the spec.
// Metrics land in results/agentcore-collision.json and the page itself in
// results/collision.html.
//
// Same gate as the solver benchmark: AGENTRAY_TEST_OPENAI_{BASE_URL,API_KEY,MODEL}.
//
// Run: go test -timeout 30m -count=1 -v -run TestBench_AgentcoreCollisionSimCompaction ./bench/

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

// compactionMarkers mirror agentcore's compaction message prefixes (kept
// private there): a model-written summary, and the deterministic elide used
// when summarization degrades. Seeing either in a request proves compaction
// rewrote history before that turn.
const (
	summaryMarkerProbe = "[context summary of earlier conversation]"
	elideMarkerProbe   = "[context compaction]"
)

// benchContextBudget is the production default soft compaction ceiling —
// the bench exercises compaction at the real budget, not an artificial one.
const benchContextBudget = 120_000

// seedTargetBytes sizes the fabricated prior-session history: ~460 KB is
// ~115k estimated tokens, so the resumed run sits at the 120k budget and
// tips over it within the first build turns.
const seedTargetBytes = 460_000

// probeRecorder wraps the real provider and records every request, so the test
// can prove compaction happened (marker present) and the goal survived it.
type probeRecorder struct {
	inner agentcore.LLMProvider
	mu    sync.Mutex
	reqs  []agentcore.ChatRequest
}

func (r *probeRecorder) record(req agentcore.ChatRequest) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
}
func (r *probeRecorder) requests() []agentcore.ChatRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentcore.ChatRequest(nil), r.reqs...)
}
func (r *probeRecorder) Name() string        { return r.inner.Name() }
func (r *probeRecorder) SupportsTools() bool { return r.inner.SupportsTools() }
func (r *probeRecorder) Chat(ctx context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	r.record(req)
	return r.inner.Chat(ctx, req)
}
func (r *probeRecorder) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	r.record(req)
	return r.inner.Stream(ctx, req)
}

// pageWriter is the agent's only tool: it captures each full-page write so the
// test can inspect every milestone and the final artifact.
type pageWriter struct {
	mu     sync.Mutex
	writes []string // full page content, in order
}

func (w *pageWriter) Name() string { return "write_file" }
func (w *pageWriter) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: "write_file",
		Description: "Write the COMPLETE current HTML page to disk (path collision.html). " +
			"Call once per milestone with the entire page so far.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "file path (use collision.html)"},
				"content": map[string]any{"type": "string", "description": "the complete HTML document"},
			},
			"required": []any{"path", "content"},
		},
	}
}
func (w *pageWriter) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	w.mu.Lock()
	w.writes = append(w.writes, in.Content)
	n := len(w.writes)
	w.mu.Unlock()
	return fmt.Sprintf("wrote collision.html (milestone write #%d, %d bytes)", n, len(in.Content)), nil
}
func (w *pageWriter) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...)
}

// memSession is a minimal in-memory SessionStore so the test can seed a prior
// session log and let the agent resume it durably.
type memSession struct {
	mu  sync.Mutex
	log map[string][]agentcore.SessionEntry
}

func (s *memSession) Append(_ context.Context, id string, e agentcore.SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Seq = len(s.log[id]) + 1
	s.log[id] = append(s.log[id], e)
	return nil
}
func (s *memSession) Log(_ context.Context, id string) ([]agentcore.SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentcore.SessionEntry(nil), s.log[id]...), nil
}

// designNote fabricates one prior-session assistant message: deterministic,
// varied, summarizable design prose (~6 KB) — the bulk lives in assistant
// TEXT, the shape only LLM summarization (not tool-result eliding) can fold.
func designNote(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Design note %d — flight-dynamics and rendering considerations for the airplane collision simulator.\n", i)
	for p := 0; p < 10; p++ {
		fmt.Fprintf(&b, "Iteration %d.%d: the left aircraft enters at x=%d with cruise speed %d px/s while the "+
			"right aircraft mirrors it at %d px/s; closure rate is therefore %d px/s and the projected "+
			"intersection sits near the canvas midpoint. We considered easing the approach with a %d-frame ramp, "+
			"pooling %d explosion particles to avoid GC pauses, sampling the HUD every %d frames, and clamping "+
			"debris spin to %d rad/s. Canvas layering keeps the sky gradient static while sprites redraw; the "+
			"collision predicate compares squared distance against the sum of hull radii to avoid a sqrt per "+
			"frame. Restart rewinds the deterministic clock rather than reloading the page, and the speed slider "+
			"rescales both velocity vectors symmetrically so the impact point stays centered.\n",
			i, p, 40+i, 90+p, 100+((i+p)%17), 190+p, 12+(i%9), 200+10*p, 3+(p%5), 6+(i%7))
	}
	return b.String()
}

func TestBench_AgentcoreCollisionSimCompaction(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("set AGENTRAY_TEST_OPENAI_BASE_URL, AGENTRAY_TEST_OPENAI_API_KEY, AGENTRAY_TEST_OPENAI_MODEL to run the live benchmark")
	}
	rec := &probeRecorder{inner: ai.NewOpenAIProvider(key, base, ai.DefaultCompat())}

	problem, err := os.ReadFile("PROBLEM2.md")
	if err != nil {
		t.Fatalf("read PROBLEM2.md: %v", err)
	}

	// Seed the durable log with a long prior design session, ending with the
	// build task as the fresh, unanswered user message the resumed run acts on.
	const sessionID = "bench-collision-session"
	store := &memSession{log: map[string][]agentcore.SessionEntry{}}
	seed := func(role agentcore.Role, content string) {
		m := agentcore.Message{Role: role, Content: content}
		if err := store.Append(context.Background(), sessionID, agentcore.SessionEntry{
			Kind: agentcore.EntryMessage, Message: &m,
		}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}
	// The first user message becomes the compaction goal pin — it must carry
	// the actual objective.
	seed(agentcore.RoleUser, "We are designing an airplane collision simulator: a single self-contained "+
		"web page where two airplanes fly toward each other, collide, and explode. Think through the "+
		"design with me before we build it.")
	seedBytes := 0
	for i := 0; seedBytes < seedTargetBytes; i++ {
		n := designNote(i)
		seedBytes += len(n)
		seed(agentcore.RoleAssistant, n)
	}
	seed(agentcore.RoleUser, string(problem))

	writer := &pageWriter{}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 18
	limits.MaxToolCalls = 24
	// The production budget. The seeded history alone estimates just under it,
	// so the first build turns tip the run over and compaction must fold the
	// old design discussion mid-build.
	limits.MaxContextTokens = benchContextBudget

	agent, err := agentcore.New(agentcore.Config{
		Provider:  rec,
		Model:     model,
		Limits:    &limits,
		MaxTokens: 16000, // a full page per turn needs output headroom
		// Match production: stable cache key so the re-sent prefix is cache-read.
		PromptCacheKey:       "bench-collision",
		PromptCacheRetention: "short",
		Tools:                agentcore.NewToolSet(writer),
		Policy:               agentcore.NewAllowList("write_file"),
		Session:              store,
		SessionID:            sessionID,
		ResumeSession:        true,
		Definition: agentcore.AgentDefinition{
			Agents: "You are an expert front-end engineer. Follow the milestone plan in the task " +
				"EXACTLY: after each milestone call write_file(path, content) with the COMPLETE " +
				"current HTML page — never a fragment, never HTML in your text reply. Keep prose " +
				"to one short sentence per turn. After milestone 4 is written, reply with exactly: SHIPPED.",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := agent.Prompt(ctx, string(problem)) // input is superseded by the resumed log's task
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	writes := writer.snapshot()
	reqs := rec.requests()

	// --- Compaction actually fired. ---
	firstCompacted := -1 // index of the first request that saw compacted history
	summaries, elides := 0, 0
	for i, req := range reqs {
		for _, m := range req.Messages {
			if strings.HasPrefix(m.Content, summaryMarkerProbe) {
				summaries++
				if firstCompacted < 0 {
					firstCompacted = i
				}
				break
			}
			if strings.HasPrefix(m.Content, elideMarkerProbe) {
				elides++
				if firstCompacted < 0 {
					firstCompacted = i
				}
				break
			}
		}
	}
	t.Logf("behavior: turns=%d stop=%q writes=%d requests=%d seed_bytes=%d first_compacted_request=%d "+
		"(summary-bearing=%d elide-bearing=%d) wall=%s usage=%+v",
		res.Turns, res.StopReason, len(writes), len(reqs), seedBytes, firstCompacted, summaries, elides,
		wall.Round(time.Second), res.Usage)

	if firstCompacted < 0 {
		t.Fatalf("compaction never fired at the %d budget: no request carried a summary/elide marker "+
			"(turns=%d, requests=%d)", benchContextBudget, res.Turns, len(reqs))
	}
	// The run must have kept BUILDING after history was rewritten: at least one
	// provider request (i.e. one more working turn) came after the first
	// compacted request, and the milestone writes kept landing.
	if firstCompacted >= len(reqs)-1 {
		t.Fatalf("no working turn happened after compaction (first compacted request is the last request)")
	}
	if len(writes) < 3 {
		t.Fatalf("expected at least 3 milestone writes, got %d", len(writes))
	}

	// --- The goal survived compaction into the LAST request the model saw. ---
	last := reqs[len(reqs)-1]
	var goalSurvived bool
	for _, m := range last.Messages {
		lc := strings.ToLower(m.Content)
		if strings.Contains(lc, "airplane") || strings.Contains(lc, "collision") {
			goalSurvived = true
			break
		}
	}
	if !goalSurvived {
		t.Fatal("the airplane-collision goal is absent from the final request — compaction lost the task")
	}
	// --- Compaction actually SHRANK the context: the final request must be far
	// smaller than the seeded history it replaced. ---
	lastBytes := 0
	for _, m := range last.Messages {
		lastBytes += len(m.Content)
		for _, tc := range m.ToolCalls {
			lastBytes += len(tc.Arguments)
		}
	}
	if lastBytes > seedBytes/2 {
		t.Errorf("final request still carries %d bytes against a %d-byte seed — compaction did not shrink history", lastBytes, seedBytes)
	}

	// --- The finished page satisfies the spec (loose, model-agnostic checks). ---
	final := writes[len(writes)-1]
	low := strings.ToLower(final)
	checks := map[string]bool{
		"self-contained html":    strings.Contains(low, "<html") && strings.Contains(low, "</html>"),
		"canvas scene":           strings.Contains(low, "<canvas"),
		"animation loop":         strings.Contains(low, "requestanimationframe"),
		"collision logic":        strings.Contains(low, "collision") || strings.Contains(low, "collide"),
		"two planes":             strings.Contains(low, "plane"),
		"restart control":        strings.Contains(low, "restart") || strings.Contains(low, "reset"),
		"substantial (≥4 KB)":    len(final) >= 4096,
		"no external references": !strings.Contains(low, "http://") && !strings.Contains(low, "https://") && !strings.Contains(low, "<script src"),
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("final page fails check: %s", name)
		}
	}

	// --- Persist artifacts for human inspection + the comparison record. ---
	if werr := os.WriteFile("results/collision.html", []byte(final), 0o644); werr != nil {
		t.Logf("could not save page: %v", werr)
	}
	report := map[string]any{
		"solver":                  "agentcore",
		"model":                   model,
		"date":                    time.Now().Format("2006-01-02"),
		"problem":                 "PROBLEM2.md (airplane collision simulator, compaction probe)",
		"context_budget_tokens":   benchContextBudget,
		"seeded_history_bytes":    seedBytes,
		"turns":                   res.Turns,
		"stop_reason":             res.StopReason,
		"wall_seconds":            wall.Seconds(),
		"usage":                   res.Usage,
		"milestone_writes":        len(writes),
		"requests":                len(reqs),
		"first_compacted_request": firstCompacted,
		"summary_bearing_reqs":    summaries,
		"elide_bearing_reqs":      elides,
		"final_page_bytes":        len(final),
		"final_request_bytes":     lastBytes,
		"final":                   res.Final,
	}
	if buf, jerr := json.MarshalIndent(report, "", "  "); jerr == nil {
		if werr := os.WriteFile("results/agentcore-collision.json", buf, 0o644); werr != nil {
			t.Logf("could not save report: %v", werr)
		}
	}
}
