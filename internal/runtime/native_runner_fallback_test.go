package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/telemetry/llm"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
)

func TestNativeRunnerFallbackCompactionToolAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var effects, primary, fallback, summaryPrimary, summaryFallback atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Tools    []struct{ Function struct{ Name string } }
			Messages json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer "+body.Model+"-row-fresh" {
			t.Error("runner used another provider row's key")
		}
		summary := strings.HasPrefix(body.Model, "summary-")
		if summary || body.Model == "primary" {
			if len(body.Tools) != 0 {
				t.Error("tool-free model advertised tools")
			}
		} else {
			found := false
			for _, tool := range body.Tools {
				found = found || tool.Function.Name == "write"
			}
			if !found {
				t.Error("tool-free primary erased fallback's host catalogue")
			}
			if !strings.Contains(string(body.Messages), "Earlier work summary") {
				t.Error("parent lost compacted request view")
			}
		}
		switch body.Model {
		case "primary", "summary-primary":
			if summary {
				summaryPrimary.Add(1)
			} else {
				primary.Add(1)
			}
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"model unavailable"}}`))
			return
		case "summary-fallback":
			summaryFallback.Add(1)
		case "fallback":
			fallback.Add(1)
		default:
			t.Error("unexpected model")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if !summary && fallback.Load() == 1 {
			_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"write-1","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`+"\n\ndata: [DONE]\n\n")
			return
		}
		text := "finished"
		if summary {
			text = "Earlier evidence was checked. Continue the task."
		}
		payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 3}})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	defer server.Close()
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", ProviderID: "primary-row", Model: "primary", BaseURL: server.URL, APIKey: "stale-a", Capabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported}, Fallback: &TierConfig{Provider: "openai", ProviderID: "fallback-row", Model: "fallback", BaseURL: server.URL, APIKey: "stale-b"}}}
	summaryTier := ModelTier{TierConfig: TierConfig{Provider: "openai", ProviderID: "summary-primary-row", Model: "summary-primary", BaseURL: server.URL, APIKey: "stale-summary-a", Fallback: &TierConfig{Provider: "openai", ProviderID: "summary-fallback-row", Model: "summary-fallback", BaseURL: server.URL, APIKey: "stale-summary-b"}}}
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{nativeChildWrite{&effects}}
	p.MaxContextTokens, p.KeepRecentTokens = 1500, 500
	p.PiCompactionTier = &summaryTier
	p.RefreshProviderKey = func(_ context.Context, id, vendor, endpoint string) (string, error) {
		if vendor != "openai" || endpoint != server.URL {
			return "", fmt.Errorf("unexpected credential route")
		}
		return id + "-fresh", nil
	}
	var mu sync.Mutex
	var traces []llm.TraceRecord
	p.Tracer = llm.SinkFunc(func(record llm.TraceRecord) { mu.Lock(); defer mu.Unlock(); traces = append(traces, record) })
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "write and finish", NativeHistory: piRequestJSON(piLongRequest())}, tier, nil)
	if err != nil || result.Final != "finished" || effects.Load() != 1 || primary.Load() != 1 || fallback.Load() != 2 || summaryPrimary.Load() != 1 || summaryFallback.Load() != 1 {
		t.Fatal("runner fallback/summary did not complete", err, result.StopReason, effects.Load(), primary.Load(), fallback.Load(), summaryPrimary.Load(), summaryFallback.Load())
	}
	if !strings.Contains(string(result.NativeState), `"opaque":0`) {
		t.Fatal("compaction replaced native history")
	}
	mu.Lock()
	input, summaryTraces := 0, 0
	for _, trace := range traces {
		input += trace.Usage.InputTokens
		if strings.HasPrefix(trace.Model, "summary-") {
			summaryTraces++
			if trace.SessionKey == p.SessionID {
				t.Error("summary shared parent trace session")
			}
		}
	}
	traceCount := len(traces)
	mu.Unlock()
	if traceCount != 5 || summaryTraces != 2 || input != result.Usage.InputTokens || input != 5 {
		t.Fatal("trace/usage duplicated or missing", traceCount, summaryTraces, input, result.Usage.InputTokens)
	}
	result, err = runner.runModelLoop(ctx, p, RunOptions{Prompt: "continue", ResumeFromRunID: p.SessionID}, tier, nil)
	if err != nil || result.Final != "finished" || primary.Load() != 1 || fallback.Load() != 3 || effects.Load() != 1 {
		t.Fatal("resume lost selected fallback or repeated effects", err)
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	selections := 0
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiModelSelection {
			selections++
		}
	}
	if selections != 1 {
		t.Fatal("auxiliary selection entered parent journal or resume reselected", selections)
	}
	if _, err = recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
	changed := tier
	copy := *tier.Fallback
	copy.BaseURL += "/changed"
	changed.Fallback = &copy
	if _, err = runner.runModelLoop(ctx, p, RunOptions{Prompt: "continue", ResumeFromRunID: p.SessionID}, changed, nil); err == nil {
		t.Fatal("resume silently migrated provider route")
	}
	if primary.Load() != 1 || fallback.Load() != 3 {
		t.Fatal("changed identity reached provider")
	}
	_, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil {
		t.Fatal("runner leaked lease", err)
	}
	_ = release()
}

func TestNativeLadderOptionsRejectAmbiguousVendorRefresh(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", ProviderID: "a", Model: "primary", APIKey: "a", Fallback: &TierConfig{Provider: "openai", ProviderID: "b", Model: "fallback", APIKey: "b"}}}
	opts := PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { t.Error("binding refreshed a key"); return "", nil }}
	if _, err := (BuildParams{}).nativeLadderOptions(tier, opts); err == nil {
		t.Fatal("ambiguous vendor-only callback accepted")
	}
	p := BuildParams{RefreshProviderKey: func(context.Context, string, string, string) (string, error) {
		t.Error("binding refreshed a row")
		return "", nil
	}}
	if _, err := p.nativeLadderOptions(tier, opts); err != nil {
		t.Fatal(err)
	}
	tier.Fallback.ProviderID = "a"
	if _, err := (BuildParams{}).nativeLadderOptions(tier, opts); err != nil {
		t.Fatal("same-row refresh rejected", err)
	}
	tier.Fallback.Provider, tier.Fallback.ProviderID = "anthropic", "b"
	if _, err := (BuildParams{}).nativeLadderOptions(tier, opts); err != nil {
		t.Fatal("unambiguous vendor refresh rejected", err)
	}
}

func TestNativeRunnerFallbackChildOwnsSelection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var parentPrimary, childPrimary, parentFallback, childFallback atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Messages json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		parent := strings.Contains(string(body.Messages), "PARENT-ONLY")
		if !parent && !strings.Contains(string(body.Messages), "isolated-child-task") {
			t.Error("child lost its task")
		}
		if r.Header.Get("Authorization") != "Bearer "+body.Model {
			t.Error("child inherited parent's selected credential")
		}
		if body.Model == "primary" {
			if parent {
				parentPrimary.Add(1)
			} else {
				childPrimary.Add(1)
			}
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if parent && parentFallback.Add(1) == 1 {
			payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "spawn-1", "type": "function", "function": map[string]string{"name": subagent.ToolSpawnSubagent, "arguments": `{"task":"isolated-child-task"}`}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 3}})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
			return
		}
		answer := "parent answer"
		if !parent {
			childFallback.Add(1)
			answer = "child answer"
		}
		payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 3}})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	defer server.Close()
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", ProviderID: "a", Model: "primary", BaseURL: server.URL, APIKey: "primary", Fallback: &TierConfig{Provider: "openai", ProviderID: "b", Model: "fallback", BaseURL: server.URL, APIKey: "fallback"}}}
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = nil
	var traceMu sync.Mutex
	traceCount, traceInput := 0, 0
	p.Tracer = llm.SinkFunc(func(record llm.TraceRecord) {
		traceMu.Lock()
		defer traceMu.Unlock()
		traceCount++
		traceInput += record.Usage.InputTokens
	})
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
	if err != nil || result.Final != "parent answer" || parentPrimary.Load() != 1 || childPrimary.Load() != 1 || parentFallback.Load() != 2 || childFallback.Load() != 1 {
		t.Fatal("parent/child ladder ownership failed", err, parentPrimary.Load(), childPrimary.Load(), parentFallback.Load(), childFallback.Load())
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	if traceCount != 5 || traceInput != 6 || result.Usage.InputTokens != 6 {
		t.Fatal("child usage or attempt traces duplicated", traceCount, traceInput, result.Usage.InputTokens)
	}
	store := p.Session.(*agentcore.MemorySessionStore)
	if len(store.Sessions()) != 2 {
		t.Fatal("child did not own a durable session")
	}
	for _, id := range store.Sessions() {
		entries, err := store.Log(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		selections := 0
		for _, entry := range entries {
			if entry.Kind == agentcore.EntryPiModelSelection {
				selections++
			}
		}
		if selections != 1 {
			t.Fatal("parent and child overwrote selection generations", selections)
		}
		if _, err = recoverPiState(entries); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeRunnerFallbackPersistenceFailurePreventsToolEffect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "primary" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"write-1","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "primary", APIKey: "a", BaseURL: server.URL, FallbackModel: "fallback"}}
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{nativeChildWrite{&effects}}
	p.Session = &nativeSelectionFailStore{MemorySessionStore: agentcore.NewMemorySessionStore(), selectionError: errors.New("selection journal unavailable")}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	if _, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "write"}, tier, nil); err == nil || !strings.Contains(err.Error(), "selection journal unavailable") {
		t.Fatal("selection write failure lost", err)
	}
	if effects.Load() != 0 {
		t.Fatal("effect executed without durable model selection")
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiEffectStart || entry.Kind == agentcore.EntryPiModelSelection {
			t.Fatal("failed selection admitted an effect")
		}
	}
	_, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil {
		t.Fatal("failed request leaked session lease", err)
	}
	_ = release()
}
