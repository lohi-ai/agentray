package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	agentruntime "github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/workloads"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// This is the Operations rehearsal path: marketplace install, operator-owned
// configuration, HTTP Run now, NATS consumer, real provider adapter/tools, and
// PostgreSQL run/Plans/monitor reads. Only the model's choices are scripted.
func TestConfiguredObserverOperationsRehearsal(t *testing.T) {
	t.Setenv("AGENT_KEY_ENC_SECRET", "observer-rehearsal-test-secret")
	s := openRequiredAppTestStore(t)
	ctx := context.Background()
	storage.SetPackCatalog(marketplacePresets, marketplacePresetBySlug)
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("observer-rehearsal-%d@test.local", time.Now().UnixNano()),
		"Observer rehearsal", "password1234", "Rehearsal workspace", "Rehearsal project")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// A reviewed binding exists but has never published. The production readiness
	// tool must expose this gap; the model must not fabricate a revenue result.
	source, err := s.CreateDataConnector(ctx, boot.User.ID, boot.Project.ID, "Reviewed revenue export", "postgres", "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConnectorSync(ctx, boot.User.ID, boot.Project.ID, source.ID, storage.ConnectorSyncInput{
		SourceTable: "reviewed_payments", KeyColumn: "id", ScheduleCron: "0 * * * *", Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	const period = "2026-10-04 Asia/Ho_Chi_Minh"
	const condition = "data_quality:unknown_receipt"
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(boot.Project.ID+"|lohi-evidence-v1|"+period+"|"+condition)))
	findingArgs, err := json.Marshal(map[string]any{
		"category": "data", "title": "Lohi readiness gap", "rationale": "Reviewed revenue export has never published; business analysis suppressed.",
		"evidence": map[string]any{
			"observation_key": map[string]string{"project": boot.Project.ID, "definition_version": "lohi-evidence-v1", "period": period, "condition": condition},
			"binding":         source.ID, "readiness": "syncing", "reason": "unknown_receipt", "warnings": []string{"no published generation"},
		}, "idempotency_key": key,
	})
	if err != nil {
		t.Fatal(err)
	}
	var failProvider atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
			return
		}
		if failProvider.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"rehearsal provider unavailable","type":"availability_error"}}`))
			return
		}
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID       string
					Function struct{ Name string }
				} `json:"tool_calls"`
			}
			Tools  []json.RawMessage
			Stream bool
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode provider: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !request.Stream {
			t.Error("native rehearsal request did not enable streaming")
		}
		writeChunk := func(delta map[string]any, finish string) {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk, err := json.Marshal(map[string]any{"id": "rehearsal", "choices": []any{map[string]any{
				"index": 0, "delta": delta, "finish_reason": finish,
			}}, "usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 2}})
			if err != nil {
				t.Errorf("encode provider chunk: %v", err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
		}
		writeText := func(content string) {
			writeChunk(map[string]any{"role": "assistant", "content": content}, "stop")
		}
		// The scheduler's optional reflection uses the same adapter after the run.
		if len(request.Tools) == 0 {
			writeText(`{"memories":[]}`)
			return
		}
		// Native tool results correlate by call ID; result names are optional.
		callNames := map[string]string{}
		for _, message := range request.Messages {
			for _, call := range message.ToolCalls {
				callNames[call.ID] = call.Function.Name
			}
		}
		results := map[string]string{}
		for _, message := range request.Messages {
			if message.Role == "tool" {
				results[callNames[message.ToolCallID]] = message.Content
			}
		}
		writeTool := func(name, args string) {
			writeChunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": "call-" + name, "type": "function", "function": map[string]string{"name": name, "arguments": args},
			}}}, "tool_calls")
		}
		switch {
		case results["read_skill"] == "":
			writeTool("read_skill", `{"id":"lohi-revenue-observer-v1"}`)
		case results["list_sources"] == "":
			writeTool("list_sources", `{}`)
		case results["source_status"] == "":
			if !strings.Contains(results["list_sources"], source.ID) {
				t.Error("real list_sources omitted the configured binding")
			}
			writeTool("source_status", fmt.Sprintf(`{"connector_id":%q}`, source.ID))
		case results["list_findings"] == "":
			if !strings.Contains(results["source_status"], `"state":"syncing"`) || !strings.Contains(results["source_status"], `"reason":"unknown_receipt"`) {
				t.Errorf("readiness gap not exposed: %s", results["source_status"])
			}
			writeTool("list_findings", `{"limit":50}`)
		case results["submit_recommendation"] == "" && !strings.Contains(results["list_findings"], "Lohi readiness gap"):
			writeTool("submit_recommendation", string(findingArgs))
		default:
			writeText(condition + "; binding " + source.ID + " is syncing with unknown receipt for " + period + ". Business analysis suppressed; existing finding retained; delivery skipped because no channel is configured.")
		}
	}))
	defer provider.Close()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS did not start")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	scheduler := agentruntime.NewScheduler(nc, s, agentruntime.WithTraceSink(agentruntime.NewStoreTraceSink(s)), agentruntime.WithSessionStore(agentruntime.NewSessionStore(s)))
	if err := scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scheduler.Stop)
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	registerAgentRoutes(e, s, scheduler, nil, agentruntime.ToolBuildContext{}, agentruntime.NewLiveRegistry(), false, nil)
	registerAgentMonitorRoutes(e, s)
	registerOperationsRoutes(e, s, scheduler)
	request := func(method, path string, body any, want int) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		req := httptest.NewRequest(method, path+separator+"project_id="+boot.Project.ID, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, rec.Code, rec.Body.String(), want)
		}
		t.Logf("%s %s = %d %s", method, path, rec.Code, rec.Body.String())
		return rec.Body.Bytes()
	}
	var installed struct{ Agent storage.Agent }
	if err := json.Unmarshal(request(http.MethodPost, "/api/marketplace/agents/insight-digest/install", nil, http.StatusCreated), &installed); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPut, "/api/workspace/models", map[string]string{"provider": "openai", "model": "observer-rehearsal", "base_url": provider.URL, "api_key": "test-key"}, http.StatusOK)
	request(http.MethodPut, "/api/agent/config", storage.AgentConfigInput{
		Enabled: true, RedactPII: true, Autonomy: storage.AutonomyScheduled,
		Scopes: map[string]bool{"monitor": true, "data_quality": true, "analyze_build": true, "growth_suggest": true},
	}, http.StatusOK)
	var manifest struct{ Schedules []storage.AgentTrigger }
	if err := json.Unmarshal(workloads.LohiRevenueObserverManifest(), &manifest); err != nil {
		t.Fatal(err)
	}
	var daily storage.AgentTrigger
	for i, template := range manifest.Schedules {
		if template.Enabled {
			t.Fatal("observer template unexpectedly enabled")
		}
		template.Kind = storage.TriggerSchedule
		template.PromptTemplate += " Rehearsal closed HCM period: " + period + ". No authorized delivery channel."
		var trigger storage.AgentTrigger
		if err := json.Unmarshal(request(http.MethodPost, "/api/agent/triggers?agent="+installed.Agent.ID, template, http.StatusCreated), &trigger); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			daily = trigger
		}
	}
	request(http.MethodGet, "/api/agents/"+installed.Agent.ID+"/monitor", nil, http.StatusOK)
	waitRun := func(previous string, wantStatus string) storage.AgentRun {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			runs, err := s.ListAgentRunsForAgent(ctx, boot.User.ID, boot.Project.ID, installed.Agent.ID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) > 0 && runs[0].ID != previous && runs[0].FinishedAt != nil {
				if runs[0].Status != wantStatus {
					t.Fatalf("rehearsal terminal status: %+v", runs[0])
				}
				return runs[0]
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("Operations rehearsal never reached a terminal state")
		return storage.AgentRun{}
	}
	var previous string
	for attempt := 0; attempt < 2; attempt++ {
		request(http.MethodPost, "/api/operations/"+daily.ID+"/run", nil, http.StatusAccepted)
		run := waitRun(previous, "done")
		previous = run.ID
		if !strings.Contains(run.Summary, condition) || !strings.Contains(run.Summary, "Business analysis suppressed") {
			t.Fatalf("missing readiness-gated result: %+v", run)
		}
		_, calls, err := s.GetAgentRun(ctx, boot.User.ID, boot.Project.ID, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, call := range calls {
			if !call.Allowed || strings.Contains(call.ResultMeta, " error:") || call.Tool == "send_notification" || call.Tool == "run_sql" {
				t.Fatalf("gate allowed unauthorized analysis/delivery or failed tool: %+v", call)
			}
			names = append(names, call.Tool)
		}
		want := "read_skill,list_sources,source_status,list_findings"
		if attempt == 0 {
			want += ",submit_recommendation"
		}
		if strings.Join(names, ",") != want {
			t.Fatalf("rehearsal tool history=%v, want %s", names, want)
		}
		t.Logf("rehearsal attempt=%d run=%s status=%s finished=%s tools=%v summary=%q", attempt+1, run.ID, run.Status, run.FinishedAt, names, run.Summary)
	}
	findings, _, err := s.ListRecommendationsPage(ctx, boot.Project.ID, "", 50)
	if err != nil || len(findings) != 1 || findings[0].Category != "data" {
		t.Fatalf("repeat rehearsal findings=%+v err=%v, want one category-data finding", findings, err)
	}
	failProvider.Store(true)
	request(http.MethodPost, "/api/operations/"+daily.ID+"/run", nil, http.StatusAccepted)
	failure := waitRun(previous, "error")
	if !strings.Contains(failure.Summary, "data_quality:availability") || !strings.Contains(failure.Summary, "rehearsal provider unavailable") {
		t.Fatalf("provider failure was not auditable: %+v", failure)
	}
	trace, err := s.AgentLLMCallTrace(ctx, boot.User.ID, boot.Project.ID, failure.ID)
	if err != nil || len(trace) == 0 || trace[0].Error == "" {
		t.Fatalf("missing failed provider trace: %+v %v", trace, err)
	}
	t.Logf("provider-failure run=%s status=%s finished=%s summary=%q trace_error=%q", failure.ID, failure.Status, failure.FinishedAt, failure.Summary, trace[0].Error)
	var detail struct{ Agent storage.AgentMonitorRow }
	if err := json.Unmarshal(request(http.MethodGet, "/api/agents/"+installed.Agent.ID+"/monitor", nil, http.StatusOK), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Agent.RunCount != 3 || detail.Agent.ErrorCount != 1 || detail.Agent.RunningCount != 0 || detail.Agent.LastError != failure.Summary {
		t.Fatalf("incorrect monitoring totals: %+v", detail.Agent)
	}
	request(http.MethodGet, "/api/agents/monitor", nil, http.StatusOK)
	request(http.MethodGet, "/api/operations/"+daily.ID, nil, http.StatusOK)
	request(http.MethodPut, "/api/agent/agents/"+installed.Agent.ID, map[string]any{"name": installed.Agent.Name, "enabled": false}, http.StatusOK)
	request(http.MethodPost, "/api/operations/"+daily.ID+"/run", nil, http.StatusConflict)
}
