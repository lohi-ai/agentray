package bench_test

// Live benchmark #4: an agentcore agent builds "Thính Du Ký" — a single-file
// pixel active-idle web game for kiem-lai audiobook listeners (PROBLEM4.md).
// This is the first bench to exercise the GOAL GATE: Config.Goal demands the
// run end with STATUS: DONE only after `node verify.mjs` passes, so the loop
// keeps nudging a premature finish instead of accepting it.
//
// The agent gets the same local toolset as the TTS bench (run_shell,
// write_file, read_file, list_dir — types reused from tts_test.go) in a fresh
// scratch workspace. The harness monitors every provider request (goal
// nudges, compaction), then independently re-verifies the artifact: re-runs
// verify.mjs, applies its own static checks against game.html, and copies
// artifacts to results/game/ plus a run record to results/game/agentcore-game.json.
//
// Gate: AGENTRAY_TEST_OPENAI_{BASE_URL,API_KEY,MODEL}.
//
// Run: go test -timeout 70m -count=1 -v -run TestBench_AgentcoreIdleGame ./bench/

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/ai"
)

const goalNudgeMarkerProbe = "[goal gate]"

func TestBench_AgentcoreIdleGame(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("set AGENTRAY_TEST_OPENAI_BASE_URL, AGENTRAY_TEST_OPENAI_API_KEY, AGENTRAY_TEST_OPENAI_MODEL to run the live benchmark")
	}
	rec := &probeRecorder{inner: ai.NewOpenAIProvider(key, base, ai.DefaultCompat())}

	problem, err := os.ReadFile("PROBLEM4.md")
	if err != nil {
		t.Fatalf("read PROBLEM4.md: %v", err)
	}

	// Fresh workspace per run (AGENTRAY_BENCH_GAME_WORKSPACE keeps one for
	// debugging — it is then NOT wiped).
	ws := strings.TrimSpace(os.Getenv("AGENTRAY_BENCH_GAME_WORKSPACE"))
	if ws == "" {
		ws = filepath.Join(os.TempDir(), "agentray-bench-game")
		if err := os.RemoveAll(ws); err != nil {
			t.Fatalf("clean workspace: %v", err)
		}
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mk workspace: %v", err)
	}
	ws, err = filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	t.Logf("workspace: %s", ws)

	shell := &ttsShellTool{root: ws, env: os.Environ()}
	writer := &ttsWriteTool{root: ws}
	reader := &ttsReadTool{root: ws}
	lister := &ttsListTool{root: ws}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 60
	limits.MaxToolCalls = 150

	obs := newRunObserver()
	agent, err := obs.build(agentcore.Config{
		Provider:             rec,
		Model:                model,
		Limits:               &limits,
		MaxTokens:            16000,
		PromptCacheKey:       "bench-game",
		PromptCacheRetention: "short",
		Tools:                agentcore.NewToolSet(shell, writer, reader, lister),
		Policy:               agentcore.NewAllowList("run_shell", "write_file", "read_file", "list_dir"),
		Goal: "game.html, verify.mjs, and README.md exist in the workspace root, `node verify.mjs` " +
			"has been run via run_shell and exited 0 with every check green, and the game meets all " +
			"hard requirements in the task.",
		Definition: agentcore.AgentDefinition{
			Agents: "You are a senior web-game developer working alone in a shell workspace. Build " +
				"exactly what the task asks — ship quality over scope. Use write_file for files and " +
				"run_shell to verify; iterate until your own verify.mjs passes. Keep text replies to " +
				"one or two short sentences — the work product is files, not prose.",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := agent.Prompt(ctx, string(problem))
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	reqs := rec.requests()
	shellLog := shell.snapshot()

	// --- Harness monitoring: goal-gate + compaction behavior. ---
	firstCompacted, summaries, elides, goalNudges := -1, 0, 0, 0
	for i, req := range reqs {
		compacted := false
		for _, m := range req.Messages {
			if strings.HasPrefix(m.Content, goalNudgeMarkerProbe) {
				goalNudges++
			}
			if !compacted && strings.HasPrefix(m.Content, summaryMarkerProbe) {
				summaries++
				compacted = true
				if firstCompacted < 0 {
					firstCompacted = i
				}
			}
			if !compacted && strings.HasPrefix(m.Content, elideMarkerProbe) {
				elides++
				compacted = true
				if firstCompacted < 0 {
					firstCompacted = i
				}
			}
		}
	}
	// Nudges accumulate in the transcript of every later request; count the
	// distinct ones from the final request only.
	if len(reqs) > 0 {
		goalNudges = 0
		for _, m := range reqs[len(reqs)-1].Messages {
			if strings.HasPrefix(m.Content, goalNudgeMarkerProbe) {
				goalNudges++
			}
		}
	}
	shellFails := 0
	for _, e := range shellLog {
		if e.Exit != 0 {
			shellFails++
		}
	}
	t.Logf("behavior: turns=%d stop=%q requests=%d shell_calls=%d (nonzero_exit=%d) file_writes=%d "+
		"goal_nudges=%d first_compacted_request=%d summary_reqs=%d elide_reqs=%d wall=%s usage=%+v",
		res.Turns, res.StopReason, len(reqs), len(shellLog), shellFails, writer.count,
		goalNudges, firstCompacted, summaries, elides, wall.Round(time.Second), res.Usage)

	// --- Goal gate: the run must close with an explicit status line. ---
	upFinal := strings.ToUpper(res.Final)
	if !strings.Contains(upFinal, goal.Done) {
		if strings.Contains(upFinal, goal.Blocked) {
			t.Errorf("agent declared STATUS: BLOCKED: %q", ttsTruncateMiddle(res.Final, 400))
		} else {
			t.Errorf("final reply carries no goal status line: %q", ttsTruncateMiddle(res.Final, 400))
		}
	}

	// --- Deliverables exist. ---
	gamePath := filepath.Join(ws, "game.html")
	gameData, err := os.ReadFile(gamePath)
	if err != nil {
		t.Fatalf("game.html missing: %v", err)
	}
	if len(gameData) > 150_000 {
		t.Errorf("game.html is %d bytes, want < 150000", len(gameData))
	}
	if len(gameData) < 8_000 {
		t.Errorf("game.html is only %d bytes — implausibly small for the spec", len(gameData))
	}
	for _, f := range []string{"verify.mjs", "README.md"} {
		st, serr := os.Stat(filepath.Join(ws, f))
		if serr != nil {
			t.Errorf("required file missing: %s", f)
		} else if f == "README.md" && st.Size() < 500 {
			t.Errorf("README.md is only %d bytes — not a real writeup", st.Size())
		}
	}

	// --- The agent's own verifier must pass when the HARNESS runs it. ---
	vctx, vcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer vcancel()
	vcmd := exec.CommandContext(vctx, "node", "verify.mjs")
	vcmd.Dir = ws
	vout, verr := vcmd.CombinedOutput()
	t.Logf("harness re-run of verify.mjs:\n%s", ttsTruncateMiddle(string(vout), 2000))
	if verr != nil {
		t.Errorf("node verify.mjs failed under the harness: %v", verr)
	}

	// --- Independent static checks (verify.mjs is not trusted). ---
	game := strings.ToLower(string(gameData))
	for _, want := range []string{
		"<canvas", "requestanimationframe", "image-rendering", "pixelated",
		"localstorage", "visibilitychange",
		"#ef4444", "#0a0a0a", "#f5f0e8", "#141414", "#8a857c", "0.75rem",
		"đột phá", // breakthrough action, Vietnamese UI
	} {
		if !strings.Contains(game, want) {
			t.Errorf("game.html static check failed: missing %q", want)
		}
	}
	if !strings.Contains(game, "luyện khí") && !strings.Contains(game, "trúc cơ") {
		t.Errorf("game.html names no cultivation realm (want luyện khí / trúc cơ ladder)")
	}
	for _, banned := range []string{
		"<script src", "<link rel=\"stylesheet\" href", "url(http", "src=\"http", "href=\"http",
		"fetch(", "xmlhttprequest", "websocket", "new audio", "audiocontext", "<audio",
		"@import",
	} {
		if strings.Contains(game, banned) {
			t.Errorf("game.html static check failed: contains banned %q", banned)
		}
	}

	// --- Copy artifacts out + persist the run record. ---
	outDir := "results/game"
	for _, f := range []string{"game.html", "verify.mjs", "README.md"} {
		if cerr := ttsCopyFile(filepath.Join(ws, f), filepath.Join(outDir, f)); cerr != nil {
			t.Logf("artifact copy %s: %v", f, cerr)
		}
	}
	if derr := obs.dump(outDir); derr != nil {
		t.Logf("observer dump: %v", derr)
	}
	t.Logf("observed: %+v", obs.summarize())
	if slData, jerr := json.MarshalIndent(shellLog, "", "  "); jerr == nil {
		if err := os.MkdirAll(outDir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(outDir, "shell-log.json"), slData, 0o644)
		}
	}
	record := map[string]any{
		"solver":                  "agentcore",
		"model":                   model,
		"date":                    time.Now().Format("2006-01-02"),
		"problem":                 "PROBLEM4.md (pixel active-idle web game for audiobook listeners, goal-gated)",
		"workspace":               ws,
		"turns":                   res.Turns,
		"stop_reason":             res.StopReason,
		"requests":                len(reqs),
		"shell_calls":             len(shellLog),
		"shell_nonzero_exits":     shellFails,
		"file_writes":             writer.count,
		"goal_nudges":             goalNudges,
		"first_compacted_request": firstCompacted,
		"summary_bearing_reqs":    summaries,
		"elide_bearing_reqs":      elides,
		"game_bytes":              len(gameData),
		"harness_verify_pass":     verr == nil,
		"final":                   ttsTruncateMiddle(res.Final, 2000),
		"wall_seconds":            wall.Seconds(),
		"usage":                   res.Usage,
	}
	if data, jerr := json.MarshalIndent(record, "", "  "); jerr == nil {
		if werr := os.WriteFile(filepath.Join(outDir, "agentcore-game.json"), data, 0o644); werr != nil {
			t.Logf("could not save run record: %v", werr)
		}
	}
}
