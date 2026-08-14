package bench_test

// Live benchmark #5: an agentcore agent works INSIDE the real kiem-lai web/
// Next.js codebase (PROBLEM5.md) — "Tu Tiên Du Ký v2", an endless
// side-scrolling chill pixel world integrated with the platform: the global
// audio-player store (nghe đạo bonus), the hoá thân active persona, the
// shared REALM_NAMES ladder, module/route conventions, and @lohi-ui chrome.
//
// Unlike Problems 1-4 the workspace is NOT scratch: tools are rooted at
// ../../web and the harness enforces a SCOPE FENCE via git — the tree must be
// clean before the run, and afterwards every changed path must sit inside
// modules/tu-tien/, app/tu-tien/, or the single allowed ProfileContent.tsx
// tile edit. The goal gate holds the finish until lint + tsc are green; the
// harness re-runs both itself and leaves all changes uncommitted for review.
//
// Gate: AGENTRAY_TEST_OPENAI_{BASE_URL,API_KEY,MODEL}.
//
// Run: go test -timeout 90m -count=1 -v -run TestBench_AgentcoreTuTienWeb ./bench/

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
	"github.com/lohi-ai/agentray/agentcore/plugins/preset"
	"github.com/lohi-ai/agentray/ai"
)

// tuTienAllowedPath reports whether a git-reported path (relative to the repo
// root, e.g. "web/modules/tu-tien/page.tsx") is inside the scope fence.
func tuTienAllowedPath(p string) bool {
	p = strings.TrimSpace(strings.ReplaceAll(p, `"`, ""))
	return strings.Contains(p, "modules/tu-tien/") ||
		strings.Contains(p, "app/tu-tien/") ||
		strings.HasSuffix(p, "modules/profile/ProfileContent.tsx")
}

func tuTienGit(t *testing.T, ws string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestBench_AgentcoreTuTienWeb(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("set AGENTRAY_TEST_OPENAI_BASE_URL, AGENTRAY_TEST_OPENAI_API_KEY, AGENTRAY_TEST_OPENAI_MODEL to run the live benchmark")
	}
	rec := &probeRecorder{inner: ai.NewOpenAIProvider(key, base, ai.DefaultCompat())}

	problem, err := os.ReadFile("PROBLEM5.md")
	if err != nil {
		t.Fatalf("read PROBLEM5.md: %v", err)
	}

	ws, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatalf("resolve web/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "package.json")); err != nil {
		t.Fatalf("web/ workspace not found at %s: %v", ws, err)
	}
	t.Logf("workspace: %s (real repo tree — never wiped)", ws)

	// The tree must be clean so the post-run diff is exactly the agent's work.
	if dirty := strings.TrimSpace(tuTienGit(t, ws, "status", "--porcelain", ".")); dirty != "" {
		t.Fatalf("web/ working tree is dirty — commit or stash before the bench:\n%s", dirty)
	}
	headBefore := strings.TrimSpace(tuTienGit(t, ws, "rev-parse", "HEAD"))

	shell := &ttsShellTool{root: ws, env: os.Environ()}
	writer := &ttsWriteTool{root: ws}
	reader := &ttsReadTool{root: ws}
	lister := &ttsListTool{root: ws}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 100
	limits.MaxToolCalls = 250

	agent, err := preset.New(agentcore.Config{
		Provider:             rec,
		Model:                model,
		Limits:               &limits,
		MaxTokens:            16000,
		PromptCacheKey:       "bench-tutien",
		PromptCacheRetention: "short",
		Tools:                agentcore.NewToolSet(shell, writer, reader, lister),
		Policy:               agentcore.NewAllowList("run_shell", "write_file", "read_file", "list_dir"),
		Goal: "The tu-tien feature is complete per the task: modules/tu-tien/ (with README.md), " +
			"app/tu-tien/page.tsx, and the single ProfileContent.tsx tile exist; `pnpm run lint` and " +
			"`pnpm exec tsc --noEmit` have both been run via run_shell and exited 0; no files outside " +
			"the scope fence were touched; nothing was committed.",
		Definition: agentcore.AgentDefinition{
			Agents: "You are a senior frontend engineer working alone inside a production Next.js " +
				"codebase. Read neighboring modules before writing — match their conventions exactly. " +
				"Stay strictly inside the task's scope fence and never run git commit/push. Long " +
				"commands (lint, tsc) may need timeout_seconds up to 900. Keep text replies to one or " +
				"two short sentences — the work product is code.",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := agent.Prompt(ctx, string(problem))
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	reqs := rec.requests()
	shellLog := shell.snapshot()

	// --- Monitoring: goal nudges (distinct in final request) + compaction. ---
	firstCompacted, summaries, elides, goalNudges := -1, 0, 0, 0
	for i, req := range reqs {
		for _, m := range req.Messages {
			if strings.HasPrefix(m.Content, summaryMarkerProbe) || strings.HasPrefix(m.Content, elideMarkerProbe) {
				if strings.HasPrefix(m.Content, summaryMarkerProbe) {
					summaries++
				} else {
					elides++
				}
				if firstCompacted < 0 {
					firstCompacted = i
				}
				break
			}
		}
	}
	if len(reqs) > 0 {
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

	// --- Goal gate closed properly. ---
	upFinal := strings.ToUpper(res.Final)
	if !strings.Contains(upFinal, goal.Done) {
		if strings.Contains(upFinal, goal.Blocked) {
			t.Errorf("agent declared STATUS: BLOCKED: %q", ttsTruncateMiddle(res.Final, 400))
		} else {
			t.Errorf("final reply carries no goal status line: %q", ttsTruncateMiddle(res.Final, 400))
		}
	}

	// --- Scope fence: nothing committed, every change inside the fence. ---
	if headAfter := strings.TrimSpace(tuTienGit(t, ws, "rev-parse", "HEAD")); headAfter != headBefore {
		t.Errorf("agent moved HEAD (%s -> %s) — commits are forbidden", headBefore[:12], headAfter[:12])
	}
	// NOTE: do not TrimSpace the whole blob — porcelain lines are
	// column-positional ("XY path") and a global trim eats the first line's
	// leading status char, shifting the path slice.
	status := strings.TrimRight(tuTienGit(t, ws, "status", "--porcelain", "."), "\n")
	if strings.TrimSpace(status) == "" {
		t.Fatalf("no working-tree changes in web/ — the agent produced nothing")
	}
	var changed, violations []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[2:])
		changed = append(changed, p)
		if !tuTienAllowedPath(p) {
			violations = append(violations, line)
		}
	}
	t.Logf("changed files (%d):\n%s", len(changed), status)
	for _, v := range violations {
		t.Errorf("scope-fence violation: %s", v)
	}

	// --- Required files. ---
	moduleDir := filepath.Join(ws, "modules", "tu-tien")
	for _, f := range []string{
		filepath.Join(moduleDir, "page.tsx"),
		filepath.Join(moduleDir, "index.ts"),
		filepath.Join(moduleDir, "README.md"),
		filepath.Join(ws, "app", "tu-tien", "page.tsx"),
	} {
		if _, serr := os.Stat(f); serr != nil {
			t.Errorf("required file missing: %s", f)
		}
	}
	if data, rerr := os.ReadFile(filepath.Join(ws, "app", "tu-tien", "page.tsx")); rerr == nil {
		if !strings.Contains(string(data), "@/modules/tu-tien") {
			t.Errorf("app/tu-tien/page.tsx does not re-export from @/modules/tu-tien")
		}
	}
	if data, rerr := os.ReadFile(filepath.Join(ws, "modules", "profile", "ProfileContent.tsx")); rerr == nil {
		if !strings.Contains(string(data), "/tu-tien") {
			t.Errorf("ProfileContent.tsx has no entry link to /tu-tien")
		}
	}

	// --- Independent static checks over the module source. ---
	var moduleSrc strings.Builder
	_ = filepath.Walk(moduleDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") || strings.HasSuffix(path, ".css") {
			if data, rerr := os.ReadFile(path); rerr == nil {
				moduleSrc.Write(data)
				moduleSrc.WriteString("\n")
			}
		}
		return nil
	})
	src := strings.ToLower(moduleSrc.String())
	if len(src) < 8_000 {
		t.Errorf("modules/tu-tien source is only %d bytes — implausibly small", len(src))
	}
	for _, want := range []string{
		"useaudioplayer", "getactivepersona", "realm_names", "useauth",
		"<canvas", "requestanimationframe", "pixelated", "localstorage", "visibilitychange",
		"đạo hữu vô danh",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("modules/tu-tien static check failed: missing %q", want)
		}
	}
	for _, banned := range []string{"new audio", "audiocontext", "<audio", "fetch(\"http", "fetch('http"} {
		if strings.Contains(src, banned) {
			t.Errorf("modules/tu-tien static check failed: contains banned %q", banned)
		}
	}

	// --- Harness re-runs the verification gates itself. ---
	runCheck := func(name string, args ...string) bool {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer ccancel()
		cmd := exec.CommandContext(cctx, args[0], args[1:]...)
		cmd.Dir = ws
		out, cerr := cmd.CombinedOutput()
		if cerr != nil {
			t.Errorf("%s failed under the harness: %v\n%s", name, cerr, ttsTruncateMiddle(string(out), 3000))
			return false
		}
		t.Logf("%s: OK", name)
		return true
	}
	lintOK := runCheck("pnpm run lint", "pnpm", "run", "lint")
	tscOK := runCheck("pnpm exec tsc --noEmit", "pnpm", "exec", "tsc", "--noEmit")

	// --- Persist the run record (repo changes stay uncommitted for review). ---
	outDir := "results/game2"
	diffStat := tuTienGit(t, ws, "diff", "--stat", ".")
	record := map[string]any{
		"solver":                  "agentcore",
		"model":                   model,
		"date":                    time.Now().Format("2006-01-02"),
		"problem":                 "PROBLEM5.md (Tu Tiên Du Ký v2 — endless chill pixel world inside web/, goal-gated)",
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
		"changed_files":           changed,
		"scope_violations":        len(violations),
		"harness_lint_pass":       lintOK,
		"harness_tsc_pass":        tscOK,
		"diff_stat":               diffStat,
		"final":                   ttsTruncateMiddle(res.Final, 2000),
		"wall_seconds":            wall.Seconds(),
		"usage":                   res.Usage,
	}
	if err := os.MkdirAll(outDir, 0o755); err == nil {
		if data, jerr := json.MarshalIndent(record, "", "  "); jerr == nil {
			if werr := os.WriteFile(filepath.Join(outDir, "agentcore-tutien.json"), data, 0o644); werr != nil {
				t.Logf("could not save run record: %v", werr)
			}
		}
		if slData, jerr := json.MarshalIndent(shellLog, "", "  "); jerr == nil {
			_ = os.WriteFile(filepath.Join(outDir, "shell-log.json"), slData, 0o644)
		}
	}
}
