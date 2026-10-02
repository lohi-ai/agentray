//go:build pi

package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
)

func piChildSSE(w http.ResponseWriter, tool, args, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	delta := map[string]any{"role": "assistant", "content": text}
	reason := "stop"
	if tool != "" {
		delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "reused-call", "type": "function", "function": map[string]any{"name": tool, "arguments": args}}}}
		reason = "tool_calls"
	}
	body := map[string]any{"id": "native-child", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}}
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", piSessionJSON(body))
}

func TestPiRunnerForksUseNativeProviderIsolationAndGovernance(t *testing.T) {
	ctx := piSessionContext(t)
	var parents, children, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []json.RawMessage
			Tools    []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw := string(piSessionJSON(body.Messages))
		isParent := strings.Contains(raw, "PARENT-ONLY")
		hasSpawn := false
		for _, tool := range body.Tools {
			if tool.Function.Name == subagent.ToolSpawnSubagent {
				hasSpawn = true
			}
		}
		if isParent {
			if !hasSpawn {
				t.Error("parent lost delegation tool")
			}
			if parents.Add(1) == 1 {
				piChildSSE(w, subagent.ToolSpawnSubagent, `{"task":"isolated-child-task"}`, "")
			} else {
				piChildSSE(w, "", "", "parent answer")
			}
		} else {
			if !strings.Contains(raw, "isolated-child-task") || strings.Contains(raw, "parent private history") {
				t.Errorf("child inherited wrong context: %s", raw)
			}
			if hasSpawn {
				t.Error("child advertised delegation beyond its depth cap")
			}
			if children.Add(1) == 1 {
				piChildSSE(w, "write", `{}`, "")
			} else {
				piChildSSE(w, "", "", "child answer")
			}
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{piComposedTool{&effects}}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-fork", BaseURL: server.URL + "/v1", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY", NativeHistory: json.RawMessage(`[{"role":"user","content":"parent private history","timestamp":1}]`)}, tier, nil)
	if err != nil {
		t.Fatal(err)
	}
	if parents.Load() != 2 || children.Load() != 2 || effects.Load() != 1 || result.Final != "parent answer" {
		t.Fatalf("native fork failed: parent=%d child=%d effects=%d final=%q", parents.Load(), children.Load(), effects.Load(), result.Final)
	}
	if result.Usage.InputTokens != 28 || result.Usage.OutputTokens != 12 {
		t.Fatalf("child usage missing or counted twice: %+v", result.Usage)
	}
	memory := p.Session.(*agentcore.MemorySessionStore)
	sessions := memory.Sessions()
	var childID string
	for _, id := range sessions {
		if id != p.SessionID {
			if childID != "" {
				t.Fatalf("extra child session: %v", sessions)
			}
			childID = id
		}
	}
	if childID == "" {
		t.Fatal("native fork was not durable")
	}
	log, err := memory.Log(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	childResult, done, err := piCompletedChild(log)
	if err != nil || !done || childResult.Final != "child answer" || childResult.NativeRevision != result.NativeRevision {
		t.Fatalf("child completion missing: %+v %v", childResult, err)
	}
	if strings.Contains(string(childResult.NativeState), "PARENT-ONLY") || !strings.Contains(string(childResult.NativeState), `"toolResult"`) {
		t.Fatal("child lost isolated native tool transcript")
	}
	// Reattach through the same consumer callback: no process/provider/tool is
	// needed, no historical spend is charged, and the request must match.
	worker, known, err := tier.BindPi(agentcore.PiConfig{Worker: piSessionWorker(t)}, PiModelOptions{MaxTokens: p.MaxTokens})
	if err != nil {
		t.Fatal(err)
	}
	fork := piForkRunner(worker, known, p.Session, p.ToolChoice, nil)
	parent, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	req := subagent.ForkRequest{SessionID: childID, Prompt: "isolated-child-task", Task: "isolated-child-task"}
	attached, err := fork(agentcore.WithDelegationDepth(ctx, 1), parent.Fork(childID), req, nil)
	if err != nil || attached.StopReason != "reattached" || attached.Usage.InputTokens != 0 || parents.Load() != 2 || children.Load() != 2 || effects.Load() != 1 {
		t.Fatalf("reattach repeated native work: %+v %v", attached, err)
	}

	// A settled tool result is sufficient to continue an interrupted child; the
	// physical tool must not be repeated or billed as new work.
	resumedStore := agentcore.NewMemorySessionStore()
	cut := false
	for _, entry := range log {
		if err := resumedStore.Append(ctx, childID, entry); err != nil {
			t.Fatal(err)
		}
		var event struct {
			Type    string
			Message struct{ Role string }
		}
		if entry.Kind == piEventEntry && json.Unmarshal([]byte(entry.Content), &event) == nil && event.Type == "message_end" && event.Message.Role == "toolResult" {
			cut = true
			break
		}
	}
	if !cut {
		t.Fatal("missing recoverable tool-result boundary")
	}
	resumedParams := p
	resumedParams.Session = resumedStore
	resumedParent, err := Build(resumedParams)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := piForkRunner(worker, known, resumedStore, p.ToolChoice, nil)(agentcore.WithDelegationDepth(ctx, 1), resumedParent.Fork(childID), req, nil)
	if err != nil || resumed.Final != "child answer" || effects.Load() != 1 || children.Load() != 3 || resumed.Usage.InputTokens != 7 {
		t.Fatalf("resume repeated settled work or lost current usage: %+v %v effects=%d requests=%d", resumed, err, effects.Load(), children.Load())
	}
	// Losing the final host receipt is ambiguous: the child must not infer host
	// approval or repeat the native run after agent_end.
	missingReceipt := agentcore.NewMemorySessionStore()
	for _, entry := range log[:len(log)-1] {
		if err := missingReceipt.Append(ctx, childID, entry); err != nil {
			t.Fatal(err)
		}
	}
	missingParams := p
	missingParams.Session = missingReceipt
	missingParent, err := Build(missingParams)
	if err != nil {
		t.Fatal(err)
	}
	_, err = piForkRunner(worker, known, missingReceipt, p.ToolChoice, nil)(ctx, missingParent.Fork(childID), req, nil)
	if err == nil || !strings.Contains(err.Error(), "completion receipt") || children.Load() != 3 {
		t.Fatalf("ambiguous completion repeated work: %v", err)
	}
	req.Prompt = "changed request"
	if _, err := fork(ctx, parent.Fork(childID), req, nil); err == nil {
		t.Fatal("reused child session accepted a different request")
	}
}

func TestPiRunnerChildSchemaRetryKeepsOriginalNativeTranscript(t *testing.T) {
	ctx := piSessionContext(t)
	var parents, children atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw := string(piSessionJSON(body.Messages))
		if strings.Contains(raw, "PARENT-ONLY") {
			if parents.Add(1) == 1 {
				piChildSSE(w, subagent.ToolSpawnSubagent, `{"task":"return a JSON value","output_schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}`, "")
			} else {
				if !strings.Contains(raw, `\"ok\":true`) || strings.Contains(raw, "validation failed:") {
					t.Errorf("parent did not receive corrected child output: %s", raw)
				}
				piChildSSE(w, "", "", "parent done")
			}
		} else if children.Add(1) == 1 {
			piChildSSE(w, "", "", "invalid JSON answer")
		} else {
			if !strings.Contains(raw, "invalid JSON answer") || !strings.Contains(raw, "output_schema validation") {
				t.Errorf("retry lost prior answer or correction: %s", raw)
			}
			piChildSSE(w, "", "", `{"ok":true}`)
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-fork", BaseURL: server.URL + "/v1", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
	if err != nil || result.Final != "parent done" || parents.Load() != 2 || children.Load() != 2 || result.Usage.InputTokens != 28 {
		t.Fatalf("native child retry failed: %+v %v (%d/%d)", result, err, parents.Load(), children.Load())
	}
	memory := p.Session.(*agentcore.MemorySessionStore)
	var original, retry agentcore.RunResult
	for _, id := range memory.Sessions() {
		if id == p.SessionID {
			continue
		}
		entries, err := memory.Log(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		child, done, err := piCompletedChild(entries)
		if err != nil || !done {
			t.Fatalf("child not completed: %s %v", id, err)
		}
		if strings.HasSuffix(id, "/retry") {
			retry = child
		} else {
			original = child
		}
	}
	if original.Final != "invalid JSON answer" || retry.Final != `{"ok":true}` {
		t.Fatalf("missing original/retry: %q/%q", original.Final, retry.Final)
	}
	var seed struct{ Messages json.RawMessage }
	_ = json.Unmarshal(original.NativeState, &seed)
	if _, err := piConversationSuffix(seed.Messages, retry.NativeState); err != nil {
		t.Fatalf("corrective fork reconstructed native messages: %v", err)
	}
}

func TestPiRunnerRepeatedProviderCallIDsCreateDistinctChildren(t *testing.T) {
	ctx := piSessionContext(t)
	var parents, children atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.Contains(string(piSessionJSON(body.Messages)), "PARENT-ONLY") {
			if parents.Add(1) <= 2 {
				piChildSSE(w, subagent.ToolSpawnSubagent, `{"task":"same task in a new invocation"}`, "")
			} else {
				piChildSSE(w, "", "", "finished both")
			}
		} else {
			children.Add(1)
			piChildSSE(w, "", "", "child complete")
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-fork", BaseURL: server.URL + "/v1", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
	if err != nil || result.Final != "finished both" || children.Load() != 2 || parents.Load() != 3 {
		t.Fatalf("provider ID reused a prior child: %+v %v requests=%d/%d", result, err, parents.Load(), children.Load())
	}
	if ids := p.Session.(*agentcore.MemorySessionStore).Sessions(); len(ids) != 3 {
		t.Fatalf("physical invocations shared a child log: %v", ids)
	}
}

func TestPiRunnerCancellationReachesNativeChild(t *testing.T) {
	ctx, cancel := context.WithCancel(piSessionContext(t))
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return
		}
		if strings.Contains(string(piSessionJSON(body.Messages)), "PARENT-ONLY") {
			piChildSSE(w, subagent.ToolSpawnSubagent, `{"task":"wait for cancellation"}`, "")
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-fork", BaseURL: server.URL + "/v1", APIKey: "test"}}
	done := make(chan error, 1)
	go func() {
		_, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("child never started: %v", err)
	case <-ctx.Done():
		t.Fatal("child start timed out")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled native child reported success")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("parent cancellation did not settle native child")
	}
	memory := p.Session.(*agentcore.MemorySessionStore)
	for _, id := range memory.Sessions() {
		log, err := memory.Log(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range log {
			if entry.Kind == agentcore.EntryPiChildResult {
				t.Fatal("cancelled child recorded a successful completion")
			}
		}
		checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
		_, free, err := agentcore.AcquireSessionLease(checkCtx, memory, id)
		if err != nil {
			stop()
			t.Fatalf("child leaked its session lease: %v", err)
		}
		_ = free()
		stop()
	}
}

func TestPiForkKeepsInheritedPermissionHook(t *testing.T) {
	ctx := piSessionContext(t)
	var effects, hooks, requests atomic.Int32
	parent, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("legacy must not run")), Model: "test",
		Tools: agentcore.NewToolSet(piComposedTool{&effects}), Policy: agentcore.NewAllowList("write"),
		Hooks: agentcore.Hooks{Before: []agentcore.BeforeToolCall{func(context.Context, agentcore.ToolCall) agentcore.Decision {
			hooks.Add(1)
			return agentcore.Decision{Allow: false, Reason: "inherited denial"}
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := agentcore.PiConfig{Worker: piSessionWorker(t), Options: piSessionOptions(), Callback: func(_ context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		return piSessionReply(requests.Add(1) == 1), nil
	}}
	result, err := piForkRunner(worker, true, nil, agentcore.ToolChoice{}, nil)(ctx, parent.Fork(""), subagent.ForkRequest{Prompt: "try write", Task: "try write"}, nil)
	if err != nil || result.Final != "done" || requests.Load() != 2 || hooks.Load() != 1 || effects.Load() != 0 || !strings.Contains(string(result.NativeState), "inherited denial") {
		t.Fatalf("native child widened inherited permissions: %+v %v requests=%d hooks=%d effects=%d", result, err, requests.Load(), hooks.Load(), effects.Load())
	}
}

func TestPiRunnerParallelChildrenHaveSeparateSessions(t *testing.T) {
	ctx := piSessionContext(t)
	var parents, children atomic.Int32
	bothStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw := string(piSessionJSON(body.Messages))
		if strings.Contains(raw, "PARENT-ONLY") {
			if parents.Add(1) == 1 {
				calls := []any{}
				for i, task := range []string{"child-left", "child-right"} {
					calls = append(calls, map[string]any{"index": i, "id": task, "type": "function", "function": map[string]any{"name": subagent.ToolSpawnSubagent, "arguments": string(piSessionJSON(map[string]any{"task": task}))}})
				}
				response := map[string]any{"id": "parallel", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", piSessionJSON(response))
			} else {
				piChildSSE(w, "", "", "parallel done")
			}
		} else {
			left, right := strings.Contains(raw, "child-left"), strings.Contains(raw, "child-right")
			if left == right {
				t.Errorf("siblings shared input context: %s", raw)
			}
			if children.Add(1) == 2 {
				close(bothStarted)
			}
			select {
			case <-bothStarted:
				piChildSSE(w, "", "", "child complete")
			case <-ctx.Done():
				t.Error("native children did not run concurrently")
			}
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-fork", BaseURL: server.URL + "/v1", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
	if err != nil || result.Final != "parallel done" || parents.Load() != 2 || children.Load() != 2 {
		t.Fatalf("parallel forks failed: %+v %v requests=%d/%d", result, err, parents.Load(), children.Load())
	}
	if ids := p.Session.(*agentcore.MemorySessionStore).Sessions(); len(ids) != 3 {
		t.Fatalf("parallel children shared a session: %v", ids)
	}
}
