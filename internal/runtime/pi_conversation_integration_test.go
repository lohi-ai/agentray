//go:build pi

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

func TestPiConversationNativeProviderRoundTripAndBranches(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL for native conversation SQL verification")
	}
	ctx := piSessionContext(t)
	st, err := storage.Open(ctx, config.Config{PostgresURL: url, DuckDBPath: filepath.Join(t.TempDir(), "conversation.duckdb")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx, "pi-history-"+uuid.NewString()+"@test.local", "Pi history", "password1234", "Pi history", "Pi history")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, boot.User.ID, boot.Project.ID, "", "Native history")
	if err != nil {
		t.Fatal(err)
	}
	var calls, effects atomic.Int32
	var mu sync.Mutex
	var include, exclude []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		raw := string(piSessionJSON(payload.Messages))
		mu.Lock()
		for _, want := range include {
			if !strings.Contains(raw, want) {
				t.Errorf("provider context missing %q: %s", want, raw)
			}
		}
		for _, unwanted := range exclude {
			if strings.Contains(raw, unwanted) {
				t.Errorf("abandoned context %q leaked: %s", unwanted, raw)
			}
		}
		mu.Unlock()
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "data: "+`{"id":"tool","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"history-call","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprintf(w, "data: {\"id\":\"text\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"answer-%d\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"total_tokens\":11}}\n\n", n)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "history", BaseURL: server.URL + "/v1", APIKey: "test"}}
	runTurn := func(prompt string, want, unwanted []string) (PiConversationHistory, storage.AgentConversationEntry) {
		t.Helper()
		mu.Lock()
		include, exclude = want, unwanted
		mu.Unlock()
		base, err := BuildPiHistory(ctx, st, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		user, err := AppendMessageEntryAtLeaf(ctx, st, conv.ID, "user", prompt, "", boot.User.ID, base.LeafID)
		if err != nil {
			t.Fatal(err)
		}
		p := representativeBuildParams()
		p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
		p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
		p.Tools = []agentcore.Tool{piComposedTool{&effects}}
		result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: prompt, InputID: user.ID, NativeHistory: base.Messages, NativeHistoryRevision: base.Revision}, tier, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.NativeRevision == "" {
			t.Fatal("native revision missing")
		}
		if err := appendPiConversationTurn(ctx, st, conv.ID, "", "", base, result); err != nil {
			t.Fatal(err)
		}
		display, err := appendMessageAtLeaf(ctx, st, conv.ID, "assistant", result.Final, "", "", "", result.Turns, false, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		history, err := BuildPiHistory(ctx, st, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		var native struct{ Messages json.RawMessage }
		if err := json.Unmarshal(result.NativeState, &native); err != nil {
			t.Fatal(err)
		}
		if !samePiJSON(history.Messages, native.Messages) {
			t.Fatalf("SQL round trip changed native messages:\n%s\n%s", history.Messages, native.Messages)
		}
		if _, err := BuildHistory(ctx, st, conv.ID); err == nil {
			t.Fatal("legacy reducer accepted native conversation")
		}
		summarized := false
		_, err = CompactConversationNow(ctx, st, conv.ID, 1, func(context.Context, string, string) (string, error) { summarized = true; return "lossy", nil })
		if err == nil || summarized {
			t.Fatal("legacy compaction reached native conversation")
		}
		return history, display
	}
	first, firstDisplay := runTurn("first-question", []string{"first-question"}, nil)
	runTurn("second-question", []string{"first-question", "history-call", "answer-2", "second-question"}, nil)
	if effects.Load() != 1 {
		t.Fatal("history replay executed a physical tool again")
	}
	if err := st.SetConversationLeaf(ctx, conv.ID, firstDisplay.ID); err != nil {
		t.Fatal(err)
	}
	runTurn("fork-question", []string{"first-question", "answer-2", "fork-question"}, []string{"second-question", "answer-3"})
	// A stale result must not overwrite a newer completed native turn.
	stale := agentcore.RunResult{NativeRevision: first.Revision, NativeState: piSessionJSON(map[string]any{"messages": first.Messages})}
	if err := appendPiConversationTurn(ctx, st, conv.ID, "", "", first, stale); !errors.Is(err, storage.ErrConversationLeafChanged) {
		t.Fatalf("stale publication: %v", err)
	}
	if _, err := AppendClearEntry(ctx, st, conv.ID, ""); err != nil {
		t.Fatal(err)
	}
	runTurn("after-clear", []string{"after-clear"}, []string{"first-question", "fork-question", "history-call", "answer-2"})
	if calls.Load() != 5 {
		t.Fatalf("unexpected provider requests: %d", calls.Load())
	}
	// Forking back before the first input shares the root anchor but must still
	// reject a response whose triggering input has left the active branch.
	other, err := st.CreateConversation(ctx, boot.User.ID, boot.Project.ID, "", "Root fork")
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildPiHistory(ctx, st, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := AppendMessageEntryAtLeaf(ctx, st, other.ID, "user", "abandoned", "", boot.User.ID, base.LeafID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetConversationLeaf(ctx, other.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendMessageEntry(ctx, st, other.ID, "user", "replacement", "", boot.User.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	abandoned := agentcore.RunResult{NativeRevision: first.Revision, NativeState: piSessionJSON(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "abandoned", "agentrayInputId": original.ID}}})}
	if err := appendPiConversationTurn(ctx, st, other.ID, "", "", base, abandoned); !errors.Is(err, storage.ErrConversationLeafChanged) {
		t.Fatalf("abandoned root turn published: %v", err)
	}
	t.Run("late input and answer display", func(t *testing.T) {
		pendingConv, err := st.CreateConversation(ctx, boot.User.ID, boot.Project.ID, "", "Late input")
		if err != nil {
			t.Fatal(err)
		}
		base, err := BuildPiHistory(ctx, st, pendingConv.ID)
		if err != nil {
			t.Fatal(err)
		}
		user, err := AppendMessageEntryAtLeaf(ctx, st, pendingConv.ID, "user", "task", "", boot.User.ID, base.LeafID)
		if err != nil {
			t.Fatal(err)
		}
		late, err := AppendMessageEntry(ctx, st, pendingConv.ID, "user", "queued after final drain", "", boot.User.ID, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		native := []json.RawMessage{piSessionJSON(map[string]any{"role": "user", "content": "task", "agentrayInputId": user.ID}), json.RawMessage(`{"role":"assistant","content":[]}`)}
		result := agentcore.RunResult{NativeRevision: first.Revision, NativeState: piSessionJSON(map[string]any{"messages": native})}
		if err := appendPiConversationTurn(ctx, st, pendingConv.ID, "", "", base, result); err != nil {
			t.Fatal(err)
		}
		pending, err := BuildPiHistory(ctx, st, pendingConv.ID)
		if err != nil {
			t.Fatal(err)
		}
		messages := piConvMessages(t, pending)
		if len(messages) != 3 || !strings.Contains(string(messages[2]), late.ID) {
			t.Fatalf("late input disappeared from SQL history: %s", pending.Messages)
		}
		// A continuation cannot replace a pending user message with an unrelated
		// durable session state. The normal next run can consume that seeded input.
		resume, err := buildPiResumeHistory(ctx, st, pendingConv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := piConversationSuffix(resume.Messages, result.NativeState); err == nil {
			t.Fatal("resume silently dropped pending input")
		}
		messages = append(messages, json.RawMessage(`{"role":"assistant","content":[]}`))
		result.NativeState = piSessionJSON(map[string]any{"messages": messages})
		if err := appendPiConversationTurn(ctx, st, pendingConv.ID, "", "", pending, result); err != nil {
			t.Fatal(err)
		}
		checkpoint, err := buildPiResumeHistory(ctx, st, pendingConv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := appendMessageAtLeaf(ctx, st, pendingConv.ID, "user", "displayed human answer", "", boot.User.ID, "", 0, false, nil, true); err != nil {
			t.Fatal(err)
		}
		resumed, err := buildPiResumeHistory(ctx, st, pendingConv.ID)
		if err != nil || resumed.LeafID != checkpoint.LeafID || !samePiJSON(resumed.Messages, checkpoint.Messages) {
			t.Fatalf("answer display polluted resume prefix: %+v %v", resumed, err)
		}
		answer := json.RawMessage(`{"role":"user","content":"native human answer","agentrayAnswerId":"receipt","timestamp":456}`)
		result.NativeState = piSessionJSON(map[string]any{"messages": append(messages, answer)})
		if err := appendPiConversationTurn(ctx, st, pendingConv.ID, "", "", resumed, result); err != nil {
			t.Fatal(err)
		}
		final, err := BuildPiHistory(ctx, st, pendingConv.ID)
		if err != nil || len(piConvMessages(t, final)) != 5 || strings.Contains(string(final.Messages), "displayed human answer") {
			t.Fatalf("resumed answer duplicated: %s %v", final.Messages, err)
		}
	})

}

func TestPiConversationRevisionMismatchBeforeProvider(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Error(w, "unexpected", 500) }))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "history", BaseURL: server.URL + "/v1", APIKey: "test"}}
	_, err := runner.runModelLoop(piSessionContext(t), p, RunOptions{Prompt: "next", NativeHistory: json.RawMessage(`[]`), NativeHistoryRevision: "wrong-revision"}, tier, nil)
	if err == nil || !strings.Contains(err.Error(), "revision") || requests.Load() != 0 {
		t.Fatalf("mismatched revision reached provider: %v (%d)", err, requests.Load())
	}
}
