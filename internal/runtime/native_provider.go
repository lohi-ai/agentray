package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lohi-ai/agentray/ai"
)

func validateNativeProviderModel(raw json.RawMessage) error {
	var model struct{ API string }
	if err := json.Unmarshal(raw, &model); err != nil {
		return err
	}
	switch model.API {
	case "openai-completions", "openai-responses", "anthropic-messages", "openai-codex-responses", "azure-openai-responses", "pi-messages":
		return nil
	}
	return fmt.Errorf("native Go provider for API %q is not ported", model.API)
}

// NativeProviderStream is the host's direct Go provider dispatcher. It selects
// the model's declared API and never routes an unported API through a worker or
// a legacy transcript adapter. NativeAgent uses it unless StreamFn is supplied.
func NativeProviderStream(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
	return nativeProviderStream(ctx, model, transcript, options, nil)
}
func nativeProviderStream(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any, pool ai.TokenSource) (*ai.AssistantMessageEventStream, error) {
	if err := validateNativeProviderModel(model); err != nil {
		return nil, err
	}
	controls := map[string]any{}
	for key, value := range options {
		switch key {
		case "signal", "telemetryContext", "fetch", "onPayload", "onResponse", "onProviderStreamEvent":
			continue
		}
		controls[key] = value
	}
	raw, err := json.Marshal(controls)
	if err != nil {
		return nil, err
	}
	provider := ai.OpenAICompletionsStreamOptions{Options: raw}
	if client, exists := options["fetch"]; exists && client != nil {
		var ok bool
		provider.Client, ok = client.(*http.Client)
		if !ok {
			return nil, fmt.Errorf("native fetch must be an *http.Client")
		}
	}
	switch callback := options["onPayload"].(type) {
	case nil:
	case func(json.RawMessage, json.RawMessage) (json.RawMessage, error):
		provider.OnPayload = func(_ context.Context, payload, model json.RawMessage) (json.RawMessage, error) {
			return callback(payload, model)
		}
	case func(context.Context, json.RawMessage, json.RawMessage) (json.RawMessage, error):
		provider.OnPayload = callback
	default:
		return nil, fmt.Errorf("unsupported native onPayload callback %T", callback)
	}
	switch callback := options["onResponse"].(type) {
	case nil:
	case func(ai.CompletionsResponse, json.RawMessage) error:
		provider.OnResponse = func(_ context.Context, response ai.CompletionsResponse, model json.RawMessage) error {
			return callback(response, model)
		}
	case func(context.Context, ai.CompletionsResponse, json.RawMessage) error:
		provider.OnResponse = callback
	default:
		return nil, fmt.Errorf("unsupported native onResponse callback %T", callback)
	}
	switch callback := options["onProviderStreamEvent"].(type) {
	case nil:
	case func(*json.RawMessage, json.RawMessage) error:
		provider.OnProviderStreamEvent = func(_ context.Context, event *json.RawMessage, model json.RawMessage) error {
			return callback(event, model)
		}
	case func(context.Context, *json.RawMessage, json.RawMessage) error:
		provider.OnProviderStreamEvent = callback
	default:
		return nil, fmt.Errorf("unsupported native onProviderStreamEvent callback %T", callback)
	}
	var selected struct{ API string }
	_ = json.Unmarshal(model, &selected) // Validated before callback binding.
	if pool != nil {
		if selected.API != "openai-codex-responses" {
			return nil, fmt.Errorf("native Codex account pool cannot serve API %q", selected.API)
		}
		return ai.StreamCodexResponsesPooled(ctx, model, transcript, provider, pool)
	}
	if selected.API == ai.VendorPiMessages {
		return ai.StreamPiMessagesJSON(ctx, model, transcript, provider)
	}
	if selected.API == "openai-codex-responses" {
		return ai.StreamCodexResponsesSimple(ctx, model, transcript, provider)
	}
	if selected.API == "azure-openai-responses" {
		return ai.StreamAzureResponsesSimple(ctx, model, transcript, provider)
	}
	if selected.API == "openai-responses" {
		return ai.StreamOpenAIResponsesSimple(ctx, model, transcript, provider)
	}
	if selected.API == "anthropic-messages" {
		return ai.StreamAnthropicSimple(ctx, model, transcript, provider)
	}
	return ai.StreamOpenAICompletionsSimple(ctx, model, transcript, provider)
}
