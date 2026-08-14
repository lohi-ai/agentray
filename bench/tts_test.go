package bench_test

// Live benchmark #3 (hard): an agentcore agent builds a lite CPU-realtime
// Vietnamese TTS pipeline (PROBLEM3.md) — real training pairs pulled from the
// kiem-lai production DB + GCS bucket (voice thuy-trang), a smoke training run
// that must demonstrably learn, CPU inference, and a measured real-time
// factor < 1.0. Unlike Problems 1–2 the agent gets a full local toolset:
// run_shell (bash in a scratch workspace, with DATABASE_URL exported into the
// tool env only — never into the prompt), write_file, read_file, list_dir.
//
// The test's job is MONITORING: record every provider request, every shell
// command, compaction behavior, and verify the agent's report against real
// files on disk (WAVs exist, scripts exist, numbers parse). Artifacts are
// copied to results/tts/ and the run record to results/tts/agentcore-tts.json.
//
// Same gate as the other benchmarks: AGENTRAY_TEST_OPENAI_{BASE_URL,API_KEY,MODEL}.
//
// Run: go test -timeout 120m -count=1 -v -run TestBench_AgentcoreLiteTTS ./bench/

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

// ---------- workspace path safety ----------

func ttsResolve(root, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is required")
	}
	clean := p
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(root, clean)
	}
	clean = filepath.Clean(clean)
	if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	return clean, nil
}

func ttsTruncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + fmt.Sprintf("\n… [%d bytes truncated] …\n", len(s)-max) + s[len(s)-half:]
}

// ---------- run_shell ----------

type ttsShellEntry struct {
	Command string  `json:"command"`
	Exit    int     `json:"exit"`
	Seconds float64 `json:"seconds"`
}

type ttsShellTool struct {
	root string
	env  []string
	mu   sync.Mutex
	log  []ttsShellEntry
}

func (s *ttsShellTool) Name() string { return "run_shell" }
func (s *ttsShellTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: "run_shell",
		Description: "Run a bash command in the workspace (cwd = workspace root). Returns combined " +
			"stdout+stderr (long output is middle-truncated) and the exit code when non-zero. " +
			"For anything long, write a script with write_file and run it.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "the bash command line"},
				"timeout_seconds": map[string]any{"type": "integer",
					"description": "kill the command after this many seconds (default 300, max 900)"},
			},
			"required": []any{"command"},
		},
	}
}
func (s *ttsShellTool) Run(ctx context.Context, args string) (string, error) {
	var in struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	timeout := 300 * time.Second
	if in.TimeoutSeconds > 0 {
		if in.TimeoutSeconds > 900 {
			in.TimeoutSeconds = 900
		}
		if in.TimeoutSeconds < 5 {
			in.TimeoutSeconds = 5
		}
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "bash", "-lc", in.Command)
	cmd.Dir = s.root
	cmd.Env = s.env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	s.mu.Lock()
	s.log = append(s.log, ttsShellEntry{
		Command: ttsTruncateMiddle(in.Command, 300), Exit: exitCode, Seconds: elapsed.Seconds(),
	})
	s.mu.Unlock()

	text := ttsTruncateMiddle(string(out), 12_000)
	if cctx.Err() == context.DeadlineExceeded {
		text += fmt.Sprintf("\n[command timed out after %s]", timeout)
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok && cctx.Err() != context.DeadlineExceeded {
			return "", err // could not start at all
		}
		return fmt.Sprintf("exit_code=%d\n%s", exitCode, text), nil
	}
	if text == "" {
		text = "(no output)"
	}
	return text, nil
}
func (s *ttsShellTool) snapshot() []ttsShellEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ttsShellEntry(nil), s.log...)
}

// ---------- write_file / read_file / list_dir ----------

type ttsWriteTool struct {
	root  string
	mu    sync.Mutex
	count int
}

func (w *ttsWriteTool) Name() string { return "write_file" }
func (w *ttsWriteTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        "write_file",
		Description: "Write a file (relative to the workspace root), creating parent directories.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []any{"path", "content"},
		},
	}
}
func (w *ttsWriteTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	abs, err := ttsResolve(w.root, in.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(in.Content), 0o755); err != nil {
		return "", err
	}
	w.mu.Lock()
	w.count++
	w.mu.Unlock()
	return fmt.Sprintf("wrote %s (%d bytes)", in.Path, len(in.Content)), nil
}

type ttsReadTool struct{ root string }

func (r *ttsReadTool) Name() string { return "read_file" }
func (r *ttsReadTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        "read_file",
		Description: "Read a file from the workspace. Large files are windowed via offset_bytes/max_bytes.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":         map[string]any{"type": "string"},
				"offset_bytes": map[string]any{"type": "integer", "description": "start offset (default 0)"},
				"max_bytes":    map[string]any{"type": "integer", "description": "max bytes to return (default 8000, max 20000)"},
			},
			"required": []any{"path"},
		},
	}
}
func (r *ttsReadTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Path        string `json:"path"`
		OffsetBytes int64  `json:"offset_bytes"`
		MaxBytes    int    `json:"max_bytes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	abs, err := ttsResolve(r.root, in.Path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if in.MaxBytes <= 0 {
		in.MaxBytes = 8000
	}
	if in.MaxBytes > 20000 {
		in.MaxBytes = 20000
	}
	if in.OffsetBytes > 0 {
		if _, err := f.Seek(in.OffsetBytes, io.SeekStart); err != nil {
			return "", err
		}
	}
	buf := make([]byte, in.MaxBytes)
	n, _ := io.ReadFull(f, buf)
	out := string(buf[:n])
	if in.OffsetBytes+int64(n) < st.Size() {
		out += fmt.Sprintf("\n[%d of %d bytes shown; continue with offset_bytes=%d]",
			n, st.Size(), in.OffsetBytes+int64(n))
	}
	return out, nil
}

type ttsListTool struct{ root string }

func (l *ttsListTool) Name() string { return "list_dir" }
func (l *ttsListTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        "list_dir",
		Description: "List a workspace directory (name, size; directories end with /).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "directory (default: workspace root)"},
			},
		},
	}
}
func (l *ttsListTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if args != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return "", err
		}
	}
	if in.Path == "" {
		in.Path = "."
	}
	abs, err := ttsResolve(l.root, in.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, e := range entries {
		if i >= 200 {
			fmt.Fprintf(&b, "… (%d more entries)\n", len(entries)-i)
			break
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			fmt.Fprintf(&b, "%s\n", e.Name())
			continue
		}
		fmt.Fprintf(&b, "%s\t%d\n", e.Name(), info.Size())
	}
	if b.Len() == 0 {
		return "(empty)", nil
	}
	return b.String(), nil
}

// ---------- helpers ----------

// ttsEnvValue extracts KEY=VALUE from a dotenv-style file without ever
// logging the value. Empty string when the file or key is missing.
func ttsEnvValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func ttsCopyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// ---------- the benchmark ----------

func TestBench_AgentcoreLiteTTS(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("set AGENTRAY_TEST_OPENAI_BASE_URL, AGENTRAY_TEST_OPENAI_API_KEY, AGENTRAY_TEST_OPENAI_MODEL to run the live benchmark")
	}
	rec := &probeRecorder{inner: ai.NewOpenAIProvider(key, base, ai.DefaultCompat())}

	problem, err := os.ReadFile("PROBLEM3.md")
	if err != nil {
		t.Fatalf("read PROBLEM3.md: %v", err)
	}

	// Fresh workspace per run (override with AGENTRAY_BENCH_TTS_WORKSPACE to
	// keep a previous one for debugging — it is then NOT wiped).
	ws := strings.TrimSpace(os.Getenv("AGENTRAY_BENCH_TTS_WORKSPACE"))
	if ws == "" {
		ws = filepath.Join(os.TempDir(), "agentray-bench-tts")
		if err := os.RemoveAll(ws); err != nil {
			t.Fatalf("clean workspace: %v", err)
		}
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mk workspace: %v", err)
	}
	ws, err = filepath.EvalSymlinks(ws) // macOS /var/folders vs /private/var
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	t.Logf("workspace: %s", ws)

	// Secrets go into the SHELL ENV only, never the prompt or the transcript.
	shellEnv := os.Environ()
	dbURL := ttsEnvValue("../../api/.env", "DATABASE_URL")
	if dbURL == "" {
		t.Log("WARNING: DATABASE_URL not found in ../../api/.env — the agent will have to use the fallback data path")
	} else {
		shellEnv = append(shellEnv, "DATABASE_URL="+dbURL)
	}

	shell := &ttsShellTool{root: ws, env: shellEnv}
	writer := &ttsWriteTool{root: ws}
	reader := &ttsReadTool{root: ws}
	lister := &ttsListTool{root: ws}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 80
	limits.MaxToolCalls = 200
	// Context budget stays at the production default (200k) — this bench
	// observes whether the long build triggers compaction, it does not force it.

	agent, err := agentcore.New(agentcore.Config{
		Provider:             rec,
		Model:                model,
		Limits:               &limits,
		MaxTokens:            16000,
		PromptCacheKey:       "bench-tts",
		PromptCacheRetention: "short",
		Tools:                agentcore.NewToolSet(shell, writer, reader, lister),
		Policy:               agentcore.NewAllowList("run_shell", "write_file", "read_file", "list_dir"),
		Definition: agentcore.AgentDefinition{
			Agents: "You are a senior ML/audio engineer working alone in a shell workspace. Follow the " +
				"task stages in order and verify each stage actually ran before moving on. Prefer " +
				"write_file for scripts and run_shell to execute them; keep individual commands under " +
				"their timeout by batching work into scripts. Keep your text replies to one or two " +
				"short sentences — the work product is files, not prose. NEVER print secret values " +
				"(e.g. echo $DATABASE_URL is forbidden; pass it to tools as \"$DATABASE_URL\"). Every " +
				"number you report must come from a command you actually ran. When report.json is " +
				"written and every stage is done, reply with exactly: SHIPPED.",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := agent.Prompt(ctx, string(problem))
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	reqs := rec.requests()
	shellLog := shell.snapshot()

	// --- Harness monitoring: compaction + tool behavior over the long run. ---
	firstCompacted, summaries, elides := -1, 0, 0
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
	shellFails := 0
	for _, e := range shellLog {
		if e.Exit != 0 {
			shellFails++
		}
	}
	t.Logf("behavior: turns=%d stop=%q requests=%d shell_calls=%d (nonzero_exit=%d) file_writes=%d "+
		"first_compacted_request=%d summary_reqs=%d elide_reqs=%d wall=%s usage=%+v",
		res.Turns, res.StopReason, len(reqs), len(shellLog), shellFails, writer.count,
		firstCompacted, summaries, elides, wall.Round(time.Second), res.Usage)

	if !strings.Contains(res.Final, "SHIPPED") {
		t.Errorf("final reply does not contain SHIPPED: %q", ttsTruncateMiddle(res.Final, 400))
	}

	// --- The report must exist, parse, and hold honest-looking numbers. ---
	repPath := filepath.Join(ws, "report.json")
	repData, err := os.ReadFile(repPath)
	if err != nil {
		t.Fatalf("report.json missing: %v", err)
	}
	var rep struct {
		DataSource   string   `json:"data_source"`
		Pairs        int      `json:"pairs"`
		Architecture string   `json:"architecture"`
		Params       int64    `json:"params"`
		SampleRate   int      `json:"sample_rate"`
		TrainSteps   int      `json:"train_steps"`
		LossStart    float64  `json:"loss_start"`
		LossEnd      float64  `json:"loss_end"`
		RTF          float64  `json:"rtf"`
		RTFSentences int      `json:"rtf_sentences"`
		WavFiles     []string `json:"wav_files"`
	}
	if err := json.Unmarshal(repData, &rep); err != nil {
		t.Fatalf("report.json does not parse: %v", err)
	}
	t.Logf("report: source=%s pairs=%d arch=%q params=%d sr=%d steps=%d loss %.4f→%.4f rtf=%.3f (%d sentences) wavs=%d",
		rep.DataSource, rep.Pairs, rep.Architecture, rep.Params, rep.SampleRate,
		rep.TrainSteps, rep.LossStart, rep.LossEnd, rep.RTF, rep.RTFSentences, len(rep.WavFiles))

	if rep.DataSource == "fallback-local-synth" {
		t.Log("DEGRADED: agent used the local-synth fallback instead of production thuy-trang data")
	}
	if rep.Pairs < 10 {
		t.Errorf("pairs = %d, want >= 10", rep.Pairs)
	}
	if rep.TrainSteps < 50 {
		t.Errorf("train_steps = %d, want >= 50", rep.TrainSteps)
	}
	if !(rep.LossEnd < rep.LossStart) {
		t.Errorf("loss did not decrease: start=%v end=%v", rep.LossStart, rep.LossEnd)
	}
	if !(rep.RTF > 0) {
		t.Errorf("rtf = %v, want > 0 (measured)", rep.RTF)
	}
	if rep.RTF >= 1.0 {
		t.Errorf("rtf = %v — NOT real-time on CPU (want < 1.0)", rep.RTF)
	}
	if rep.RTFSentences < 3 {
		t.Errorf("rtf_sentences = %d, want >= 3", rep.RTFSentences)
	}

	// --- The WAVs must actually exist on disk and be real audio files. ---
	if len(rep.WavFiles) < 3 {
		t.Errorf("report lists %d wav files, want >= 3", len(rep.WavFiles))
	}
	bigWav := false
	for _, wf := range rep.WavFiles {
		abs, rerr := ttsResolve(ws, wf)
		if rerr != nil {
			t.Errorf("wav path %q: %v", wf, rerr)
			continue
		}
		data, rerr := os.ReadFile(abs)
		if rerr != nil {
			t.Errorf("reported wav %q does not exist: %v", wf, rerr)
			continue
		}
		if len(data) < 44 || string(data[:4]) != "RIFF" {
			t.Errorf("reported wav %q is not a RIFF/WAV file (%d bytes)", wf, len(data))
			continue
		}
		if len(data) > 20_000 {
			bigWav = true
		}
	}
	if !bigWav {
		t.Errorf("no reported wav exceeds 20 KB — implausible for real synthesized sentences")
	}

	// --- The pipeline files the stages demand. ---
	for _, f := range []string{"DESIGN.md", "REPORT.md", "prepare_data.py", "train.py", "infer.py", "bench.py"} {
		st, serr := os.Stat(filepath.Join(ws, f))
		if serr != nil {
			t.Errorf("required file missing: %s", f)
			continue
		}
		if f == "DESIGN.md" && st.Size() < 800 {
			t.Errorf("DESIGN.md is only %d bytes — not a real design doc", st.Size())
		}
	}

	// --- Copy artifacts out + persist the run record. ---
	outDir := "results/tts"
	for _, f := range []string{"report.json", "REPORT.md", "DESIGN.md", "prepare_data.py", "train.py", "infer.py", "bench.py"} {
		if cerr := ttsCopyFile(filepath.Join(ws, f), filepath.Join(outDir, f)); cerr != nil {
			t.Logf("artifact copy %s: %v", f, cerr)
		}
	}
	for i, wf := range rep.WavFiles {
		if i >= 3 {
			break
		}
		if abs, rerr := ttsResolve(ws, wf); rerr == nil {
			if cerr := ttsCopyFile(abs, filepath.Join(outDir, "samples", filepath.Base(wf))); cerr != nil {
				t.Logf("wav copy %s: %v", wf, cerr)
			}
		}
	}
	if slData, jerr := json.MarshalIndent(shellLog, "", "  "); jerr == nil {
		if err := os.MkdirAll(outDir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(outDir, "shell-log.json"), slData, 0o644)
		}
	}
	record := map[string]any{
		"solver":                  "agentcore",
		"model":                   model,
		"date":                    time.Now().Format("2006-01-02"),
		"problem":                 "PROBLEM3.md (lite CPU-realtime Vietnamese TTS, voice thuy-trang)",
		"workspace":               ws,
		"turns":                   res.Turns,
		"stop_reason":             res.StopReason,
		"requests":                len(reqs),
		"shell_calls":             len(shellLog),
		"shell_nonzero_exits":     shellFails,
		"file_writes":             writer.count,
		"first_compacted_request": firstCompacted,
		"summary_bearing_reqs":    summaries,
		"elide_bearing_reqs":      elides,
		"final":                   ttsTruncateMiddle(res.Final, 2000),
		"wall_seconds":            wall.Seconds(),
		"usage":                   res.Usage,
		"report":                  json.RawMessage(repData),
	}
	if data, jerr := json.MarshalIndent(record, "", "  "); jerr == nil {
		if werr := os.WriteFile(filepath.Join(outDir, "agentcore-tts.json"), data, 0o644); werr != nil {
			t.Logf("could not save run record: %v", werr)
		}
	}
}
