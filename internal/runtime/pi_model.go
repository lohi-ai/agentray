package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/ai"
)

// PiModelOptions supplies host limits/pricing for one resolved model binding.
// A nil pricing table means unknown prices, not a free model.
type PiModelOptions struct {
	MaxTokens         int
	Pricing           observe.Pricing
	ToolChoice        agentcore.ToolChoice
	ParallelToolCalls *bool
	OutputSchema      *agentcore.OutputSchema
	// RefreshKey resolves the bound tier's credential on every request. Errors
	// and empty credentials fail the request; they never fall back to a stale key.
	RefreshKey func(context.Context, string) (string, error)
}

// BindPi connects this tier's single API-key model to Pi's original provider
// implementation. It preserves the resolver's provider identity, endpoint, and
// chosen API dialect. OAuth pools and fallback ladders require their own host
// lifecycle integration and are rejected until that integration is selected.
// The returned bool says whether the native cost metadata is known.
func (t ModelTier) BindPi(cfg agentcore.PiConfig, opts PiModelOptions) (agentcore.PiConfig, bool, error) {
	if strings.TrimSpace(t.Model) == "" {
		return cfg, false, errors.New("Pi model ID is required")
	}
	if err := t.Capabilities.Validate(); err != nil {
		return cfg, false, err
	}
	if err := opts.ToolChoice.Validate(); err != nil {
		return cfg, false, err
	}
	forced := opts.ToolChoice.Mode == agentcore.ToolChoiceRequired || opts.ToolChoice.Mode == agentcore.ToolChoiceNamed
	if forced && (t.Capabilities.Tools == agentcore.CapabilityUnsupported || t.Capabilities.ToolChoice == agentcore.CapabilityUnsupported) {
		return cfg, false, errors.New("Pi model does not support forced tool choice")
	}
	disableForChoice := opts.ToolChoice.Mode == agentcore.ToolChoiceNone && t.Capabilities.ToolChoice == agentcore.CapabilityUnsupported
	if t.Capabilities.ToolChoice == agentcore.CapabilityUnsupported {
		opts.ToolChoice, opts.ParallelToolCalls = agentcore.ToolChoice{}, nil
	}
	if t.Capabilities.StructuredOutput == agentcore.CapabilityUnsupported {
		opts.OutputSchema = nil // The composed host still validates every text answer.
	}
	if t.Fallback != nil || strings.TrimSpace(t.FallbackModel) != "" {
		return cfg, false, errors.New("Pi model binding requires explicit native fallback lifecycle integration")
	}
	if ai.IsOAuthVendor(t.Provider) || t.TokenSource != nil {
		return cfg, false, errors.New("Pi model binding requires explicit OAuth account-pool lifecycle integration")
	}
	provider, err := t.RawProvider()
	if err != nil {
		return cfg, false, err
	}
	var api, endpoint string
	var compat map[string]any
	switch p := provider.(type) {
	case *ai.OpenAIProvider:
		api, endpoint = "openai-completions", p.BaseURL
		compat = map[string]any{"maxTokensField": p.Compat.MaxTokensField}
	case *ai.OpenAIResponsesProvider:
		api, endpoint = "openai-responses", strings.TrimSuffix(p.BaseURL, "/responses")
	case *ai.AnthropicProvider:
		api, endpoint = "anthropic-messages", p.BaseURL
	default:
		return cfg, false, fmt.Errorf("provider %q has no native Pi model binding", provider.Name())
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return cfg, false, errors.New("Pi provider endpoint must be an HTTP(S) URL without embedded credentials")
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultRunMaxTokens
	}
	if cap := t.Capabilities.MaxOutputTokens; cap > 0 && maxTokens > cap {
		maxTokens = cap
	}
	input := []string{"text"}
	if t.Capabilities.ImageInput != agentcore.CapabilityUnsupported {
		input = append(input, "image")
	}
	cost := map[string]float64{}
	known := true
	for _, rate := range []struct {
		name  string
		usage agentcore.Usage
	}{
		{"input", agentcore.Usage{InputTokens: 1000000}},
		{"output", agentcore.Usage{OutputTokens: 1000000}},
		{"cacheRead", agentcore.Usage{CacheReadTokens: 1000000}},
		{"cacheWrite", agentcore.Usage{CacheWriteTokens: 1000000}},
	} {
		value, found := opts.Pricing.Cost(t.Model, rate.usage)
		cost[rate.name] = value
		known = known && found
	}
	model := map[string]any{"id": t.Model, "name": t.Model, "api": api, "provider": provider.Name(), "baseUrl": endpoint,
		"reasoning": t.Capabilities.ReasoningEffort != agentcore.CapabilityUnsupported,
		"input":     input, "contextWindow": t.EffectiveWindow(), "maxTokens": maxTokens, "cost": cost}
	if compat != nil {
		model["compat"] = compat
	}
	options := map[string]json.RawMessage{}
	if len(cfg.Options) > 0 {
		if err := json.Unmarshal(cfg.Options, &options); err != nil || options == nil {
			return cfg, false, errors.New("Pi options must be an object")
		}
	}
	initial := map[string]json.RawMessage{}
	if raw := options["initialState"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &initial); err != nil || initial == nil {
			return cfg, false, errors.New("Pi initial state must be an object")
		}
	}
	initial["model"], err = json.Marshal(model)
	if err != nil {
		return cfg, false, fmt.Errorf("invalid native Pi model metadata: %w", err)
	}
	var boundModel any
	_ = json.Unmarshal(initial["model"], &boundModel)
	checkModel := func(raw json.RawMessage) error {
		var requested any
		if err := json.Unmarshal(raw, &requested); err != nil || !reflect.DeepEqual(requested, boundModel) {
			return errors.New("Pi request uses an unbound model or changed endpoint; explicit model migration required")
		}
		return nil
	}
	toolsDisabled := t.Capabilities.Tools == agentcore.CapabilityUnsupported || disableForChoice
	if toolsDisabled {
		initial["tools"] = json.RawMessage(`[]`)
	}
	options["initialState"], _ = json.Marshal(initial)
	options["streamMode"] = json.RawMessage(`"native"`)
	streamOptions := map[string]json.RawMessage{}
	if raw := options["streamOptions"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &streamOptions); err != nil || streamOptions == nil {
			return cfg, false, errors.New("Pi stream options must be an object")
		}
	}
	streamOptions["maxTokens"], _ = json.Marshal(maxTokens)
	options["streamOptions"], _ = json.Marshal(streamOptions)
	var callbacks []string
	if raw := options["callbacks"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &callbacks); err != nil {
			return cfg, false, err
		}
	}
	originalCallbacks := slices.Clone(callbacks)
	controls := opts.ToolChoice.Mode != agentcore.ToolChoiceDefault || opts.ParallelToolCalls != nil || opts.OutputSchema != nil
	for _, name := range []string{"getApiKey", "prepareRequest", "beforeToolCall", "onPayload"} {
		if name == "beforeToolCall" && !toolsDisabled {
			continue
		}
		if name == "onPayload" && !controls {
			continue
		}
		if !slices.Contains(callbacks, name) {
			callbacks = append(callbacks, name)
		}
	}
	options["callbacks"], _ = json.Marshal(callbacks)
	cfg.Options, err = json.Marshal(options)
	if err != nil {
		return cfg, false, err
	}
	original := cfg.Callback
	cfg.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		if method == "onPayload" && controls {
			var request struct{ Payload, Model json.RawMessage }
			if err := json.Unmarshal(params, &request); err != nil {
				return nil, err
			}
			if err := checkModel(request.Model); err != nil {
				return nil, err
			}
			if slices.Contains(originalCallbacks, method) {
				if original == nil {
					return nil, errors.New("missing Pi onPayload callback")
				}
				value, err := original(ctx, method, params, emit)
				if err != nil {
					return nil, err
				}
				if len(value) > 0 {
					request.Payload = value
				}
			}
			return piControlledPayload(api, request.Payload, opts)
		}
		if method == "prepareRequest" {
			var request struct{ Model json.RawMessage }
			if err := json.Unmarshal(params, &request); err != nil {
				return nil, err
			}
			if err := checkModel(request.Model); err != nil {
				return nil, err
			}
			if !slices.Contains(originalCallbacks, method) {
				return nil, nil
			}
			if original == nil {
				return nil, errors.New("missing Pi prepareRequest callback")
			}
			value, err := original(ctx, method, params, emit)
			if err != nil || len(value) == 0 {
				return value, err
			}
			var update struct{ Model json.RawMessage }
			if err := json.Unmarshal(value, &update); err != nil {
				return nil, err
			}
			if len(update.Model) > 0 {
				if err := checkModel(update.Model); err != nil {
					return nil, err
				}
			}
			return value, nil
		}
		if method == "getApiKey" {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var requested string
			if err := json.Unmarshal(params, &requested); err != nil || requested != provider.Name() {
				return nil, errors.New("Pi requested credentials for an unbound provider")
			}
			key := t.APIKey
			if opts.RefreshKey != nil {
				var err error
				key, err = opts.RefreshKey(ctx, requested)
				if err != nil {
					return nil, err
				}
			}
			if strings.TrimSpace(key) == "" {
				return nil, errors.New("Pi provider credential is empty")
			}
			return json.Marshal(key)
		}
		if method == "beforeToolCall" && toolsDisabled {
			return json.Marshal(map[string]any{"block": true, "reason": "the selected model does not support tools"})
		}
		if method != "tool" && method != "stream" && !slices.Contains(originalCallbacks, method) {
			return nil, nil
		}
		if original == nil {
			return nil, fmt.Errorf("missing Pi callback for %s", method)
		}
		return original(ctx, method, params, emit)
	}
	return cfg, known, nil
}
