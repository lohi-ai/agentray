package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestNativeLadderToolCapabilitiesPreserveHostCatalogue(t *testing.T) {
	for _, mode := range []string{"primary-disabled", "fallback-disabled", "choice-none", "host-empty", "host-policy"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var effects, primary, fallback atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string
					Tools []json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				wantTools := 1
				if mode == "host-empty" || mode == "host-policy" || mode == "choice-none" || (mode == "primary-disabled" && body.Model == "primary") || (mode == "fallback-disabled" && body.Model == "fallback") {
					wantTools = 0
				}
				if len(body.Tools) != wantTools {
					t.Errorf("%s tool catalogue size=%d want=%d", body.Model, len(body.Tools), wantTools)
				}
				if body.Model == "primary" {
					primary.Add(1)
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
					return
				}
				if body.Model != "fallback" {
					t.Error("unexpected candidate")
				}
				n := fallback.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 1 {
					// Even an unsupported provider may hallucinate a tool call;
					// host execution must enforce the committed rung's capability.
					_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"write-1","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
				} else {
					_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"finished"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "primary", APIKey: "a", BaseURL: server.URL, Fallback: &TierConfig{Provider: "openai", ProviderID: "b", Model: "fallback", APIKey: "b", BaseURL: server.URL}}}
			if mode == "primary-disabled" {
				tier.Capabilities.Tools = agentcore.CapabilityUnsupported
			}
			if mode == "fallback-disabled" {
				tier.Fallback.Capabilities.Tools = agentcore.CapabilityUnsupported
			}
			choice := agentcore.ToolChoice{}
			if mode == "choice-none" {
				tier.Capabilities.ToolChoice, tier.Fallback.Capabilities.ToolChoice = agentcore.CapabilityUnsupported, agentcore.CapabilityUnsupported
				choice.Mode = agentcore.ToolChoiceNone
			}
			tools := json.RawMessage(`[{"name":"write","description":"write","parameters":{"type":"object"}}]`)
			if mode == "host-empty" {
				tools = json.RawMessage(`[]`)
			}
			base := agentcore.PiConfig{Options: piRequestJSON(map[string]any{"initialState": map[string]any{"tools": tools}}), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "tool" {
					return nil, fmt.Errorf("unexpected host callback %s", method)
				}
				effects.Add(1)
				return json.RawMessage(`{"content":[{"type":"text","text":"written"}]}`), nil
			}}
			ladder, err := newNativeModelLadder(tier, base, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{ToolChoice: choice}, nil })
			if err != nil {
				t.Fatal(err)
			}
			binding, _, stream := ladder.admissionBinding()
			policy := agentcore.NewAllowList("write")
			if mode == "host-policy" {
				policy = agentcore.NewAllowList()
			}
			result, err := RunPi(ctx, PiRunConfig{Input: json.RawMessage(`"finish"`), Session: PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttempts: &agentcore.RetryPolicy{MaxAttempts: 1}, Policy: policy}})
			if err != nil || result.Projection.Final != "finished" || primary.Load() != 1 || fallback.Load() != 2 {
				t.Fatal("capability transition failed", err, result.Projection.StopReason, primary.Load(), fallback.Load())
			}
			wantEffects := int32(0)
			if mode == "primary-disabled" {
				wantEffects = 1
			}
			if effects.Load() != wantEffects {
				t.Fatal("candidate lost or widened tool execution", effects.Load())
			}
			var state struct{ Tools []json.RawMessage }
			if err = json.Unmarshal(result.State, &state); err != nil {
				t.Fatal(err)
			}
			wantCatalogue := 1
			if mode == "host-empty" || mode == "host-policy" {
				wantCatalogue = 0
			}
			if len(state.Tools) != wantCatalogue {
				t.Fatal("model capability rewrote host catalogue")
			}
		})
	}
}
