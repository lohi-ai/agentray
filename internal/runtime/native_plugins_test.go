package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
	"github.com/lohi-ai/agentray/agentcore/plugins/advisor"
	"github.com/lohi-ai/agentray/agentcore/plugins/memory"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeMemoryTodoAndAdvisorShareComposedLifecycle(t *testing.T) {
	ctx := piSessionContext(t)
	memories := newScopedMemory()
	_ = memories.Remember(ctx, agentcore.MemoryEntry{ScopeID: "agent", Content: "remembered evidence", Kind: agentcore.MemoryLearning})
	plan := todo.NewStore()
	reviews, requests := 0, 0
	review := advisor.Of(func(_ context.Context, r advisor.Review) ([]advisor.Note, error) {
		reviews++
		raw, _ := json.Marshal(r.Messages)
		for _, want := range []string{"original task", "remembered evidence", "new durable lesson", "verify result"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("advisor did not receive request evidence %q: %s", want, raw)
			}
		}
		if r.Round != reviews-1 {
			t.Errorf("review round=%d calls=%d", r.Round, reviews)
		}
		if len(r.Messages) > 0 {
			r.Messages[0].Content = "observer must not rewrite native history"
		}
		if reviews == 1 {
			return []advisor.Note{{Severity: advisor.SeverityConcern, Text: "verify evidence before finishing"}}, nil
		}
		if !strings.Contains(string(raw), "verify evidence before finishing") {
			t.Error("advisor correction missing from next request")
		}
		return nil, nil
	})
	a, err := agentcore.Build(agentcore.ConfigPlugin(agentcore.Config{
		Provider: agentcore.NewFauxProvider(), Model: "native-fixture", Definition: agentcore.AgentDefinition{ScopeID: "agent"},
		Policy: agentcore.NewAllowList(memory.ToolLearn, todo.ToolName),
	}), memory.Plugin{Store: memories}, todo.Plugin{Store: plan}, review)
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	result, err := RunPi(ctx, PiRunConfig{Host: host, Task: "original task", Input: piSessionJSON("original task"), Session: PiSessionConfig{
		Policy: agentcore.NewAllowList(memory.ToolLearn, todo.ToolName), Pi: NativeAgentConfig{Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method != "stream" {
				return nil, fmt.Errorf("unexpected callback %s", method)
			}
			requests++
			if !strings.Contains(string(params), "remembered evidence") {
				t.Error("memory recall missing from native prompt")
			}
			reply := map[string]any{}
			_ = json.Unmarshal(piSessionReply(false), &reply)
			switch requests {
			case 1:
				reply["stopReason"] = "toolUse"
				reply["content"] = []any{map[string]any{"type": "thinking", "thinking": "learn", "thinkingSignature": "native-signature"}, map[string]any{"type": "toolCall", "id": "learn-1", "name": memory.ToolLearn, "arguments": map[string]any{"lesson": "new durable lesson"}}}
			case 2:
				reply["stopReason"] = "toolUse"
				reply["content"] = []any{map[string]any{"type": "toolCall", "id": "plan-1", "name": todo.ToolName, "arguments": map[string]any{"items": []any{map[string]any{"content": "verify result", "status": "completed"}}}}}
			default:
				if strings.Count(string(params), todo.ContextPrefix) != 1 {
					t.Error("native todo context missing or repeated")
				}
				reply["content"] = []any{map[string]any{"type": "text", "text": "finished"}}
			}
			return json.Marshal(reply)
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 4 || reviews != 2 {
		t.Fatal("plugin lifecycle lost", requests, reviews)
	}
	learned, _ := memories.Recall(ctx, "agent", "", 10)
	if len(learned) != 2 || learned[1].Content != "new durable lesson" || len(plan.List()) != 1 {
		t.Fatal("plugin state lost", learned, plan.List())
	}
	if !strings.Contains(string(result.State), "native-signature") || strings.Contains(string(result.State), "observer must not rewrite") || strings.Contains(string(result.State), todo.ContextPrefix) {
		t.Fatal("observer projection changed native transcript", string(result.State))
	}
}

type nativeRequestObserverProbe struct {
	phases   []agentcore.ObservePhase
	requests []string
}

func (*nativeRequestObserverProbe) Name() string { return "native-observer-probe" }
func (p *nativeRequestObserverProbe) BeginRun(context.Context, agentcore.RunInfo) (agentcore.Extension, error) {
	return p, nil
}
func (p *nativeRequestObserverProbe) ObserveMessages(_ context.Context, phase agentcore.ObservePhase, _ int, messages []agentcore.Message) {
	p.phases = append(p.phases, phase)
	if phase == agentcore.PhaseRequest {
		raw, _ := json.Marshal(messages)
		p.requests = append(p.requests, string(raw))
	}
}

func TestNativeCompactionRebasesObserversBeforeProviderRequest(t *testing.T) {
	ctx := piSessionContext(t)
	probe := &nativeRequestObserverProbe{}
	a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(), Model: "fixture", Extensions: []agentcore.ExtensionFactory{probe}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	calls := 0
	source := []any{map[string]any{"role": "user", "content": strings.Repeat("old context ", 2000), "timestamp": 1}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "old answer"}}, "stopReason": "stop", "timestamp": 2}, map[string]any{"role": "user", "content": "current request", "timestamp": 3}}
	result, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON(source), Compaction: &nativehost.CompactionPolicy{Budget: 1200, KeepRecent: 10, Summarize: func(context.Context, json.RawMessage, string) (string, agentcore.Usage, error) {
		calls++
		return "compacted evidence", agentcore.Usage{InputTokens: 7}, nil
	}}, Session: PiSessionConfig{Pi: NativeAgentConfig{Callback: func(_ context.Context, _ string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if !strings.Contains(string(params), "compacted evidence") {
			t.Error("compacted request missing")
		}
		return piSessionReply(false), nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(probe.requests) != 1 || !strings.Contains(probe.requests[0], "compacted evidence") {
		t.Fatal("observer saw pre-compaction context", calls, len(probe.requests))
	}
	rebase, request := -1, -1
	for i, phase := range probe.phases {
		if phase == agentcore.PhaseRebase {
			rebase = i
		}
		if phase == agentcore.PhaseRequest {
			request = i
		}
	}
	if rebase < 0 || request <= rebase {
		t.Fatal("observer ordering", probe.phases)
	}
	if strings.Contains(string(result.State), "agentrayContextSummary") || !strings.Contains(string(result.State), "old context ") {
		t.Fatal("compaction rewrote native checkpoint")
	}
}

func TestNativeRequestObserverFailureNeverRetriesOrFallsBack(t *testing.T) {
	for _, panicObserver := range []bool{false, true} {
		t.Run(fmt.Sprint(panicObserver), func(t *testing.T) {
			ctx := piSessionContext(t)
			observed, opened, transport := 0, 0, 0
			cause := &agentcore.ProviderError{Status: 503, Message: "host observer failed"}
			agent, err := NewNativeAgent(ctx, NativeAgentConfig{OnRequest: func(context.Context, ai.TranscriptContext) error {
				observed++
				if panicObserver {
					panic("observer panic")
				}
				return cause
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			stream := agent.observedProviderStream(func(context.Context, json.RawMessage, ai.TranscriptContext, map[string]any) (*ai.AssistantMessageEventStream, error) {
				transport++
				return nil, fmt.Errorf("unexpected transport")
			}, true)
			out := ai.NewAssistantMessageEventStream()
			_, err = (ai.FallbackProvider{Retry: agentcore.RetryPolicy{MaxAttempts: 3}}).Run(ctx, out, ai.FallbackRequest{Candidates: 2, Open: func(ctx context.Context, _, _ int) (*ai.AssistantMessageEventStream, error) {
				opened++
				return stream(ctx, json.RawMessage(`{"id":"fixture"}`), ai.TranscriptContext{}, nil)
			}})
			if err == nil || opened != 1 || observed != 1 || transport != 0 {
				t.Fatal("observer failure crossed provider boundary", err, opened, observed, transport)
			}
			var preparation *ai.PreparationError
			if !errors.As(err, &preparation) {
				t.Fatal("host failure not classified", err)
			}
		})
	}
}
