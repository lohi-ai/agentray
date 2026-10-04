package agentruntime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeLadderAttemptScopeRoutesWithoutPublishing(t *testing.T) {
	ladder := testNativeLadder(t)
	var dispatched int
	ladder.rungs[1].stream = func(context.Context, json.RawMessage, ai.TranscriptContext, map[string]any) (*ai.AssistantMessageEventStream, error) {
		dispatched++
		return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "ok"}), nil
	}
	binding, _, stream := ladder.sessionBinding()
	ctx, err := ladder.attemptContext(context.Background(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"model": ladder.rungs[1].model})
	if _, err = binding.Callback(ctx, "prepareRequest", request, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = binding.Callback(context.Background(), "prepareRequest", request, nil); err == nil {
		t.Fatal("candidate model admitted outside its scope")
	}
	var wg sync.WaitGroup
	for _, scoped := range []bool{false, true} {
		wg.Add(1)
		go func(scoped bool) {
			defer wg.Done()
			requestCtx := context.Background()
			want := `"secret-a-fresh"`
			if scoped {
				requestCtx = ctx
				want = `"secret-b-fresh"`
			}
			key, err := binding.Callback(requestCtx, "getApiKey", json.RawMessage(`"openai"`), nil)
			if err != nil || string(key) != want {
				t.Error("request scope leaked credentials across attempts", err)
			}
		}(scoped)
	}
	wg.Wait()
	if _, err = stream(ctx, ladder.rungs[1].model, ai.TranscriptContext{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = stream(context.Background(), ladder.rungs[1].model, ai.TranscriptContext{}, nil); err == nil {
		t.Fatal("unscoped candidate dispatched")
	}
	if dispatched != 1 || ladder.selection().Rung != 0 || ladder.selection().Generation != 0 {
		t.Fatal("attempt scope changed committed binding")
	}
	child := ladder.fork()
	childBinding, _, childStream := child.sessionBinding()
	if _, err = childBinding.Callback(ctx, "getApiKey", json.RawMessage(`"openai"`), nil); err == nil {
		t.Fatal("parent scope crossed child boundary")
	}
	if _, err = childStream(ctx, child.rungs[1].model, ai.TranscriptContext{}, nil); err == nil {
		t.Fatal("parent scope dispatched child")
	}
	if err = ladder.selectRung(context.Background(), 0, 1, func(context.Context, nativeLadderSelection) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = binding.Callback(ctx, "getApiKey", json.RawMessage(`"openai"`), nil); err == nil {
		t.Fatal("stale scope used credentials")
	}
	if _, err = stream(ctx, ladder.rungs[1].model, ai.TranscriptContext{}, nil); err == nil {
		t.Fatal("stale scope dispatched")
	}
}

func TestNativeLadderAttemptScopeDoesNotChangeToolPolicy(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "primary", APIKey: "fixture", Fallback: &TierConfig{Model: "fallback", Capabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported}}}}
	ladder, err := newNativeModelLadder(tier, NativeAgentConfig{}, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	binding, _, _ := ladder.sessionBinding()
	ctx, err := ladder.attemptContext(context.Background(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := binding.Callback(ctx, "beforeToolCall", json.RawMessage(`{}`), nil)
	if err != nil || len(result) != 0 {
		t.Fatal("uncommitted candidate changed tool policy", string(result), err)
	}
	if err = ladder.selectRung(context.Background(), 0, 1, func(context.Context, nativeLadderSelection) error { return nil }); err != nil {
		t.Fatal(err)
	}
	result, err = binding.Callback(ctx, "beforeToolCall", json.RawMessage(`{}`), nil)
	var decision struct{ Block bool }
	if err != nil || json.Unmarshal(result, &decision) != nil || !decision.Block {
		t.Fatal("committed tool policy not applied", string(result), err)
	}
}

func TestNativePayloadHookPreservesProviderRequestContext(t *testing.T) {
	type requestKey struct{}
	called := false
	agent, err := NewNativeAgent(context.Background(), NativeAgentConfig{
		Options: json.RawMessage(`{"initialState":{"model":{"id":"test","api":"test","provider":"test"}},"streamMode":"native","callbacks":["onPayload"]}`),
		Callback: func(ctx context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method != "onPayload" || ctx.Value(requestKey{}) != "attempt" {
				t.Error("payload hook lost provider request context")
			}
			called = true
			return nil, nil
		},
		StreamFn: func(ctx context.Context, model json.RawMessage, _ ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
			payload := options["onPayload"].(func(context.Context, json.RawMessage, json.RawMessage) (json.RawMessage, error))
			if _, err := payload(context.WithValue(ctx, requestKey{}, "attempt"), json.RawMessage(`{}`), model); err != nil {
				return nil, err
			}
			return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "done"}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if err = agent.Prompt(context.Background(), json.RawMessage(`"hello"`)); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("payload callback not exercised")
	}
}
