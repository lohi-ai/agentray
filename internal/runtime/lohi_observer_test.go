package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

const lohiObserverVersion = "lohi-revenue-observer-v1"

func TestLohiObserverSubmitRecommendationAllowsScheduledFollowup(t *testing.T) {
	for _, trigger := range []string{"scheduled", "manual"} {
		t.Run(trigger, func(t *testing.T) {
			_, hooks := buildToolsAndHooks(BuildParams{
				Trigger: trigger,
				TerminalFollowupSkills: map[string]string{
					"submit_recommendation": "observer-skill-id",
				},
			}, "scope")
			if _, stop := hooks.After[0](context.Background(), agentcore.ToolCall{
				Name: "submit_recommendation",
			}, "receipt", nil); !stop {
				t.Fatal("submit_recommendation continued before the observer skill was loaded")
			}
			if _, stop := hooks.After[0](context.Background(), agentcore.ToolCall{
				Name: "read_skill", Arguments: `{"id":"observer-skill-id"}`,
			}, "observer contract", nil); stop {
				t.Fatal("read_skill unexpectedly stopped the run")
			}
			for _, toolErr := range []error{nil, errors.New("idempotency conflict"), errors.New("write denied")} {
				_, stop := hooks.After[0](context.Background(), agentcore.ToolCall{Name: "submit_recommendation"}, "receipt", toolErr)
				if stop {
					t.Fatalf("submit_recommendation stopped %s run after error %v; conflict re-read and notification require a follow-up turn", trigger, toolErr)
				}
			}
		})
	}
}

type submitBoundaryTool struct {
	calls int
}

func (t *submitBoundaryTool) Name() string { return "submit_recommendation" }
func (t *submitBoundaryTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: t.Name(), Parameters: map[string]any{"type": "object"}}
}
func (t *submitBoundaryTool) Run(context.Context, string) (string, error) {
	t.calls++
	return "persisted finding", nil
}

func TestOrdinaryAgentSubmitRecommendationKeepsTerminalBoundary(t *testing.T) {
	for _, trigger := range []string{"scheduled", "manual"} {
		t.Run(trigger, func(t *testing.T) {
			_, hooks := buildToolsAndHooks(BuildParams{Trigger: trigger}, "ordinary-marketer-scope")
			tool := &submitBoundaryTool{}
			provider := agentcore.NewFauxProvider(
				agentcore.AssistantToolCall("one", tool.Name(), `{"title":"first"}`),
				agentcore.AssistantToolCall("two", tool.Name(), `{"title":"second"}`),
				agentcore.AssistantText("done"),
			)
			agent, err := agentcore.New(agentcore.Config{
				Provider: provider, Model: "test", Tools: agentcore.NewToolSet(tool),
				Policy: agentcore.NewAllowList(tool.Name()), Hooks: hooks,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := agent.Prompt(context.Background(), "File the recommendation"); err != nil {
				t.Fatal(err)
			}
			if tool.calls != 1 || len(provider.Recorded) != 1 {
				t.Fatalf("ordinary %s run changed: writes=%d requests=%d, want 1/1", trigger, tool.calls, len(provider.Recorded))
			}
		})
	}
}

type lohiObserverRetryNotifier struct {
	attempts atomic.Int32
}

func (n *lohiObserverRetryNotifier) Notify(_ context.Context, ch storage.AlertChannel, title, body string) error {
	if ch.Name != "ops" || title != "Lohi data gap" || !strings.Contains(body, "2026-10-03") {
		return fmt.Errorf("unexpected notification: channel=%q title=%q body=%q", ch.Name, title, body)
	}
	if n.attempts.Add(1) == 1 {
		return errors.New("delivery failed in test")
	}
	return nil
}

func TestLohiObserverScheduledRunPersistsDeliveryFailureAndRetry(t *testing.T) {
	databaseURL := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL to run the scheduled observer integration")
	}
	t.Setenv("AGENT_KEY_ENC_SECRET", "lohi-observer-integration-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var skillID string
	notifier := &lohiObserverRetryNotifier{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawRequest, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read provider request: %v", readErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
			return
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Name    string `json:"name"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(rawRequest, &request); err != nil {
			t.Errorf("decode provider request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		tools := map[string]bool{}
		for _, tool := range request.Tools {
			tools[tool.Function.Name] = true
		}
		for _, required := range []string{"read_skill", "list_findings", "submit_recommendation", "send_notification"} {
			if !tools[required] {
				t.Errorf("scheduled observer request did not advertise %s", required)
			}
		}

		results := map[string]string{}
		var all strings.Builder
		for _, message := range request.Messages {
			all.WriteString(message.Content)
			all.WriteByte('\n')
			if message.Role == "tool" {
				results[message.Name] = message.Content
			}
		}
		history := all.String()
		writeTool := func(id, name, args string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
						"id": id, "type": "function", "function": map[string]string{"name": name, "arguments": args},
					}}},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 2},
			})
		}
		writeText := func(text string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message":       map[string]string{"role": "assistant", "content": text},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 2},
			})
		}

		switch {
		case results["read_skill"] == "":
			if !strings.Contains(history, lohiObserverVersion) || !strings.Contains(history, "daily_completeness") || !strings.Contains(history, "Authorized delivery channel: ops") {
				t.Errorf("scheduled prompt did not expose the configured observer skill and mode")
			}
			writeTool("read-observer", "read_skill", fmt.Sprintf(`{"id":%q}`, skillID))
		case results["list_findings"] == "":
			if !strings.Contains(results["read_skill"], "retryable delivery obligation") || !strings.Contains(results["read_skill"], "at-least-once retry") {
				t.Errorf("loaded observer skill omitted repairable delivery contract: %q", results["read_skill"])
			}
			writeTool("list-findings", "list_findings", `{"limit":50}`)
		case results["submit_recommendation"] == "" && !strings.Contains(results["list_findings"], "Lohi data gap"):
			writeTool("submit-finding", "submit_recommendation", `{"category":"data","title":"Lohi data gap","rationale":"Required input is stale for the closed HCM day.","evidence":{"observation_key":{"project":"current","definition_version":"lohi-evidence-v1","period":"2026-10-03","condition":"data_quality:stale_input"},"range":"2026-10-03 Asia/Ho_Chi_Minh","warnings":["stale input"]},"idempotency_key":"8bcf79bd1688b254880320d94b55bf6a1a7bb770ba87d9e64872939c472a29b4"}`)
		case results["send_notification"] == "":
			writeTool("deliver-finding", "send_notification", `{"channel":"ops","title":"Lohi data gap","body":"2026-10-03 data_quality:stale_input; see the persisted finding."}`)
		case notifier.attempts.Load() == 1:
			writeText("Finding recorded; delivery failed and remains eligible for a later overlap/retry.")
		default:
			writeText("Existing finding delivery retry sent to the authorized channel.")
		}
	}))
	defer server.Close()

	st, err := storage.Open(ctx, config.Config{
		PostgresURL: databaseURL,
		DuckDBPath:  filepath.Join(t.TempDir(), "lohi-observer.duckdb"),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)

	stamp := time.Now().UnixNano()
	boot, err := st.CreateAccount(ctx,
		fmt.Sprintf("lohi-observer-%d@example.com", stamp),
		"Lohi observer", "password1234", "Observer workspace", "Observer project",
	)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if _, err := st.UpsertWorkspaceModelTiers(ctx, boot.User.ID, boot.Workspace.ID, storage.WorkspaceModelTiersInput{
		Provider: "openai", Model: "observer-test", BaseURL: server.URL, APIKey: "test-key",
	}); err != nil {
		t.Fatalf("configure model: %v", err)
	}
	if _, err := st.UpsertAgentConfig(ctx, boot.User.ID, boot.Project.ID, storage.AgentConfigInput{
		Enabled: true, RedactPII: true, Autonomy: storage.AutonomyScheduled,
		Scopes: map[string]bool{"monitor": true, "data_quality": true, "analyze_build": true, "growth_suggest": true},
	}); err != nil {
		t.Fatalf("configure agent: %v", err)
	}
	observerBody, err := os.ReadFile(filepath.Join("..", "workloads", "config", lohiObserverVersion, "SKILL.md"))
	if err != nil {
		t.Fatalf("read embedded observer skill source: %v", err)
	}
	installed, err := st.UpsertAgentSkill(ctx, boot.User.ID, boot.Project.ID, "", storage.AgentSkill{
		Name:        lohiObserverVersion,
		Description: "Run gated daily completeness and weekly mature-cohort observations over lohi-evidence-v1 using existing AgentRay schedules, Plans, and delivery.",
		Body:        string(observerBody), Enabled: true,
	})
	if err != nil {
		t.Fatalf("install observer skill: %v", err)
	}
	skillID = installed.ID
	if _, err := st.CreateAlertChannel(ctx, boot.User.ID, boot.Workspace.ID, storage.AlertChannel{
		Kind: "webhook", Name: "ops", Config: json.RawMessage(`{"url":"https://unused.invalid"}`),
	}); err != nil {
		t.Fatalf("create delivery channel: %v", err)
	}

	var manifest struct {
		Schedules []struct {
			Mode           string `json:"mode"`
			PromptTemplate string `json:"prompt_template"`
		} `json:"schedules"`
	}
	manifestRaw, err := os.ReadFile(filepath.Join("..", "workloads", "config", lohiObserverVersion, "manifest.json"))
	if err != nil {
		t.Fatalf("read embedded observer manifest source: %v", err)
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	prompt := manifest.Schedules[0].PromptTemplate + " Authorized delivery channel: ops."
	if manifest.Schedules[0].Mode != "daily_completeness" || prompt == "" {
		t.Fatalf("unexpected observer schedule: %+v", manifest.Schedules[0])
	}

	runner := NewRunner(st, WithNotifier(notifier), WithSessionStore(NewSessionStore(st)))
	first, _, err := runner.Run(ctx, RunOptions{ProjectID: boot.Project.ID, Trigger: "scheduled", Prompt: prompt})
	if err != nil {
		t.Fatalf("first scheduled run: %v", err)
	}
	second, _, err := runner.Run(ctx, RunOptions{ProjectID: boot.Project.ID, Trigger: "scheduled", Prompt: prompt})
	if err != nil {
		t.Fatalf("retry scheduled run: %v", err)
	}
	if notifier.attempts.Load() != 2 {
		t.Fatalf("notification attempts = %d, want failed first attempt plus successful retry", notifier.attempts.Load())
	}

	findings, _, err := st.ListRecommendationsPage(ctx, boot.Project.ID, "", 50)
	if err != nil || len(findings) != 1 {
		t.Fatalf("persisted findings = %d, err=%v, want one deduplicated finding", len(findings), err)
	}
	assertRun := func(runID, wantSummary string, wantTools []string) {
		t.Helper()
		run, calls, err := st.GetAgentRun(ctx, boot.User.ID, boot.Project.ID, runID)
		if err != nil {
			t.Fatalf("GetAgentRun(%s): %v", runID, err)
		}
		if run.Trigger != "scheduled" || run.Status != "done" || run.FinishedAt == nil || !strings.Contains(run.Summary, wantSummary) {
			t.Fatalf("persisted run = %+v, want complete scheduled status containing %q", run, wantSummary)
		}
		var got []string
		for _, call := range calls {
			got = append(got, call.Tool)
		}
		if strings.Join(got, ",") != strings.Join(wantTools, ",") {
			t.Fatalf("persisted tools = %v, want %v", got, wantTools)
		}
	}
	assertRun(first.ID, "delivery failed", []string{"read_skill", "list_findings", "submit_recommendation", "send_notification"})
	assertRun(second.ID, "delivery retry sent", []string{"read_skill", "list_findings", "send_notification"})
}
