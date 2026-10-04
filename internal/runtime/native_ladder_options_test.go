package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeAttemptOptionsOwnCredentialCapAndPayload(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", ProviderID: "a", Model: "primary", APIKey: "a", Capabilities: agentcore.ModelCapabilities{MaxOutputTokens: 100}, Fallback: &TierConfig{Provider: "openai", ProviderID: "b", Model: "fallback", APIKey: "b", Capabilities: agentcore.ModelCapabilities{MaxOutputTokens: 500}}}}
	refreshes := map[string]int{}
	ladder, err := newNativeModelLadder(tier, NativeAgentConfig{Options: json.RawMessage(`{"streamOptions":{"temperature":0.1}}`)}, func(rung ModelTier) (PiModelOptions, error) {
		return PiModelOptions{MaxTokens: 1000, ToolChoice: agentcore.ToolChoice{Mode: agentcore.ToolChoiceNone}, RefreshKey: func(context.Context, string) (string, error) {
			refreshes[rung.ProviderID]++
			return rung.APIKey + "-fresh", nil
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, _, stream := ladder.sessionBinding()
	session, err := NewPiSession(context.Background(), PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, err := ladder.attemptContext(session.ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]any{"apiKey": "stale-primary", "maxTokens": float64(100), "temperature": 0.9, "sessionId": "logical-request"}
	for i := 0; i < 2; i++ {
		request, err := session.nativeAttemptOptions(ctx, ladder.rungs[1], original)
		if err != nil {
			t.Fatal(err)
		}
		if request["apiKey"] != "b-fresh" || request["maxTokens"] != float64(500) || request["temperature"] != 0.9 || request["sessionId"] != "logical-request" {
			t.Fatal("attempt controls not bound to candidate")
		}
		if !nativeModelIdentityEqual(request["model"].(json.RawMessage), ladder.rungs[1].model) {
			t.Fatal("stale spread model")
		}
		payload := request["onPayload"].(func(context.Context, json.RawMessage, json.RawMessage) (json.RawMessage, error))
		controlled, err := payload(ctx, json.RawMessage(`{"messages":[],"tool_choice":"required","tools":[{"type":"function"}]}`), ladder.rungs[1].model)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(controlled, &body) != nil || string(body["tool_choice"]) != `"none"` {
			t.Fatal("candidate payload controls missing", string(controlled))
		}
		request["apiKey"] = "mutated"
		request["maxTokens"] = 0
	}
	if refreshes["a"] != 0 || refreshes["b"] != 2 || original["apiKey"] != "stale-primary" || original["maxTokens"] != float64(100) {
		t.Fatal("attempt leaked credential/options or skipped refresh")
	}
	if ladder.selection().Rung != 0 {
		t.Fatal("preparing options published candidate")
	}
	if _, err = session.nativeAttemptOptions(ctx, ladder.rungs[0], original); err == nil {
		t.Fatal("foreign rung accepted in attempt scope")
	}
}

func TestNativeAttemptOptionsNullCredentialDropsPreviousKey(t *testing.T) {
	ladder := testNativeLadder(t)
	original := ladder.rungs[1].config.Callback
	ladder.rungs[1].config.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		if method == "getApiKey" {
			return json.RawMessage(`null`), nil
		}
		return original(ctx, method, params, emit)
	}
	binding, _, stream := ladder.sessionBinding()
	session, err := NewPiSession(context.Background(), PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, err := ladder.attemptContext(session.ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	request, err := session.nativeAttemptOptions(ctx, ladder.rungs[1], map[string]any{"apiKey": "previous-static-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := request["apiKey"]; exists {
		t.Fatal("pooled/federated binding inherited static credential")
	}
	// Refresh failure must occur before any provider admission.
	ladder.rungs[1].config.Callback = func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		return nil, errors.New("refresh failed")
	}
	called := false
	rung := ladder.rungs[1]
	rung.stream = func(context.Context, json.RawMessage, ai.TranscriptContext, map[string]any) (*ai.AssistantMessageEventStream, error) {
		called = true
		return nil, nil
	}
	if _, err = session.openNativeAttempt(ctx, rung, ai.TranscriptContext{}, nil); err == nil || called {
		t.Fatal("refresh failure reached provider")
	}
}
