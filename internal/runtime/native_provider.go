package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lohi-ai/agentray/ai"
)

func validateNativeProviderModel(raw json.RawMessage) error {
	var model struct{ API string }
	if err := json.Unmarshal(raw, &model); err != nil {
		return err
	}
	switch model.API {
	case "openai-completions", "openai-responses", "anthropic-messages", "openai-codex-responses", "azure-openai-responses", "pi-messages", ai.VendorGoogleAntigravity:
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
	provider, err := ai.BindNativeStreamOptions(raw, options, ai.OpenAICompletionsStreamOptions{})
	if err != nil {
		return nil, err
	}
	var selected struct{ API, Provider string }
	_ = json.Unmarshal(model, &selected) // Validated before callback binding.
	if pool != nil {
		if selected.API == ai.VendorGoogleAntigravity && selected.Provider == ai.VendorGoogleAntigravity {
			return ai.StreamAntigravityPooled(ctx, model, transcript, provider, pool)
		}
		if selected.API == "anthropic-messages" && selected.Provider == ai.VendorClaudeCode {
			return ai.StreamClaudeCodePooled(ctx, model, transcript, provider, pool)
		}
		if selected.API != "openai-codex-responses" {
			return nil, fmt.Errorf("native Codex account pool cannot serve API %q", selected.API)
		}
		return ai.StreamCodexResponsesPooled(ctx, model, transcript, provider, pool)
	}
	if selected.API == ai.VendorGoogleAntigravity {
		return nil, fmt.Errorf("Antigravity requires an account pool")
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
