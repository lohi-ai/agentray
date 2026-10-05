package agentruntime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestScheduledProviderTimeoutPersistsTerminalRun(t *testing.T) {
	t.Run("model call timeout", func(t *testing.T) { testScheduledProviderTimeout(t, false) })
	t.Run("deadline before first model turn", func(t *testing.T) { testScheduledProviderTimeout(t, true) })
}

func testScheduledProviderTimeout(t *testing.T, timeoutBeforeModel bool) {
	databaseURL := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("AGENTRAY_TEST_DATABASE_URL is required")
	}
	t.Setenv("AGENT_KEY_ENC_SECRET", "scheduled-timeout-integration-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := storage.Open(ctx, config.Config{
		PostgresURL: databaseURL,
		DuckDBPath:  filepath.Join(t.TempDir(), "scheduled-timeout.duckdb"),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)

	boot, err := st.CreateAccount(ctx,
		fmt.Sprintf("scheduled-timeout-%d@test.local", time.Now().UnixNano()),
		"Scheduled timeout", "password1234", "Timeout workspace", "Timeout project",
	)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	requestStarted := make(chan struct{}, 1)
	releaseProvider := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/embeddings") && !timeoutBeforeModel {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
			return
		}
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-releaseProvider:
		}
	}))
	defer provider.Close()
	defer close(releaseProvider)
	if _, err := st.UpsertWorkspaceModelTiers(ctx, boot.User.ID, boot.Workspace.ID, storage.WorkspaceModelTiersInput{
		Provider: "openai", Model: "timeout-test", BaseURL: provider.URL, APIKey: "test-key",
	}); err != nil {
		t.Fatalf("configure model: %v", err)
	}
	if _, err := st.UpsertAgentConfig(ctx, boot.User.ID, boot.Project.ID, storage.AgentConfigInput{
		Enabled: true, RedactPII: true, Autonomy: storage.AutonomyScheduled,
		Scopes: map[string]bool{"monitor": true, "data_quality": true},
	}); err != nil {
		t.Fatalf("configure agent: %v", err)
	}
	body, err := os.ReadFile(filepath.Join("..", "workloads", "config", lohiObserverVersion, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAgentSkill(ctx, boot.User.ID, boot.Project.ID, "", storage.AgentSkill{
		Name: lohiObserverVersion, Body: string(body), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("start nats-server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect nats: %v", err)
	}
	t.Cleanup(nc.Close)

	scheduler := NewScheduler(nc, st,
		WithTraceSink(NewStoreTraceSink(st)),
		WithSessionStore(NewSessionStore(st)),
	)
	scheduler.runTimeout = time.Second
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("scheduler.Start: %v", err)
	}
	t.Cleanup(scheduler.Stop)
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}
	if err := scheduler.PublishScheduled(boot.Project.ID, boot.Project.ID, "Perform the scheduled observer rehearsal."); err != nil {
		t.Fatalf("PublishScheduled: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush publish: %v", err)
	}
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled provider request did not start")
	}

	var run storage.AgentRun
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, listErr := st.ListAgentRunsForAgent(ctx, boot.User.ID, boot.Project.ID, boot.Project.ID, 5)
		if listErr != nil {
			t.Fatalf("ListAgentRunsForAgent: %v", listErr)
		}
		if len(runs) > 0 {
			run = runs[0]
			if run.Status != "running" {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if run.ID == "" {
		t.Fatal("scheduled path did not create a run")
	}
	if run.Status != "error" || run.FinishedAt == nil {
		t.Fatalf("provider timeout left run non-terminal: %+v", run)
	}
	if !strings.Contains(run.Summary, "context deadline exceeded") {
		t.Fatalf("provider timeout summary = %q, want an auditable deadline reason", run.Summary)
	}
	if !strings.Contains(run.Summary, "data_quality:availability") || !strings.Contains(run.Summary, "remain unverified") {
		t.Fatalf("observer provider failure misrepresented readiness: %q", run.Summary)
	}
	_, tools, err := st.GetAgentRun(ctx, boot.User.ID, boot.Project.ID, run.ID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("provider failed before tool execution, got tools %+v", tools)
	}
	calls, err := st.AgentLLMCallTrace(ctx, boot.User.ID, boot.Project.ID, run.ID)
	if err != nil {
		t.Fatalf("AgentLLMCallTrace: %v", err)
	}
	if timeoutBeforeModel {
		if len(calls) != 0 {
			t.Fatalf("deadline before model turn fabricated a model trace: %+v", calls)
		}
	} else if len(calls) != 1 || calls[0].StopReason != "aborted" || calls[0].Error != "Request aborted" {
		// The scheduler records the deadline cause in the run summary; the native
		// provider trace preserves its explicit abort result.
		t.Fatalf("provider failure trace = %+v, want one auditable aborted model call", calls)
	}
	t.Logf("scheduled run=%s status=%s finished=%s summary=%q tools=%d model_calls=%d", run.ID, run.Status, run.FinishedAt, run.Summary, len(tools), len(calls))
}
