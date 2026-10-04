package agentruntime

import (
	"context"
	"encoding/json"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
)

func supportsNativeOAuthPool(provider string) bool {
	switch ai.NormalizeOAuthVendor(provider) {
	case ai.VendorOpenAICodex, ai.VendorClaudeCode, ai.VendorGoogleAntigravity:
		return true
	default:
		return false
	}
}

// bindNativeTier keeps the model/credential callbacks and concrete stream
// dispatcher together. Callers cannot admit a pooled model without its stream.
func (t ModelTier) bindNativeTier(cfg agentcore.PiConfig, opts PiModelOptions, override engine.StreamFn) (agentcore.PiConfig, bool, engine.StreamFn, error) {
	pooled := override == nil && supportsNativeOAuthPool(t.Provider) && t.TokenSource != nil
	binding, known, err := t.bindPi(cfg, opts, pooled)
	if err != nil {
		return cfg, false, nil, err
	}
	stream := override
	if pooled {
		stream = func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
			return nativeProviderStream(ctx, model, transcript, options, t.TokenSource)
		}
	}
	if stream == nil {
		var wire struct {
			InitialState struct{ Model json.RawMessage }
		}
		if err := json.Unmarshal(binding.Options, &wire); err != nil {
			return cfg, false, nil, err
		}
		if err := validateNativeProviderModel(wire.InitialState.Model); err != nil {
			return cfg, false, nil, err
		}
	}
	return binding, known, stream, nil
}
