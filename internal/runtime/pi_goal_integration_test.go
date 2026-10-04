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

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

type piNestedGoalTool struct{}

func (piNestedGoalTool) Name() string { return "revise_nested" }
func (piNestedGoalTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: "revise_nested", Parameters: map[string]any{"type": "object"}}
}
func (piNestedGoalTool) Run(ctx context.Context, _ string) (string, error) {
	invoker, ok := agentcore.ToolInvokerFrom(ctx)
	if !ok {
		return "", fmt.Errorf("missing nested tool bridge")
	}
	for _, next := range []string{"intermediate condition", "revised condition", "revised condition"} {
		if _, err := invoker.InvokeTool(ctx, goal.ToolName, string(piSessionJSON(map[string]string{"goal": next, "reason": "new evidence"}))); err != nil {
			return "", err
		}
	}
	// Even a plain outer tool that discards its nested output cannot discard
	// the completion-contract commits.
	return "nested revisions complete", nil
}

func piGoalHost(t *testing.T, ctx context.Context, store agentcore.SessionStore, id, condition string, tools ...agentcore.Tool) (*agentcore.PiToolHost, *goal.Store) {
	t.Helper()
	plugin, trail := goal.UntilRevisable(condition)
	a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test", Goal: condition,
		Session: store, SessionID: id, Policy: agentcore.NewAllowList(goal.ToolName, "revise_nested"), Tools: agentcore.NewToolSet(tools...), Extensions: []agentcore.ExtensionFactory{plugin}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host, trail
}

func piGoalReply(tool, condition string) json.RawMessage {
	var reply map[string]any
	_ = json.Unmarshal(piSessionReply(tool != ""), &reply)
	if tool != "" {
		reply["content"] = []any{map[string]any{"type": "thinking", "thinking": "evidence", "thinkingSignature": "opaque-goal-signature"}, map[string]any{"type": "toolCall", "id": "reused", "name": tool, "arguments": map[string]string{"goal": condition, "reason": "new evidence"}}}
	} else {
		reply["content"] = []any{map[string]any{"type": "text", "text": "finished\n" + goal.Done}}
	}
	return piSessionJSON(reply)
}

func TestPiGoalRevisionNativeHistoryAndFreshWorkerResume(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			checkPiGoalResume(t, agentcore.NewMemorySessionStore(), "goal", nested)
		})
	}
}

func checkPiGoalResume(t *testing.T, store agentcore.SessionStore, sessionID string, nested bool) {
	t.Helper()
	ctx := piSessionContext(t)
	var calls atomic.Int32
	for attempt := 0; attempt < 2; attempt++ {
		condition := "original condition"
		if attempt == 1 {
			entries, _ := store.Log(ctx, sessionID)
			var err error
			condition, _, err = piStoredGoal(entries)
			if err != nil || condition != "revised condition" {
				t.Fatalf("cannot recover revised condition: %q %v", condition, err)
			}
		}
		host, trail := piGoalHost(t, ctx, store, sessionID, condition, piNestedGoalTool{})
		result, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("original requirement"), Session: PiSessionConfig{
			Store: store, SessionID: sessionID, Resume: attempt == 1, ReviseGoal: attempt == 0, Policy: agentcore.NewAllowList(goal.ToolName, "revise_nested"), Pi: agentcore.PiConfig{Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "stream" {
					return nil, fmt.Errorf("unexpected %s", method)
				}
				n := calls.Add(1)
				var request struct {
					Context struct {
						Messages []struct {
							Role     string
							Sections map[string]string
						}
					}
				}
				if err := json.Unmarshal(params, &request); err != nil {
					return nil, err
				}
				var current string
				for _, message := range request.Context.Messages {
					if section, ok := message.Sections["agentray"]; ok {
						current = section
					}
				}
				want := "original condition"
				if n > 1 {
					want = "revised condition"
				}
				if !nested && n == 2 {
					want = "intermediate condition"
				}
				if !strings.Contains(current, want) {
					t.Errorf("request %d has stale goal: %s", n, current)
				}
				if !nested && n <= 3 {
					condition := "revised condition"
					if n == 1 {
						condition = "intermediate condition"
					}
					return piGoalReply(goal.ToolName, condition), nil
				}
				if n == 1 {
					tool := goal.ToolName
					if nested {
						tool = "revise_nested"
					}
					return piGoalReply(tool, "revised condition"), nil
				}
				return piGoalReply("", ""), nil
			}},
		}})
		_ = host.Close()
		if err != nil {
			t.Fatal(err)
		}
		if trail.Goal() != "revised condition" || !strings.Contains(string(result.State), "original condition") || !strings.Contains(string(result.State), "opaque-goal-signature") {
			t.Fatalf("revision lost store/original history: goal=%s state=%s", trail.Goal(), result.State)
		}
	}
	entries, _ := store.Log(ctx, sessionID)
	var commits []piGoalRevision
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiGoalRevision {
			var revision piGoalRevision
			_ = json.Unmarshal([]byte(entry.Content), &revision)
			commits = append(commits, revision)
		}
	}
	wantRequests := int32(5)
	if nested {
		wantRequests = 3
	}
	if len(commits) != 2 || calls.Load() != wantRequests {
		t.Fatalf("repeated or missing effects: commits=%+v requests=%d", commits, calls.Load())
	}
	if nested && (!strings.Contains(commits[0].SourceCallID, "/bridge-") || commits[0].EffectID != commits[1].EffectID) {
		t.Fatalf("lost nested effect attribution: %+v", commits)
	}
	if !nested && commits[0].EffectID == commits[1].EffectID {
		t.Fatal("reused provider call ID reused a physical effect")
	}
	if _, err := recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
}

func TestPiGoalRevisionFailureCannotChangeContractOrContinue(t *testing.T) {
	for _, kind := range []string{"disabled", "denied", "write failure"} {
		t.Run(kind, func(t *testing.T) {
			ctx := piSessionContext(t)
			var store agentcore.SessionStore = agentcore.NewMemorySessionStore()
			if kind == "write failure" {
				store = &piFailStore{MemorySessionStore: agentcore.NewMemorySessionStore(), kind: agentcore.EntryPiGoalRevision}
			}
			host, trail := piGoalHost(t, ctx, store, "failure", "original condition")
			var calls atomic.Int32
			var policy agentcore.Policy = agentcore.NewAllowList(goal.ToolName)
			if kind == "denied" {
				policy = agentcore.DenyAll{}
			}
			_, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("work"), Session: PiSessionConfig{Store: store, SessionID: "failure", ReviseGoal: kind != "disabled", Policy: policy, Pi: agentcore.PiConfig{Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
				if calls.Add(1) == 1 {
					return piGoalReply(goal.ToolName, "revised condition"), nil
				}
				return piGoalReply("", ""), nil
			}}}})
			if kind == "write failure" {
				if err == nil || !strings.Contains(err.Error(), "write failure") || calls.Load() != 1 {
					t.Fatalf("failed commit continued: %v calls=%d", err, calls.Load())
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if trail.Goal() != "original condition" || len(trail.Revisions()) != 1 {
				t.Fatal("uncommitted change reached gate")
			}
			entries, _ := store.Log(ctx, "failure")
			for _, entry := range entries {
				if entry.Kind == agentcore.EntryPiGoalRevision {
					t.Fatal("refused update recorded a revision")
				}
			}
		})
	}
}

func TestPiRunnerGoalRevisionUsesOriginalProviderAndRestoresContract(t *testing.T) {
	ctx := piSessionContext(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := calls.Add(1)
		if n > 1 {
			var system string
			for _, message := range body.Messages {
				if message.Role == "system" || message.Role == "developer" {
					system += string(message.Content)
				}
			}
			if !strings.Contains(system, "revised condition") || strings.Contains(system, "original condition") {
				t.Errorf("provider got stale system sections: %s", system)
			}
		}
		if n == 1 {
			piChildSSE(w, goal.ToolName, `{"goal":"revised condition","reason":"new evidence"}`, "")
		} else {
			piChildSSE(w, "", "", "finished\n"+goal.Done)
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.PrepareNextTurn, p.RefreshKey = nil, nil
	p.Goal, p.ReviseGoal = "original condition", true
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-goal", BaseURL: server.URL + "/v1", APIKey: "test-key"}}
	if _, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "work"}, tier, nil); err != nil {
		t.Fatal(err)
	}
	p.Goal, p.ReviseGoal = "", false
	if _, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "continue", ResumeFromRunID: p.SessionID}, tier, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("unexpected requests: %d", calls.Load())
	}
	p.Goal = "original condition"
	if _, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "continue", ResumeFromRunID: p.SessionID}, tier, nil); err == nil || !strings.Contains(err.Error(), "durable goal") {
		t.Fatalf("resume overwrote revised contract: %v", err)
	}
}

func TestPiGoalRevisionPostgresResume(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL for native goal recovery")
	}
	ctx := piSessionContext(t)
	st, err := storage.Open(ctx, config.Config{PostgresURL: url, DuckDBPath: filepath.Join(t.TempDir(), "goal.duckdb")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx, "native-goal-"+uuid.NewString()+"@test.local", "Native goal", "password1234", "Native goal", "Native goal")
	if err != nil {
		t.Fatal(err)
	}
	for _, nested := range []bool{false, true} {
		run, err := st.CreateAgentRun(ctx, boot.Project.ID, "", "manual", "")
		if err != nil {
			t.Fatal(err)
		}
		t.Run(fmt.Sprint(nested), func(t *testing.T) { checkPiGoalResume(t, NewSessionStore(st), run, nested) })
	}
}

func TestPiGoalCommitCannotRecoverMissingCompletionReceipt(t *testing.T) {
	ctx := piSessionContext(t)
	store := &piFailStore{MemorySessionStore: agentcore.NewMemorySessionStore(), kind: agentcore.EntryPiEffectDone}
	host, trail := piGoalHost(t, ctx, store, "missing-receipt", "original condition")
	var calls atomic.Int32
	_, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("work"), Session: PiSessionConfig{Store: store, SessionID: "missing-receipt", ReviseGoal: true, Policy: agentcore.NewAllowList(goal.ToolName), Pi: agentcore.PiConfig{Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		calls.Add(1)
		return piGoalReply(goal.ToolName, "revised condition"), nil
	}}}})
	if err == nil || calls.Load() != 1 || trail.Goal() != "revised condition" {
		t.Fatalf("wrong crash boundary: err=%v requests=%d goal=%s", err, calls.Load(), trail.Goal())
	}
	entries, _ := store.Log(ctx, "missing-receipt")
	if condition, _, err := piStoredGoal(entries); err != nil || condition != "revised condition" {
		t.Fatalf("lost committed revision: %s %v", condition, err)
	}
	if _, err := recoverPiState(entries); err == nil || !strings.Contains(err.Error(), "unsettled") {
		t.Fatalf("goal commit made effect replayable: %v", err)
	}
}
