package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// LabService backs the AgentCore Lab (test + explain modes). It is a thin
// consumer over the same Runner the chat/scheduler paths use, so a Lab run goes
// through the identical build → loop → trace path as production — observing a run
// never changes how it behaves (the Lab's compatibility constraint). The Lab's
// only run-shaping input is the StepGate it threads in for explain mode, which
// merely pauses between turns; gating, secret resolution, budgets, and escalation
// are untouched.
//
// Every step view the Lab renders — live (explain) or replayed (historical) — is
// produced by agentcore.FoldSteps over the run's persisted agent_llm_calls trace,
// so a run reads identically both ways (AC4) and secrets never surface (the trace
// holds {{cred:NAME}} placeholders, AC2).
type LabService struct {
	store  *storage.Store
	runner *Runner
	// sandboxReady reports whether the host built an isolation sandbox. Test mode
	// requires it (tools run for real and must be confined); when false the Lab
	// fails test runs closed with a setup prompt rather than running on the host.
	sandboxReady bool

	mu       sync.Mutex
	sessions map[string]*explainSession // keyed by run id
}

// NewLabService wires a LabService over the storage layer, mirroring
// NewChatService: it builds one Runner from the host's RunnerOptions so a Lab run
// shares the same sandbox, credential vault, HTTP tool, and trace sink as every
// other run. sandboxReady tells the Lab whether test mode may execute.
func NewLabService(store *storage.Store, sandboxReady bool, runnerOpts ...RunnerOption) *LabService {
	return &LabService{
		store:        store,
		runner:       NewRunner(store, runnerOpts...),
		sandboxReady: sandboxReady,
		sessions:     map[string]*explainSession{},
	}
}

// LabTestResult is the verdict of a test-mode run: the produced output compared
// to the expected output (exact match first, then an LLM rubric judge when the
// expected text reads as criteria rather than a literal answer), plus the full
// folded step list so the same per-step inspector renders for a test run.
type LabTestResult struct {
	RunID       string              `json:"run_id"`
	Status      string              `json:"status"` // pass | fail | error | blocked
	Expected    string              `json:"expected"`
	Actual      string              `json:"actual"`
	// Verdict reports how Status was decided: "exact" (string-equal), "judge"
	// (LLM rubric), or "" (blocked/error). Rationale is the judge's one-line
	// reason when Verdict == "judge".
	Verdict     string              `json:"verdict,omitempty"`
	Rationale   string              `json:"rationale,omitempty"`
	Diff        string              `json:"diff,omitempty"`
	Steps       []agentcore.LabStep `json:"steps"`
	SetupPrompt string              `json:"setup_prompt,omitempty"` // when blocked
}

// RunTest executes an agent against an input to completion and compares the final
// output to expected (exact match — fuzzy match is deferred; a builder may still
// override the verdict manually). Tools run for real through the runner's
// sandbox; when no sandbox is configured the run fails closed with a setup prompt
// instead of executing on the host (AC5).
func (s *LabService) RunTest(ctx context.Context, userID, projectID, agentID, input, expected string) (LabTestResult, error) {
	if !s.sandboxReady {
		return LabTestResult{
			Status:   "blocked",
			Expected: expected,
			SetupPrompt: "Test mode runs the agent's tools for real inside an isolated sandbox, " +
				"which isn't configured on this server. Enable the sandbox backend (Docker) to run test cases.",
		}, nil
	}

	run, res, err := s.runner.Run(ctx, RunOptions{
		ProjectID: projectID,
		AgentID:   agentID,
		Trigger:   "manual",
		Prompt:    input,
	})
	if err != nil {
		// A run that never produced a trace still yields a verdict shape so the UI
		// shows the failure as the outcome rather than a dead end.
		return LabTestResult{RunID: run.ID, Status: "error", Expected: expected, Actual: err.Error()}, nil
	}

	steps, _ := s.stepsForRun(ctx, userID, projectID, run.ID)
	actual := strings.TrimSpace(res.Final)
	want := strings.TrimSpace(expected)
	status := "fail"
	verdict := "exact"
	rationale := ""
	diff := ""
	switch {
	case actual == want:
		// Authoritative fast path: a literal expected answer matched verbatim.
		status = "pass"
	case want == "":
		// No criteria to judge against — keep the deterministic fail.
		diff = lineDiff(want, actual)
	default:
		// The EXPECTED field is criteria ("a correct answer mentions…"), so an
		// exact-match miss isn't a real failure for open-ended/generative work
		// (marketing copy, support replies, growth plans). Ask a cheap rubric
		// judge whether the output satisfies the criteria. On any judge error we
		// degrade to the deterministic diff fail — a test is never broken by the
		// judge being unavailable (mirrors the compaction degrade philosophy).
		pass, reason, jerr := s.judge(ctx, projectID, want, actual)
		if jerr != nil {
			diff = lineDiff(want, actual)
		} else {
			verdict = "judge"
			rationale = reason
			if pass {
				status = "pass"
			} else {
				diff = lineDiff(want, actual)
			}
		}
	}
	return LabTestResult{
		RunID: run.ID, Status: status, Verdict: verdict, Rationale: rationale,
		Expected: expected, Actual: res.Final, Diff: diff, Steps: steps,
	}, nil
}

// judge asks a cheap model whether actual satisfies the criteria described in
// want. It returns pass/fail plus a one-line rationale. The judge is deliberately
// strict-but-fair: it grades against the stated criteria only, not its own taste,
// so the verdict stays reproducible. Any provider/parse failure is returned as an
// error so RunTest can degrade to the deterministic diff rather than guess.
func (s *LabService) judge(ctx context.Context, projectID, want, actual string) (bool, string, error) {
	prov, model, err := s.runner.CheapProvider(ctx, projectID)
	if err != nil {
		return false, "", err
	}
	resp, err := prov.Chat(ctx, agentcore.ChatRequest{
		Model:     model,
		MaxTokens: 256,
		Messages: []agentcore.Message{
			{Role: agentcore.RoleSystem, Content: labJudgeSystemPrompt},
			{Role: agentcore.RoleUser, Content: "CRITERIA (what a correct answer must satisfy):\n" + want +
				"\n\nAGENT OUTPUT:\n" + actual +
				"\n\nReply with exactly one line: PASS or FAIL, then a dash and a brief reason."},
		},
	})
	if err != nil {
		return false, "", err
	}
	return parseJudgeLine(resp.Message.Content)
}

// parseJudgeLine extracts pass/fail + rationale from the judge's reply. The judge
// is told to answer "PASS - reason" / "FAIL - reason" on one line; we take the
// first non-empty line and read its leading verdict token. An empty or
// unrecognized verdict is an error so RunTest degrades to the deterministic diff
// rather than guessing from noise.
func parseJudgeLine(content string) (bool, string, error) {
	line := strings.TrimSpace(content)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return false, "", errors.New("empty judge response")
	}
	upper := strings.ToUpper(line)
	switch {
	case strings.HasPrefix(upper, "PASS"):
		return true, trimVerdictPrefix(line, "PASS"), nil
	case strings.HasPrefix(upper, "FAIL"):
		return false, trimVerdictPrefix(line, "FAIL"), nil
	default:
		return false, "", errors.New("unparseable judge verdict: " + line)
	}
}

// trimVerdictPrefix strips the leading verdict token and any joining punctuation
// ("- ", ": ", "— ") to leave just the rationale.
func trimVerdictPrefix(line, token string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(line, token), " -—:"))
}

const labJudgeSystemPrompt = `You are a strict but fair test grader for an AI agent's output.
Decide ONLY whether the agent output satisfies the stated criteria — grade against the criteria, not your own preferences or style.
The criteria describe what a correct answer should contain; an answer can be worded differently and still pass if it meets them.
Respond with a single line beginning with PASS or FAIL, followed by a dash and a brief reason. Do not add anything else.`

// stepsForRun folds a run's persisted trace into Lab steps (AC4 replay). The
// userID/projectID enforce that the run belongs to a project the caller can read;
// the fold then reconstructs only what was recorded.
func (s *LabService) stepsForRun(ctx context.Context, userID, projectID, runID string) ([]agentcore.LabStep, error) {
	calls, err := s.store.ListAgentLLMCalls(ctx, userID, projectID, runID)
	if err != nil {
		return nil, err
	}
	return agentcore.FoldSteps(recordsFromCalls(calls)), nil
}

// recordsFromCalls maps persisted LLM-call rows to the neutral TurnRecord shape
// the fold consumes. The trace stores Messages/ToolCalls as the exact JSON the
// trace sink marshalled (agentcore.Message / agentcore.ToolCall), so the reverse
// is a plain unmarshal; a malformed row degrades to an empty slice rather than
// failing the whole fold.
func recordsFromCalls(calls []storage.AgentLLMCall) []agentcore.TurnRecord {
	out := make([]agentcore.TurnRecord, 0, len(calls))
	for _, c := range calls {
		var msgs []agentcore.Message
		_ = json.Unmarshal([]byte(c.MessagesJSON), &msgs)
		var toolCalls []agentcore.ToolCall
		_ = json.Unmarshal([]byte(c.ToolCallsJSON), &toolCalls)
		out = append(out, agentcore.TurnRecord{
			Messages:   msgs,
			Response:   c.Response,
			ToolCalls:  toolCalls,
			Tools:      c.Tools,
			StopReason: c.StopReason,
			Error:      c.Error,
			TokensIn:   c.TokenInput,
			TokensOut:  c.TokenOutput,
			CostUSD:    c.CostUSD,
		})
	}
	return out
}

// ReplaySteps returns the folded steps for any completed run (AC4). It is the
// historical-replay entry point; the same fold powers the live explain view, so
// the two read identically.
func (s *LabService) ReplaySteps(ctx context.Context, userID, projectID, runID string) ([]agentcore.LabStep, error) {
	return s.stepsForRun(ctx, userID, projectID, runID)
}

// --- explain mode ---------------------------------------------------------

// errExplainStopped halts a stepped run when the user stops it. The loop maps a
// non-nil StepGate error to a clean "halted" stop, not a crash.
var errExplainStopped = errors.New("agentruntime: explain run stopped by user")

// explainSession holds the channels that drive one live-stepped run. The run
// goroutine (the SSE request) blocks in StepGate on advance/stop; a separate
// advance/stop request signals these channels. projectID scopes advance/stop to
// the run's own project so a member of another project can't drive it. stopOnce
// guards the stop channel against a double-close panic from concurrent stops.
type explainSession struct {
	projectID string
	advance   chan struct{}
	stop      chan struct{}
	stopOnce  sync.Once
	// steer carries mid-run corrections injected from a sibling /steer request,
	// drained into the loop's steering queue at the top of each turn (before the
	// model reasons). Buffered so a push never blocks the requester.
	steer chan agentcore.Message
}

// LabEvent is one server-sent message of an explain run: the run id at start, a
// paused step view before each turn (after the first), and the terminal done
// frame carrying the full step list and verdict.
type LabEvent struct {
	Type    string              `json:"type"` // run | step | done | error
	RunID   string              `json:"run_id,omitempty"`
	Steps   []agentcore.LabStep `json:"steps,omitempty"`
	Current int                 `json:"current,omitempty"` // index of the latest completed step
	Status  string              `json:"status,omitempty"`
	Final   string              `json:"final,omitempty"`
	Error   string              `json:"error,omitempty"`
}

// StartExplain runs an agent in explain mode, pausing before each turn after the
// first and emitting the folded step-so-far at every pause so the builder can
// inspect what changed before advancing (AC3). It runs synchronously in the
// caller's (SSE) goroutine and blocks for the whole run; Advance/Stop are driven
// from sibling requests via the run id, which the first emitted event carries.
//
// The first turn proceeds on start (starting explain mode is the user action that
// runs step 1); every later turn waits for an explicit Advance. Halting preserves
// the loop's fail-closed gating because the gate sits at the top of the turn,
// before any tool runs.
func (s *LabService) StartExplain(ctx context.Context, userID, projectID, agentID, input string, emit func(LabEvent)) error {
	sess := &explainSession{
		projectID: projectID,
		advance:   make(chan struct{}, 1),
		stop:      make(chan struct{}),
		steer:     make(chan agentcore.Message, liveQueueDepth),
	}
	var runID string

	opts := RunOptions{
		ProjectID: projectID,
		AgentID:   agentID,
		Trigger:   "manual",
		Prompt:    input,
		// Explain runs have no client conversation id, so they source steering
		// directly from the explain session's own queue rather than the LiveRegistry
		// (the GetSteering override takes precedence over the registry in execute()).
		GetSteering: func(context.Context) []agentcore.Message { return drainMessages(sess.steer) },
		OnRunID: func(id string) {
			runID = id
			s.register(id, sess)
			emit(LabEvent{Type: "run", RunID: id})
		},
		StepGate: func(gctx context.Context, turn int) error {
			if turn <= 1 {
				return nil // step 1 runs on start
			}
			// A turn just completed; fold the trace so far and show it, then wait for
			// the user to advance (or stop / disconnect).
			steps, _ := s.stepsForRun(gctx, userID, projectID, runID)
			emit(LabEvent{Type: "step", RunID: runID, Steps: steps, Current: len(steps) - 1})
			select {
			case <-sess.advance:
				return nil
			case <-sess.stop:
				return errExplainStopped
			case <-gctx.Done():
				return gctx.Err()
			}
		},
	}

	run, res, err := s.runner.Run(ctx, opts)
	if runID != "" {
		s.unregister(runID)
	}
	if err != nil {
		emit(LabEvent{Type: "error", RunID: run.ID, Error: err.Error()})
		return err
	}

	steps, _ := s.stepsForRun(ctx, userID, projectID, run.ID)
	cur := len(steps) - 1
	emit(LabEvent{Type: "done", RunID: run.ID, Steps: steps, Current: cur, Status: run.Status, Final: res.Final})
	return nil
}

// Advance releases the next turn of a paused explain run. projectID must match
// the run's own project, so only a member of that project can drive it. Returns
// false when no such run is paused (already finished, stopped, or unknown id) or
// the project doesn't match.
func (s *LabService) Advance(projectID, runID string) bool {
	s.mu.Lock()
	sess, ok := s.sessions[runID]
	s.mu.Unlock()
	if !ok || sess.projectID != projectID {
		return false
	}
	select {
	case sess.advance <- struct{}{}:
		return true
	default:
		return true // an advance is already queued; the run will proceed
	}
}

// Stop halts a paused explain run on the next gate check. projectID must match
// the run's own project. Returns false when no such run is active or the project
// doesn't match. stopOnce makes a concurrent/repeat stop a no-op rather than a
// double-close panic.
func (s *LabService) Stop(projectID, runID string) bool {
	s.mu.Lock()
	sess, ok := s.sessions[runID]
	s.mu.Unlock()
	if !ok || sess.projectID != projectID {
		return false
	}
	sess.stopOnce.Do(func() { close(sess.stop) })
	return true
}

// Steer injects a mid-run correction into a paused explain run; it is drained
// into the loop's steering queue at the top of the next turn (before the model
// reasons), so a builder can nudge a stepped run without restarting it. projectID
// must match the run's own project. Returns false when no such run is active or
// the project doesn't match; a full queue drops the message rather than blocking.
func (s *LabService) Steer(projectID, runID, message string) bool {
	s.mu.Lock()
	sess, ok := s.sessions[runID]
	s.mu.Unlock()
	if !ok || sess.projectID != projectID {
		return false
	}
	select {
	case sess.steer <- agentcore.Message{Role: agentcore.RoleUser, Content: message}:
		return true
	default:
		return true // queue full: next-turn delivery isn't guaranteed, treat as accepted
	}
}

func (s *LabService) register(runID string, sess *explainSession) {
	s.mu.Lock()
	s.sessions[runID] = sess
	s.mu.Unlock()
}

func (s *LabService) unregister(runID string) {
	s.mu.Lock()
	delete(s.sessions, runID)
	s.mu.Unlock()
}

// lineDiff returns a compact line-oriented diff of want vs got via an LCS, with
// '-' lines present only in want, '+' lines only in got, and ' ' for common
// lines. Used to show why a test output missed its expectation (AC5).
func lineDiff(want, got string) string {
	a := strings.Split(want, "\n")
	b := strings.Split(got, "\n")
	// LCS table over lines.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out.WriteString("  " + a[i] + "\n")
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out.WriteString("- " + a[i] + "\n")
			i++
		default:
			out.WriteString("+ " + b[j] + "\n")
			j++
		}
	}
	for ; i < len(a); i++ {
		out.WriteString("- " + a[i] + "\n")
	}
	for ; j < len(b); j++ {
		out.WriteString("+ " + b[j] + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}
