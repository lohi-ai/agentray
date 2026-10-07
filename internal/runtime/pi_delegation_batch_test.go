package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2found/2ai/agentcore"
	nativehost "github.com/2found/2ai/agentcore/host"
	"github.com/2found/2ai/agentcore/plugins/ask"
	"github.com/2found/2ai/agentcore/plugins/subagent"
	"github.com/2found/2ai/ai"
)

type nativeBatchExtension struct {
	terminal bool
	wantIDs  []string
	calls    atomic.Int32
	t        *testing.T
}

func (*nativeBatchExtension) Name() string { return "batch-resume-test" }
func (e *nativeBatchExtension) BeginRun(context.Context, agentcore.RunInfo) (agentcore.Extension, error) {
	return e, nil
}
func (e *nativeBatchExtension) InterceptToolResult(ctx context.Context, call agentcore.ToolCall, _ string, err error) agentcore.ToolResultDecision {
	if agentcore.DelegationDepth(ctx) != 0 || err != nil || call.Name == "ask" {
		return agentcore.ToolResultDecision{}
	}
	return agentcore.ToolResultDecision{Terminate: e.terminal && call.Name == "write", AdditionalContexts: []agentcore.Message{{Role: agentcore.RoleUser, Content: "context for " + call.ID}}}
}
func (e *nativeBatchExtension) InterceptBatch(ctx context.Context, calls []agentcore.ToolCall) agentcore.BatchDecision {
	if agentcore.DelegationDepth(ctx) != 0 {
		return agentcore.BatchDecision{}
	}
	var ids []string
	for _, call := range calls {
		ids = append(ids, call.ID)
	}
	want := e.wantIDs
	if want == nil {
		want = []string{"spawn-two", "ordinary", "local-question", "spawn-one"}
	}
	if !reflect.DeepEqual(ids, want) {
		e.t.Errorf("batch lost source calls/order: %v", ids)
	}
	count := e.calls.Add(1)
	calls[0].ID = "must not change source history"
	return agentcore.BatchDecision{AdditionalContexts: []agentcore.Message{{Role: agentcore.RoleUser, Content: fmt.Sprintf("batch decision %d", count)}}}
}

func TestNativeDelegationBatchCommitsBeforeDelivery(t *testing.T) {
	for _, localOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("localOnly=%v", localOnly), func(t *testing.T) {
			testNativeParkedBatch(t, localOnly)
		})
	}
}

func testNativeParkedBatch(t *testing.T, localOnly bool) {
	for _, fault := range []string{"", "before batch receipt", "before delivery", "after delivery", "terminal checkpoint"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			terminal := fault == "terminal checkpoint"
			store := &nativeDelegationFaultStore{MemorySessionStore: agentcore.NewMemorySessionStore(), parent: "parent", mode: fault}
			ext := &nativeBatchExtension{terminal: terminal, t: t}
			if localOnly {
				ext.wantIDs = []string{"ask-two", "ordinary", "local-question", "ask-one"}
			}
			var writes atomic.Int32
			var mu sync.Mutex
			parents := 0
			children := map[string]int{}
			runtime := PiSessionConfig{Pi: NativeAgentConfig{Options: json.RawMessage(`{"initialState":{}}`)}}
			runtime.Pi.Callback = func(ctx context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "stream" {
					return nil, fmt.Errorf("unexpected method %s", method)
				}
				mu.Lock()
				defer mu.Unlock()
				var stream *ai.AssistantMessageEventStream
				if agentcore.DelegationDepth(ctx) == 0 {
					parents++
					if parents == 1 {
						stream = nativeChildResponse(nativeChildThinking("original-signature"),
							nativeChildCall("spawn-two", subagent.ToolSpawnSubagent, `{"task":"second child","context":"background"}`),
							nativeChildCall("ordinary", "write", `{}`),
							nativeChildCall("local-question", "ask", `{"question":"Local scope?"}`),
							nativeChildCall("spawn-one", subagent.ToolSpawnSubagent, `{"task":"first child"}`))
						if localOnly {
							stream = nativeChildResponse(nativeChildThinking("original-signature"),
								nativeChildCall("ask-two", "ask", `{"question":"Second scope?"}`),
								nativeChildCall("ordinary", "write", `{}`),
								nativeChildCall("local-question", "ask", `{"question":"Local scope?"}`),
								nativeChildCall("ask-one", "ask", `{"question":"First scope?"}`))
						}
					} else {
						if terminal {
							t.Error("terminal sibling scheduled a model request")
						}
						var request struct {
							Context struct {
								Messages []struct {
									Role    string
									Content json.RawMessage
								}
							}
						}
						if err := json.Unmarshal(params, &request); err != nil {
							t.Fatal(err)
						}
						var delivered []string
						for _, m := range request.Context.Messages {
							var text string
							if m.Role == "user" && json.Unmarshal(m.Content, &text) == nil && (strings.HasPrefix(text, "context for ") || strings.HasPrefix(text, "batch decision ")) {
								delivered = append(delivered, text)
							}
						}
						want := []string{"context for spawn-two", "context for ordinary", "context for spawn-one", fmt.Sprintf("batch decision %d", ext.calls.Load())}
						if localOnly {
							want = []string{"context for ordinary", fmt.Sprintf("batch decision %d", ext.calls.Load())}
						}
						if !reflect.DeepEqual(delivered, want) {
							t.Errorf("lost/repeated/out-of-order contexts: %v", delivered)
						}
						stream = nativeChildResponse(ai.ContentBlock{Type: "text", Text: "parent final"})
					}
				} else {
					id := agentcore.RunSessionFrom(ctx)
					children[id]++
					if children[id] == 1 {
						stream = nativeChildResponse(nativeChildCall("ask", "ask", `{"question":"Child scope?"}`))
					} else {
						stream = nativeChildResponse(ai.ContentBlock{Type: "text", Text: "child final"})
					}
				}
				message, err := stream.Result(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(message)
			}
			plugin := &subagent.Plugin{RunFork: piForkRunner(runtime, true, store, agentcore.ToolChoice{}, nil)}
			policy := agentcore.NewAllowList("ask", "write", subagent.ToolSpawnSubagent)
			a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(), Model: "test", Session: store, SessionID: "parent", Tools: agentcore.NewToolSet(ask.Tool{}, nativeChildWrite{&writes}), Policy: policy, Extensions: []agentcore.ExtensionFactory{plugin, ext}})
			if err != nil {
				t.Fatal(err)
			}
			run := func(resume bool) (PiRunResult, error) {
				host, err := a.OpenPiTools(ctx)
				if err != nil {
					return PiRunResult{}, err
				}
				defer host.Close()
				cfg := runtime
				cfg.Store, cfg.SessionID, cfg.Resume, cfg.Policy = store, "parent", resume, policy
				var input json.RawMessage
				if !resume {
					input = json.RawMessage(`"parent task"`)
				}
				return RunPi(ctx, PiRunConfig{Host: host, Session: cfg, Input: input, PricingKnown: true})
			}
			result, err := run(false)
			if err != nil || !result.Projection.Parked || ext.calls.Load() != 0 {
				t.Fatalf("initial batch: %+v %v", result, err)
			}
			for answered := 0; answered < 3; answered++ {
				entries, _ := store.Log(ctx, "parent")
				id, _, pending := agentcore.PendingQuestion(entries)
				if !pending {
					t.Fatal("missing sibling question")
				}
				lease, release, err := agentcore.AcquireSessionLease(ctx, store, "parent")
				if err != nil {
					t.Fatal(err)
				}
				_, err = agentcore.RecordSessionAnswer(lease, store, "parent", id, "approved")
				_ = release()
				if err != nil {
					t.Fatal(err)
				}
				result, err = run(true)
				if answered < 2 {
					if err != nil || !result.Projection.Parked || ext.calls.Load() != 0 || parents != 1 {
						t.Fatalf("batch ran before all answers: %+v %v", result, err)
					}
				} else if fault != "" {
					if err == nil || !store.hit {
						t.Fatalf("fault did not fire: %v", err)
					}
					if fault == "before batch receipt" {
						cfg := runtime
						cfg.Store, cfg.SessionID, cfg.Resume, cfg.Policy = store, "parent", true, policy
						session, err := NewPiSession(ctx, cfg)
						if err != nil {
							t.Fatal(err)
						}
						err = session.Continue(ctx)
						_ = session.Close()
						if !errors.Is(err, ErrPiChildResumeRequired) {
							t.Fatalf("direct resume bypassed uncommitted batch: %v", err)
						}
					}
					result, err = run(true)
				}
			}
			wantHooks, wantParents := int32(1), 2
			if fault == "before batch receipt" {
				wantHooks = 2
			} // No committed hook result exists yet.
			if terminal {
				wantHooks, wantParents = 0, 1
			}
			wantChildren := 2
			if localOnly {
				wantChildren = 0
			}
			if err != nil || result.Projection.Parked || ext.calls.Load() != wantHooks || parents != wantParents || writes.Load() != 1 || len(children) != wantChildren {
				t.Fatalf("resume: %+v %v hooks=%d parents=%d writes=%d children=%v", result, err, ext.calls.Load(), parents, writes.Load(), children)
			}
			for id, requests := range children {
				if requests != 2 {
					t.Errorf("child %s reran: %d", id, requests)
				}
			}
			if !strings.Contains(string(result.State), "original-signature") || strings.Contains(string(result.State), "must not change source history") {
				t.Fatal("hook changed native source history")
			}
			if terminal && (result.Projection.StopReason != "toolUse" || strings.Contains(string(result.State), `"agentrayDelegationContextId"`) || strings.Contains(string(result.State), `"agentrayDelegationBatchContextId"`)) {
				t.Fatalf("terminal batch leaked context: %s", result.State)
			}
			entries, _ := store.Log(ctx, "parent")
			starts := 0
			for _, entry := range entries {
				if entry.Kind == agentcore.EntryPiEvent {
					var event struct{ Type string }
					_ = json.Unmarshal([]byte(entry.Content), &event)
					if event.Type == "tool_execution_start" {
						starts++
					}
				}
			}
			if starts != 4 {
				t.Fatalf("batch continuation fabricated native tool events: %d", starts)
			}
			batches, err := piDelegationBatches(entries, result.State)
			if err != nil || len(batches) != 1 || batches[0].Receipt == nil || !batches[0].Delivered {
				t.Fatalf("batch receipt missing: %+v %v", batches, err)
			}
			if pending, err := piAnswerMessages(entries, result.State); err != nil || len(pending) != 0 {
				t.Fatalf("batch left pending deliveries: %s %v", pending, err)
			}
			if localOnly {
				checkPiLocalBatchCorruption(t, entries, result.State)
			} else {
				checkPiBatchCorruption(t, entries, result.State)
			}
		})
	}
}

func checkPiBatchCorruption(t *testing.T, entries []agentcore.SessionEntry, state json.RawMessage) {
	t.Helper()
	index := -1
	for i, e := range entries {
		if e.Kind == agentcore.EntryPiDelegationBatch {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("no batch to validate")
	}
	var receipt piDelegationBatchReceipt
	_ = json.Unmarshal([]byte(entries[index].Content), &receipt)
	var native struct{ Messages []json.RawMessage }
	_ = json.Unmarshal(state, &native)
	for _, mutate := range []func(){
		func() { native.Messages = append(native.Messages, receipt.Messages[0]) },
		func() {
			for i, raw := range native.Messages {
				if nativehost.SameJSON(raw, receipt.Messages[0]) {
					native.Messages[i] = json.RawMessage(strings.Replace(string(raw), "child final", "changed final", 1))
					break
				}
			}
		},
	} {
		_ = json.Unmarshal(state, &native)
		mutate()
		if _, err := piAnswerMessages(entries, piModelJSON(native)); err == nil {
			t.Fatal("repeated/changed native batch delivery accepted")
		}
	}
	// Every delivery prefix can recover the exact pending suffix without
	// rerunning a hook, including the gap between primary and context messages.
	_ = json.Unmarshal(state, &native)
	var base []json.RawMessage
	for _, raw := range native.Messages {
		var m struct{ AgentrayDelegationID, AgentrayDelegationContextID, AgentrayDelegationBatchContextID string }
		_ = json.Unmarshal(raw, &m)
		if m.AgentrayDelegationID == "" && m.AgentrayDelegationContextID == "" && m.AgentrayDelegationBatchContextID == "" {
			base = append(base, raw)
		}
	}
	for delivered := 0; delivered <= len(receipt.Messages); delivered++ {
		messages := append(append([]json.RawMessage(nil), base...), receipt.Messages[:delivered]...)
		pending, err := piAnswerMessages(entries, piModelJSON(map[string]any{"messages": messages}))
		if err != nil || len(pending) != len(receipt.Messages)-delivered {
			t.Fatalf("delivery prefix %d: pending=%d err=%v", delivered, len(pending), err)
		}
		for i, raw := range pending {
			if !nativehost.SameJSON(raw, receipt.Messages[delivered+i]) {
				t.Fatal("pending batch message changed during recovery")
			}
		}
	}
	// SQL JSONB may reorder the original tool argument object keys.
	_ = json.Unmarshal(state, &native)
	for i, raw := range native.Messages {
		var m struct {
			Role    string
			Content []map[string]json.RawMessage
		}
		if json.Unmarshal(raw, &m) != nil || m.Role != "assistant" {
			continue
		}
		changed := false
		for _, block := range m.Content {
			if string(block["id"]) == `"spawn-two"` {
				block["arguments"] = json.RawMessage(`{"context":"background","task":"second child"}`)
				changed = true
			}
		}
		if changed {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			fields["content"] = piModelJSON(m.Content)
			native.Messages[i] = piModelJSON(fields)
		}
	}
	if _, err := piAnswerMessages(entries, piModelJSON(native)); err != nil {
		t.Fatalf("JSONB ordering changed batch identity: %v", err)
	}
	// Old sessions may have fully delivered completion receipts without batch
	// metadata. They must not retroactively run new hooks or alter old history.
	legacy := append([]agentcore.SessionEntry(nil), entries[:index]...)
	legacy = append(legacy, entries[index+1:]...)
	if batches, err := piDelegationBatches(legacy, state); err != nil || len(batches) != 1 || !batches[0].Delivered || batches[0].Receipt != nil {
		t.Fatalf("fully delivered legacy batch was not retained: %+v %v", batches, err)
	}
	// A partially delivered old batch cannot safely infer which hook decision
	// produced the missing deliveries.
	partial := append(append([]json.RawMessage(nil), base...), receipt.Messages[0])
	if _, err := piDelegationBatches(legacy, piModelJSON(map[string]any{"messages": partial})); err == nil {
		t.Fatal("ambiguous partially delivered batch accepted")
	}
	for _, mutate := range []func(*piDelegationBatchReceipt){
		func(r *piDelegationBatchReceipt) { r.ID = "orphan" },
		func(r *piDelegationBatchReceipt) { r.Origins[0] = "other" },
		func(r *piDelegationBatchReceipt) { r.Completions[0] = "old" },
		func(r *piDelegationBatchReceipt) { r.CallsDigest = "bad" },
		func(r *piDelegationBatchReceipt) { r.Timestamp = 0 },
		func(r *piDelegationBatchReceipt) { r.End = !r.End },
		func(r *piDelegationBatchReceipt) { r.Messages = r.Messages[1:] },
	} {
		var receipt piDelegationBatchReceipt
		_ = json.Unmarshal([]byte(entries[index].Content), &receipt)
		mutate(&receipt)
		bad := append([]agentcore.SessionEntry(nil), entries...)
		bad[index].Content = string(piModelJSON(receipt))
		if _, err := piAnswerMessages(bad, state); err == nil {
			t.Fatalf("corrupt batch accepted: %+v", receipt)
		}
	}
	bad := append(append([]agentcore.SessionEntry(nil), entries...), entries[index])
	if _, err := piAnswerMessages(bad, state); err == nil {
		t.Fatal("duplicate batch receipt accepted")
	}
	bad = append([]agentcore.SessionEntry{entries[index]}, entries[:index]...)
	bad = append(bad, entries[index+1:]...)
	if _, err := piAnswerMessages(bad, state); err == nil {
		t.Fatal("batch before its completions accepted")
	}
}

func checkPiLocalBatchCorruption(t *testing.T, entries []agentcore.SessionEntry, state json.RawMessage) {
	t.Helper()
	index := -1
	for i, entry := range entries {
		if entry.Kind == agentcore.EntryPiDelegationBatch {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("missing local batch receipt")
	}
	for _, mutate := range []func(*piDelegationBatchReceipt){
		func(r *piDelegationBatchReceipt) { r.Origins[0] = "other" },
		func(r *piDelegationBatchReceipt) { r.Completions[0] = "other" },
		func(r *piDelegationBatchReceipt) { r.End = !r.End },
		func(r *piDelegationBatchReceipt) { r.CallsDigest = "changed" },
	} {
		var receipt piDelegationBatchReceipt
		_ = json.Unmarshal([]byte(entries[index].Content), &receipt)
		mutate(&receipt)
		bad := append([]agentcore.SessionEntry(nil), entries...)
		bad[index].Content = string(piModelJSON(receipt))
		if _, err := piAnswerMessages(bad, state); err == nil {
			t.Fatal("corrupt local batch accepted")
		}
	}
	legacy := append([]agentcore.SessionEntry(nil), entries[:index]...)
	legacy = append(legacy, entries[index+1:]...)
	if batches, err := piDelegationBatches(legacy, state); err != nil || len(batches) != 1 || !batches[0].Delivered || batches[0].Receipt != nil {
		t.Fatalf("fully delivered legacy local batch: %+v %v", batches, err)
	}
	var native struct{ Messages []json.RawMessage }
	_ = json.Unmarshal(state, &native)
	var partial []json.RawMessage
	removed := false
	for _, raw := range native.Messages {
		var header struct{ AgentrayAnswerID string }
		_ = json.Unmarshal(raw, &header)
		if !removed && header.AgentrayAnswerID != "" {
			removed = true
			continue
		}
		partial = append(partial, raw)
	}
	partialState := piModelJSON(map[string]any{"messages": partial})
	if _, err := piDelegationBatches(legacy, partialState); err == nil {
		t.Fatal("partial local delivery without receipt accepted")
	}
	if pending, err := piAnswerMessages(entries, partialState); err != nil || len(pending) != 1 {
		t.Fatalf("committed local answer cannot recover: %s %v", pending, err)
	}
	for i, raw := range native.Messages {
		var result struct {
			Role    string
			Details agentcore.PiToolOutcome
		}
		if json.Unmarshal(raw, &result) != nil || result.Role != "toolResult" || !result.Details.Parked {
			continue
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		result.Details.AdditionalContexts = []agentcore.Message{{Role: agentcore.RoleUser, Content: "forged context"}}
		fields["details"] = piModelJSON(result.Details)
		native.Messages[i] = piModelJSON(fields)
		if _, err := piDelegationBatches(entries, piModelJSON(native)); err == nil {
			t.Fatal("changed original local outcome accepted")
		}
		return
	}
	t.Fatal("missing original local outcome")
}
