package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/2found/2ai/agentcore"
	nativehost "github.com/2found/2ai/agentcore/host"
	"github.com/2found/2ai/telemetry/llm"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

type piLargeEvidenceTool struct{ effects *atomic.Int32 }

func (piLargeEvidenceTool) Name() string { return "read" }
func (piLargeEvidenceTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: "read", Description: "read evidence", Parameters: map[string]any{"type": "object"}}
}
func (t piLargeEvidenceTool) Run(context.Context, string) (string, error) {
	return fmt.Sprintf("tool-result-%d: %s", t.effects.Add(1), strings.Repeat("evidence ", 500)), nil
}

func TestPiRunnerCompactsSinglePromptToolLoopUsingNativeSummaryTier(t *testing.T) {
	ctx := llm.WithTraceID(piSessionContext(t), "request-compaction")
	const native = true
	var mainCalls, summaryCalls, effects, compactedCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Messages json.RawMessage
			Tools    []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw := string(body.Messages)
		if body.Model == "summarizer" {
			if native && r.Header.Get("Authorization") != "Bearer summary-current" {
				t.Error("summary used the parent's provider-row credential")
			}
			summaryCalls.Add(1)
			if len(body.Tools) != 0 || (!strings.Contains(raw, "tool-result-") && !strings.Contains(raw, "Earlier work summary")) {
				t.Errorf("invalid native summary request: %s", raw)
			}
			piChildSSE(w, "", "", "Keep working on the original task. Earlier evidence has been checked.")
			return
		}
		if native && r.Header.Get("Authorization") != "Bearer main-current" {
			t.Error("parent did not refresh its provider-row credential")
		}
		n := mainCalls.Add(1)
		if strings.Contains(raw, "Earlier work summary") {
			compactedCalls.Add(1)
			if strings.Contains(raw, "tool-result-1") {
				t.Error("compacted request still carries old tool result")
			}
		}
		if n <= 3 {
			piChildSSE(w, "read", "{}", "")
		} else {
			piChildSSE(w, "", "", "finished")
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{piLargeEvidenceTool{&effects}}
	p.MaxContextTokens, p.KeepRecentTokens = 1500, 500
	traces := newRecordingStore()
	p.Tracer = newTestSink(traces)
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "main", BaseURL: server.URL + "/v1", APIKey: "test"}}
	tier.ProviderID = "main-row"
	summaryTier := tier
	summaryTier.Model = "summarizer"
	summaryTier.ProviderID = "summary-row"
	p.PiCompactionTier = &summaryTier
	if native {
		p.RefreshKey = func(context.Context, string) (string, error) {
			t.Error("native runner used vendor-only credential refresh")
			return "", fmt.Errorf("vendor-only refresh forbidden")
		}
		p.RefreshProviderKey = func(_ context.Context, id, vendor, endpoint string) (string, error) {
			if vendor != "openai" || endpoint != server.URL+"/v1" {
				return "", fmt.Errorf("unexpected credential route")
			}
			switch id {
			case "main-row":
				return "main-current", nil
			case "summary-row":
				return "summary-current", nil
			default:
				return "", fmt.Errorf("unexpected provider row")
			}
		}
	}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "complete the original task"}, tier, nil)
	if err != nil || result.Final != "finished" || effects.Load() != 3 || mainCalls.Load() != 4 || summaryCalls.Load() == 0 || compactedCalls.Load() == 0 {
		t.Fatalf("native loop failed: %+v %v main=%d summary=%d compacted=%d effects=%d", result, err, mainCalls.Load(), summaryCalls.Load(), compactedCalls.Load(), effects.Load())
	}
	if result.Usage.InputTokens != int(mainCalls.Load()+summaryCalls.Load())*7 || result.Usage.OutputTokens != int(mainCalls.Load()+summaryCalls.Load())*3 {
		t.Fatalf("summary usage missing/doubled: %+v", result.Usage)
	}
	state := string(result.NativeState)
	if !strings.Contains(state, "tool-result-1") || strings.Contains(state, "agentrayContextSummary") {
		t.Fatal("request view replaced original native state")
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	receipts := 0
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiContextSummary {
			receipts++
			if _, err := nativehost.ParseSummary(entry.Content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if receipts == 0 {
		t.Fatal("request summaries were not durable")
	}
	recovered, err := recoverPiState(entries)
	if err != nil {
		t.Fatal(err)
	}
	var original, restored struct{ Messages json.RawMessage }
	_ = json.Unmarshal(result.NativeState, &original)
	_ = json.Unmarshal(recovered, &restored)
	if !nativehost.SameJSON(original.Messages, restored.Messages) {
		t.Fatal("summary metadata changed session recovery")
	}
	traces.mu.Lock()
	defer traces.mu.Unlock()
	summaryRows, total := 0, 0
	for _, row := range traces.rows {
		total += row.TokenInput
		if row.Model == "summarizer" {
			summaryRows++
			if row.SessionKey == p.SessionID || !strings.Contains(row.SessionKey, "/summary-") {
				t.Fatal("auxiliary request used conversational trace session")
			}
		}
	}
	if summaryRows != int(summaryCalls.Load()) || total != result.Usage.InputTokens {
		t.Fatalf("trace and native usage differ: %d %+v", total, result.Usage)
	}
}

func TestPiRequestCompactionFreshWorkerResumeReusesDurableSummary(t *testing.T) {
	checkPiRequestCompactionResume(t, agentcore.NewMemorySessionStore(), "request-view")
}

func TestPiRequestCompactionPostgresResume(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL for native request summary recovery")
	}
	ctx := piSessionContext(t)
	st, err := storage.Open(ctx, config.Config{PostgresURL: url, DuckDBPath: filepath.Join(t.TempDir(), "request-summary.duckdb")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx, "request-summary-"+uuid.NewString()+"@test.local", "Native summaries", "password1234", "Native summaries", "Native summaries")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateAgentRun(ctx, boot.Project.ID, "", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	checkPiRequestCompactionResume(t, NewSessionStore(st), run)
}

func checkPiRequestCompactionResume(t *testing.T, store agentcore.SessionStore, sessionID string) {
	t.Helper()
	ctx := piSessionContext(t)
	seed := piLongRequest()
	var summaries, requests atomic.Int32
	worker := NativeAgentConfig{Options: piRequestJSON(map[string]any{"initialState": map[string]any{"messages": seed}}), Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "stream" {
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
		requests.Add(1)
		var request struct {
			Context struct{ Messages json.RawMessage }
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, err
		}
		if !strings.Contains(string(request.Context.Messages), "saved facts") || strings.Contains(string(request.Context.Messages), `"opaque":0`) {
			t.Errorf("provider did not use saved view: %s", request.Context.Messages)
		}
		return piSessionReply(false), nil
	}}
	compaction := &nativehost.CompactionPolicy{Budget: 1500, KeepRecent: 500, Summarize: func(context.Context, json.RawMessage, string) (string, agentcore.Usage, error) {
		summaries.Add(1)
		return "saved facts", agentcore.Usage{InputTokens: 17, OutputTokens: 2}, nil
	}}
	cfg := PiRunConfig{Input: piRequestJSON("finish"), Compaction: compaction, PricingKnown: true, Session: PiSessionConfig{Pi: worker, Store: store, SessionID: sessionID}}
	first, err := RunPi(ctx, cfg)
	if err != nil || first.Projection.Usage.InputTokens != 18 || summaries.Load() != 1 {
		t.Fatalf("first request: %+v %v summaries=%d", first, err, summaries.Load())
	}
	cfg.Session.Resume = true
	cfg.Input = piRequestJSON("continue")
	// A larger current budget should still reuse the already-paid valid view.
	cfg.Compaction = &nativehost.CompactionPolicy{Budget: 100000, KeepRecent: 500, Summarize: compaction.Summarize}
	second, err := RunPi(ctx, cfg)
	if err != nil || second.Projection.Usage.InputTokens != 1 || summaries.Load() != 1 || requests.Load() != 2 {
		t.Fatalf("resume charged/regenerated old summary: %+v %v summaries=%d", second, err, summaries.Load())
	}
	var state struct{ Messages []json.RawMessage }
	_ = json.Unmarshal(second.State, &state)
	for i, raw := range seed {
		if !nativehost.SameJSON(raw, state.Messages[i]) {
			t.Fatal("resumed native transcript lost original prefix")
		}
	}
	if strings.Contains(string(second.State), "agentrayContextSummary") {
		t.Fatal("summary became a native transcript message")
	}
}

func TestPiRequestCompactionStorageFailurePreventsUnrecordedView(t *testing.T) {
	ctx := piSessionContext(t)
	store := &piFailStore{MemorySessionStore: agentcore.NewMemorySessionStore(), kind: agentcore.EntryPiContextSummary}
	var calls atomic.Int32
	result, err := RunPi(ctx, PiRunConfig{Input: piRequestJSON("finish"), Compaction: &nativehost.CompactionPolicy{Budget: 1500, KeepRecent: 500, Summarize: func(context.Context, json.RawMessage, string) (string, agentcore.Usage, error) {
		return "summary", agentcore.Usage{InputTokens: 11}, nil
	}}, Session: PiSessionConfig{Store: store, SessionID: "failed-summary", Pi: NativeAgentConfig{Options: piRequestJSON(map[string]any{"initialState": map[string]any{"messages": piLongRequest()}}), Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		calls.Add(1)
		return piSessionReply(false), nil
	}}}})
	if err == nil || calls.Load() != 0 || result.Projection.Usage.InputTokens != 11 {
		t.Fatalf("unrecorded summary reached provider or lost spend: %+v %v calls=%d", result, err, calls.Load())
	}
	entries, err := store.Log(ctx, "failed-summary")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiContextSummary {
			t.Fatal("failed receipt committed")
		}
	}
}

func TestPiRequestCompactionCancellationSettlesWithoutProviderCall(t *testing.T) {
	ctx, cancel := context.WithCancel(piSessionContext(t))
	defer cancel()
	started := make(chan struct{})
	var calls atomic.Int32
	done := make(chan struct {
		result PiRunResult
		err    error
	}, 1)
	go func() {
		result, err := RunPi(ctx, PiRunConfig{Input: piRequestJSON("finish"), Compaction: &nativehost.CompactionPolicy{Budget: 1500, KeepRecent: 500, Summarize: func(ctx context.Context, _ json.RawMessage, _ string) (string, agentcore.Usage, error) {
			close(started)
			<-ctx.Done()
			return "", agentcore.Usage{InputTokens: 9}, ctx.Err()
		}}, Session: PiSessionConfig{Pi: NativeAgentConfig{Options: piRequestJSON(map[string]any{"initialState": map[string]any{"messages": piLongRequest()}}), Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
			calls.Add(1)
			return piSessionReply(false), nil
		}}}})
		done <- struct {
			result PiRunResult
			err    error
		}{result, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("summary did not start")
	}
	cancel()
	select {
	case got := <-done:
		if got.err == nil || calls.Load() != 0 || got.result.Projection.Usage.InputTokens != 9 {
			t.Fatalf("cancellation lost accounting or entered provider: %+v %v calls=%d", got.result, got.err, calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("summary cancellation left native run blocked")
	}
}

func TestPiChildRequestCompactionKeepsIndependentViewAndUsage(t *testing.T) {
	ctx := llm.WithTraceID(piSessionContext(t), "child-compaction")
	var parents, children, summaries, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Messages json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw := string(body.Messages)
		if body.Model == "summary" {
			summaries.Add(1)
			if strings.Contains(raw, "PARENT-ONLY") {
				t.Error("child summary inherited parent transcript")
			}
			piChildSSE(w, "", "", "Child task and earlier evidence.")
			return
		}
		if strings.Contains(raw, "PARENT-ONLY") {
			if parents.Add(1) == 1 {
				piChildSSE(w, "spawn_subagent", `{"task":"child task"}`, "")
			} else {
				piChildSSE(w, "", "", "parent finished")
			}
			return
		}
		if children.Add(1) <= 3 {
			piChildSSE(w, "read", "{}", "")
		} else {
			piChildSSE(w, "", "", "child finished")
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{piLargeEvidenceTool{&effects}}
	p.MaxContextTokens, p.KeepRecentTokens = 1500, 500
	traces := newRecordingStore()
	p.Tracer = newTestSink(traces)
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "main", BaseURL: server.URL + "/v1", APIKey: "test"}}
	summaryTier := tier
	summaryTier.Model = "summary"
	p.PiCompactionTier = &summaryTier
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
	if err != nil || result.Final != "parent finished" || summaries.Load() == 0 || effects.Load() != 3 || children.Load() != 4 {
		t.Fatalf("child compaction failed: %+v %v summaries=%d children=%d effects=%d", result, err, summaries.Load(), children.Load(), effects.Load())
	}
	if result.Usage.InputTokens != int(parents.Load()+children.Load()+summaries.Load())*7 {
		t.Fatalf("child summary spend not folded once: %+v", result.Usage)
	}
	traces.mu.Lock()
	defer traces.mu.Unlock()
	for _, row := range traces.rows {
		if row.Model == "summary" && (row.Depth != 1 || row.SessionKey == p.SessionID || strings.Contains(row.NativeTraceJSON, "PARENT-ONLY")) {
			t.Fatalf("child summary attribution leaked: %+v", row)
		}
	}
}
