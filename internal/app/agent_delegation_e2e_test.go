//go:build e2e

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/config"
)

// llmStub is a scripted OpenAI-wire /chat/completions server. It answers by
// inspecting each request instead of by call order, so incidental calls the
// runtime makes (classifier, embeddings, summaries) can't derail the script:
//
//   - the front-desk classifier is routed to "data" so the real agent runs;
//   - the parent agent (the only one whose tool list carries spawn_subagent)
//     first delegates to the named teammate, then answers with whatever the
//     tool result said;
//   - the delegate agent (no spawn_subagent in its tools — depth 1 hides it)
//     recognizes the delegated task marker and answers the magic word.
type llmStub struct {
	delegateName string
	delegateHits atomic.Int32
	// depthLeakHits counts requests where a delegate-side turn still advertises
	// spawn_subagent — the depth cap failing would show up here.
	depthLeakHits atomic.Int32

	mu       sync.Mutex
	requests []string // one summary line per request, dumped on test failure
}

func (s *llmStub) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, line)
}

func (s *llmStub) dump(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, line := range s.requests {
		t.Logf("llm stub request %d: %s", i+1, line)
	}
}

const (
	stubTaskMarker = "STUB_DELEGATED_TASK: fetch the magic word"
	stubMagicWord  = "MAGIC-WORD-XYZZY"
)

func (s *llmStub) handler() http.HandlerFunc {
	type oaiMsg struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolCalls []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	type oaiReq struct {
		Stream   bool     `json:"stream"`
		Messages []oaiMsg `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}

	usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5}
	// The runtime uses both wire shapes — Chat() for the classifier and other
	// non-interactive calls, Stream() (SSE deltas) for agent turns — so the stub
	// answers in whichever the request asked for.
	writeSSE := func(w http.ResponseWriter, chunks ...map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			raw, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	writeJSON := func(w http.ResponseWriter, message map[string]any, finishReason string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": message, "finish_reason": finishReason}},
			"usage":   usage,
		})
	}
	final := func(w http.ResponseWriter, stream bool, content string) {
		if stream {
			writeSSE(w,
				map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": content}, "finish_reason": nil}}},
				map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage},
			)
			return
		}
		writeJSON(w, map[string]any{"role": "assistant", "content": content}, "stop")
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/embeddings") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":  []map[string]any{{"embedding": []float64{0.1, 0.2, 0.3}}},
				"usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1},
			})
			return
		}

		var req oaiReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}

		hasSpawn := false
		for _, tool := range req.Tools {
			if tool.Function.Name == "spawn_subagent" {
				hasSpawn = true
			}
		}
		delegatedTurn := false
		var lastToolResult string
		for _, m := range req.Messages {
			if m.Role == "tool" {
				lastToolResult = m.Content
			}
			if strings.Contains(m.Content, stubTaskMarker) {
				delegatedTurn = true
			}
		}

		summary := func(branch string) string {
			var b strings.Builder
			fmt.Fprintf(&b, "branch=%s spawnTool=%v delegatedTurn=%v", branch, hasSpawn, delegatedTurn)
			for _, m := range req.Messages {
				content := m.Content
				if r := []rune(content); len(r) > 120 {
					content = string(r[:120]) + "…"
				}
				fmt.Fprintf(&b, " | %s: %q", m.Role, content)
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, " toolcall=%s", tc.Function.Name)
				}
			}
			return b.String()
		}

		// The cheap front-desk classifier: always route to the real agent.
		for _, m := range req.Messages {
			if m.Role == "system" && strings.Contains(m.Content, "front desk") {
				s.record(summary("classifier"))
				final(w, req.Stream, `{"route":"data"}`)
				return
			}
		}

		switch {
		case delegatedTurn && !hasSpawn:
			// The delegate agent's own run (spawn_subagent must not be advertised
			// past the depth cap).
			s.record(summary("delegate"))
			s.delegateHits.Add(1)
			final(w, req.Stream, stubMagicWord)
		case delegatedTurn && hasSpawn:
			s.record(summary("depth-leak"))
			s.depthLeakHits.Add(1)
			final(w, req.Stream, stubMagicWord)
		case hasSpawn && lastToolResult != "":
			// Parent turn 2: the spawn_subagent tool result is back — answer with it.
			s.record(summary("parent-2"))
			final(w, req.Stream, "The teammate replied: "+lastToolResult)
		case hasSpawn:
			// Parent turn 1: hand the task to the granted teammate.
			s.record(summary("parent-1"))
			args, _ := json.Marshal(map[string]string{
				"task":  stubTaskMarker,
				"agent": s.delegateName,
			})
			if req.Stream {
				writeSSE(w,
					map[string]any{"choices": []map[string]any{{
						"delta": map[string]any{"tool_calls": []map[string]any{{
							"index": 0,
							"id":    "call_delegate_1",
							"type":  "function",
							"function": map[string]any{
								"name":      "spawn_subagent",
								"arguments": string(args),
							},
						}}},
						"finish_reason": nil,
					}}},
					map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": usage},
				)
				return
			}
			writeJSON(w, map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   "call_delegate_1",
					"type": "function",
					"function": map[string]any{
						"name":      "spawn_subagent",
						"arguments": string(args),
					},
				}},
			}, "tool_calls")
		default:
			// Any other incidental call (summaries, reflection, …).
			s.record(summary("other"))
			final(w, req.Stream, "ok")
		}
	}
}

// TestAgentDelegationE2E exercises cross-agent delegation end to end against
// real Postgres + the full HTTP surface: the Teammates grant API (CRUD +
// validation), then an actual delegated run — the default agent's
// spawn_subagent routes a task to a granted teammate, which runs under its own
// identity (own run row, trigger "delegate") on a scripted OpenAI-wire stub.
func TestAgentDelegationE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for e2e test")
	}
	t.Setenv("AGENT_KEY_ENC_SECRET", "agentray-delegation-e2e-secret")

	root := serviceRoot(t)
	project := fmt.Sprintf("agentray-delegation-e2e-%d", time.Now().UnixNano())
	infraHost := os.Getenv("AGENTRAY_E2E_INFRA_HOST")
	if infraHost == "" {
		infraHost = "127.0.0.1"
	}
	pgPort := freePort(t)
	chHTTPPort := freePort(t)
	chNativePort := freePort(t)
	redisPort := freePort(t)
	natsPort := freePort(t)

	env := append(os.Environ(),
		fmt.Sprintf("AGENTRAY_POSTGRES_PORT=%d", pgPort),
		fmt.Sprintf("AGENTRAY_CLICKHOUSE_HTTP_PORT=%d", chHTTPPort),
		fmt.Sprintf("AGENTRAY_CLICKHOUSE_NATIVE_PORT=%d", chNativePort),
		fmt.Sprintf("AGENTRAY_REDIS_PORT=%d", redisPort),
		fmt.Sprintf("AGENTRAY_NATS_PORT=%d", natsPort),
	)

	composeUp := exec.Command("docker", "compose", "-p", project, "-f", filepath.Join(root, "docker-compose.yml"), "up", "-d", "postgres", "clickhouse", "redis", "nats")
	composeUp.Dir = root
	composeUp.Env = env
	if output, err := composeUp.CombinedOutput(); err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		composeDown := exec.Command("docker", "compose", "-p", project, "-f", filepath.Join(root, "docker-compose.yml"), "down", "-v")
		composeDown.Dir = root
		composeDown.Env = env
		if output, err := composeDown.CombinedOutput(); err != nil {
			t.Logf("docker compose down failed: %v\n%s", err, output)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	waitForTCP(t, ctx, fmt.Sprintf("%s:%d", infraHost, pgPort))
	waitForTCP(t, ctx, fmt.Sprintf("%s:%d", infraHost, redisPort))
	waitForTCP(t, ctx, fmt.Sprintf("%s:%d", infraHost, natsPort))
	waitForHTTP(t, ctx, fmt.Sprintf("http://%s:%d/ping", infraHost, chHTTPPort))

	cfg := config.Config{
		PostgresURL:          fmt.Sprintf("postgres://lohi:lohi@%s:%d/lohi_analytics?sslmode=disable", infraHost, pgPort),
		ClickHouseAddr:       fmt.Sprintf("%s:%d", infraHost, chNativePort),
		ClickHouseDatabase:   "lohi_analytics",
		ClickHouseUser:       "lohi",
		ClickHousePassword:   "lohi",
		RedisURL:             fmt.Sprintf("redis://%s:%d/0", infraHost, redisPort),
		NATSURL:              fmt.Sprintf("nats://%s:%d", infraHost, natsPort),
		IngestSubject:        "agentray.delegation-e2e.events.ingest",
		RateLimitPerMinute:   100,
		DefaultProjectName:   "AgentRay delegation e2e",
		DefaultProjectAPIKey: "agentray_delegation_e2e_token",
		AllowedOrigins:       "http://localhost:3100,http://127.0.0.1:3100",
	}

	srv, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("shutdown failed: %v", err)
		}
	})

	ts := httptest.NewServer(srv.echo)
	defer ts.Close()
	client := ts.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client.Jar = jar

	var signup authResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/auth/signup", map[string]any{
		"email":          "delegation-e2e@example.com",
		"name":           "Delegation E2E Admin",
		"password":       "agentray-e2e",
		"workspace_name": "Delegation e2e workspace",
		"project_name":   "Delegation e2e project",
	}, &signup, http.StatusCreated)
	if signup.User.ID == "" || signup.Project.ID == "" {
		t.Fatalf("signup did not return account resources: %+v", signup)
	}
	projectID := signup.Project.ID
	withProject := func(path string) string { return ts.URL + path + "?project_id=" + projectID }

	// --- the teammate agent the default agent will delegate to ---
	var helper struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Slug      string `json:"slug"`
		IsDefault bool   `json:"is_default"`
		Enabled   bool   `json:"enabled"`
	}
	requestJSON(t, client, http.MethodPost, withProject("/api/agent/agents"), map[string]any{
		"name": "Echo Helper",
	}, &helper, http.StatusCreated)
	if helper.ID == "" || helper.IsDefault {
		t.Fatalf("created helper agent looks wrong: %+v", helper)
	}

	// --- grant API: CRUD + validation over real Postgres ---
	type delegatesResponse struct {
		Agents []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			IsDefault bool   `json:"is_default"`
		} `json:"agents"`
		Selections []struct {
			AgentID string `json:"agent_id"`
			Name    string `json:"name"`
			Slug    string `json:"slug"`
			Enabled bool   `json:"enabled"`
		} `json:"selections"`
	}

	var before delegatesResponse
	getJSONMust(t, client, withProject("/api/agent/delegates"), &before)
	if len(before.Selections) != 0 {
		t.Fatalf("fresh agent should have no delegate grants: %+v", before.Selections)
	}
	rosterHasHelper := false
	rosterHasDefault := false
	for _, a := range before.Agents {
		if a.ID == helper.ID {
			rosterHasHelper = true
		}
		// Signup seeds the Growth Lead preset as the project's default agent
		// (SeedDefaultFoundationAgent); a fresh roster must carry it.
		if a.IsDefault && a.Name != "" {
			rosterHasDefault = true
		}
	}
	if !rosterHasHelper {
		t.Fatalf("delegates roster is missing the helper agent: %+v", before.Agents)
	}
	if !rosterHasDefault {
		t.Fatalf("delegates roster is missing the seeded default agent: %+v", before.Agents)
	}

	// Self-grant is refused (self-delegation is built into the harness). The
	// default agent's id equals the project id.
	requestJSON(t, client, http.MethodPut, withProject("/api/agent/delegates/"+projectID), map[string]any{
		"enabled": true,
	}, nil, http.StatusBadRequest)

	// An agent outside the project is refused.
	requestJSON(t, client, http.MethodPut, withProject("/api/agent/delegates/00000000-0000-0000-0000-000000000001"), map[string]any{
		"enabled": true,
	}, nil, http.StatusBadRequest)

	// A valid grant round-trips, deletes, and re-creates.
	requestJSON(t, client, http.MethodPut, withProject("/api/agent/delegates/"+helper.ID), map[string]any{
		"enabled": true,
	}, nil, http.StatusOK)
	var granted delegatesResponse
	getJSONMust(t, client, withProject("/api/agent/delegates"), &granted)
	if len(granted.Selections) != 1 || granted.Selections[0].AgentID != helper.ID || !granted.Selections[0].Enabled {
		t.Fatalf("expected one enabled grant for the helper: %+v", granted.Selections)
	}
	if granted.Selections[0].Name != "Echo Helper" {
		t.Fatalf("grant selection should carry the delegate's name: %+v", granted.Selections[0])
	}

	assertStatus(t, client, http.MethodDelete, withProject("/api/agent/delegates/"+helper.ID), nil, http.StatusNoContent)
	var cleared delegatesResponse
	getJSONMust(t, client, withProject("/api/agent/delegates"), &cleared)
	if len(cleared.Selections) != 0 {
		t.Fatalf("delete should clear the grant: %+v", cleared.Selections)
	}
	requestJSON(t, client, http.MethodPut, withProject("/api/agent/delegates/"+helper.ID), map[string]any{
		"enabled": true,
	}, nil, http.StatusOK)

	// The grant mutations land in the workspace audit log.
	var audit workspaceAuditLogsResponse
	getJSONMust(t, client, ts.URL+"/api/workspaces/"+signup.Project.WorkspaceID+"/audit-logs", &audit)
	if !auditHasAction(audit, "agent.delegate.update") || !auditHasAction(audit, "agent.delegate.delete") {
		t.Fatalf("expected agent.delegate.update and agent.delegate.delete audit entries, got: %+v", audit.Logs)
	}

	// --- a real delegated run, driven by the scripted OpenAI-wire stub ---
	stub := &llmStub{delegateName: helper.Name}
	llm := httptest.NewServer(stub.handler())
	defer llm.Close()

	requestJSON(t, client, http.MethodPut, withProject("/api/workspace/models"), map[string]any{
		"provider": "openai",
		"model":    "stub-model",
		"base_url": llm.URL,
		"api_key":  "stub-key",
	}, nil, http.StatusOK)
	requestJSON(t, client, http.MethodPut, withProject("/api/agent/config"), map[string]any{
		"enabled":    true,
		"autonomy":   "suggest",
		"scopes":     map[string]bool{},
		"redact_pii": true,
	}, nil, http.StatusOK)

	var chat struct {
		RunID string `json:"run_id"`
		Final string `json:"final"`
	}
	requestJSON(t, client, http.MethodPost, withProject("/api/agent/chat"), map[string]any{
		"message": "Ask your teammate for the magic word and report it back.",
	}, &chat, http.StatusOK)
	if !strings.Contains(chat.Final, stubMagicWord) {
		stub.dump(t)
		t.Fatalf("final answer should carry the delegate's magic word, got: %q (run %s)", chat.Final, chat.RunID)
	}
	if got := stub.delegateHits.Load(); got < 1 {
		t.Fatalf("the delegate agent's own run never reached the LLM (delegate hits = %d)", got)
	}
	if got := stub.depthLeakHits.Load(); got != 0 {
		t.Fatalf("spawn_subagent leaked past the delegation depth cap (%d delegate turns advertised it)", got)
	}

	// The delegate ran under its own identity: its own run row, keyed to the
	// helper agent, with trigger "delegate"; the parent chat run is separate.
	var runs struct {
		Runs []struct {
			ID      string `json:"id"`
			AgentID string `json:"agent_id"`
			Trigger string `json:"trigger"`
			Status  string `json:"status"`
		} `json:"runs"`
	}
	getJSONMust(t, client, withProject("/api/agent/runs"), &runs)
	var delegateRunID string
	parentRunSeen := false
	for _, run := range runs.Runs {
		if run.Trigger == "delegate" && run.AgentID == helper.ID {
			delegateRunID = run.ID
			if run.Status != "done" {
				t.Fatalf("delegate run should be done, got: %+v", run)
			}
		}
		if run.ID == chat.RunID {
			parentRunSeen = true
			if run.AgentID == helper.ID {
				t.Fatalf("parent run must belong to the default agent, not the delegate: %+v", run)
			}
		}
	}
	if delegateRunID == "" {
		t.Fatalf("no run row with trigger=delegate for the helper agent: %+v", runs.Runs)
	}
	if !parentRunSeen {
		t.Fatalf("parent chat run %s missing from run list: %+v", chat.RunID, runs.Runs)
	}
	if delegateRunID == chat.RunID {
		t.Fatalf("delegate must not reuse the parent's run row")
	}
}
