package agentruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/agentcore/host"
	"github.com/2found/2ai/ai"
	"github.com/2found/2ai/telemetry/llm"
)

// complete runs an auxiliary text request through the same native transport,
// fallback, pricing and telemetry boundary as an agent request. Its messages
// are newly authored instructions, never a projection of provider history.
func (t ModelTier) complete(ctx context.Context, sink llm.Sink, request agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	messages, err := host.InputMessages(request.Messages)
	if err != nil {
		return agentcore.ChatResponse{}, err
	}
	options, err := json.Marshal(map[string]any{
		"initialState": map[string]any{"messages": messages, "thinkingLevel": "off", "tools": []any{}},
		"callbacks":    []string{"finishTurn"},
	})
	if err != nil {
		return agentcore.ChatResponse{}, err
	}
	binding := NativeAgentConfig{Options: options, Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		return json.RawMessage(`{"action":"end"}`), nil
	}}
	ladder, err := newNativeModelLadder(t, binding, func(ModelTier) (PiModelOptions, error) {
		return PiModelOptions{MaxTokens: request.MaxTokens, Pricing: ai.DefaultPricing(), ToolChoice: agentcore.ToolChoice{Mode: agentcore.ToolChoiceNone}}, nil
	})
	if err != nil {
		return agentcore.ChatResponse{}, err
	}
	binding, known, stream := ladder.admissionBinding()
	binding = bindPiTrace(binding, sink, known, "")
	result, err := RunPi(ctx, PiRunConfig{PricingKnown: known, Session: PiSessionConfig{
		Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttempts: &agentcore.RetryPolicy{},
	}})
	response := agentcore.ChatResponse{Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: result.Projection.Final}, Usage: result.Projection.Usage, StopReason: result.Projection.StopReason}
	if err == nil && response.StopReason != "stop" {
		err = errors.New("native auxiliary request did not complete")
	}
	return response, err
}
