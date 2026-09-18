package ai

import (
	"encoding/json"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// CapabilitiesFor returns adapter-level knowledge for a provider/model pair.
// It intentionally stays sparse: this is not a model catalog, and unknown
// preserves the current optimistic request path. Live discovery may overlay
// these defaults with explicit model-level facts.
func CapabilitiesFor(vendor, _ string) agentcore.ModelCapabilities {
	supported := agentcore.CapabilitySupported
	unsupported := agentcore.CapabilityUnsupported
	switch NormalizeVendor(vendor) {
	case "openai":
		return agentcore.ModelCapabilities{
			Tools: supported, ToolChoice: supported, ReasoningEffort: supported,
			StructuredOutput: supported, PromptCaching: supported,
			// AgentRay currently speaks Chat Completions here, not the stateful
			// Responses API.
			StatefulResponses: unsupported,
		}
	case VendorOpenAIResponses:
		return agentcore.ModelCapabilities{
			Tools: supported, ToolChoice: supported, ReasoningEffort: supported,
			StructuredOutput: supported, PromptCaching: supported,
			StatefulResponses: supported,
		}
	case "anthropic":
		return agentcore.ModelCapabilities{
			Tools: supported, StructuredOutput: supported, PromptCaching: supported,
			// The neutral request has no Anthropic thinking/tool-choice mapper yet.
			ToolChoice: unsupported, ReasoningEffort: unsupported,
			StatefulResponses: unsupported,
		}
	case VendorOpenAICodex, VendorGoogleAntigravity:
		return agentcore.ModelCapabilities{
			Tools: supported, ReasoningEffort: supported,
		}
	case VendorClaudeCode:
		return agentcore.ModelCapabilities{
			Tools: supported, StructuredOutput: supported, PromptCaching: supported,
			ToolChoice: unsupported, ReasoningEffort: unsupported,
			StatefulResponses: unsupported,
		}
	default:
		return agentcore.ModelCapabilities{}
	}
}

func support(value bool) agentcore.CapabilitySupport {
	if value {
		return agentcore.CapabilitySupported
	}
	return agentcore.CapabilityUnsupported
}

// discoveredCapabilities is the intentionally permissive subset of model-list
// metadata used by OpenAI-compatible routers and local engines. Every field is
// optional; only an explicit fact changes request behavior.
type discoveredCapabilities struct {
	SupportsTools             *bool                      `json:"supports_tools"`
	SupportsToolChoice        *bool                      `json:"supports_tool_choice"`
	SupportsReasoning         *bool                      `json:"supports_reasoning"`
	SupportsVision            *bool                      `json:"supports_vision"`
	SupportsStructuredOutputs *bool                      `json:"supports_structured_outputs"`
	SupportsPromptCaching     *bool                      `json:"supports_prompt_caching"`
	SupportsStatefulResponses *bool                      `json:"supports_stateful_responses"`
	Capabilities              map[string]json.RawMessage `json:"capabilities"`
	SupportedParameters       []string                   `json:"supported_parameters"`
	InputModalities           []string                   `json:"input_modalities"`
	Architecture              struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

func (d discoveredCapabilities) modelCapabilities() agentcore.ModelCapabilities {
	var out agentcore.ModelCapabilities
	// Some routers place booleans in a free-form capabilities object. Accept
	// common spellings, but never infer false from a missing key.
	for key, raw := range d.Capabilities {
		value, ok := explicitBool(raw)
		if !ok {
			continue
		}
		switch normalizeCapabilityName(key) {
		case "tools", "toolcalling", "functioncalling":
			out.Tools = support(value)
		case "toolchoice":
			out.ToolChoice = support(value)
		case "reasoning", "reasoningeffort", "thinking":
			out.ReasoningEffort = support(value)
		case "vision", "image", "imageinput":
			out.ImageInput = support(value)
		case "structuredoutput", "structuredoutputs", "responseformat", "jsonschema":
			out.StructuredOutput = support(value)
		case "promptcache", "promptcaching", "cachecontrol":
			out.PromptCaching = support(value)
		case "statefulresponses", "previousresponseid":
			out.StatefulResponses = support(value)
		}
	}

	// OpenRouter-style supported_parameters is positive evidence only: several
	// gateways return partial lists, so absence must not become unsupported.
	for _, parameter := range d.SupportedParameters {
		switch normalizeCapabilityName(parameter) {
		case "tools":
			out.Tools = agentcore.CapabilitySupported
		case "toolchoice":
			out.ToolChoice = agentcore.CapabilitySupported
		case "reasoning", "reasoningeffort":
			out.ReasoningEffort = agentcore.CapabilitySupported
		case "responseformat", "structuredoutputs":
			out.StructuredOutput = agentcore.CapabilitySupported
		case "promptcachekey", "cachecontrol":
			out.PromptCaching = agentcore.CapabilitySupported
		case "previousresponseid":
			out.StatefulResponses = agentcore.CapabilitySupported
		}
	}

	modalities := d.InputModalities
	if len(modalities) == 0 {
		modalities = d.Architecture.InputModalities
	}
	if len(modalities) > 0 {
		out.ImageInput = agentcore.CapabilityUnsupported
		for _, modality := range modalities {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				out.ImageInput = agentcore.CapabilitySupported
				break
			}
		}
	}
	// Dedicated support fields are the least ambiguous signal and win over a
	// contradictory generic map/list from the same response.
	if d.SupportsTools != nil {
		out.Tools = support(*d.SupportsTools)
	}
	if d.SupportsToolChoice != nil {
		out.ToolChoice = support(*d.SupportsToolChoice)
	}
	if d.SupportsReasoning != nil {
		out.ReasoningEffort = support(*d.SupportsReasoning)
	}
	if d.SupportsVision != nil {
		out.ImageInput = support(*d.SupportsVision)
	}
	if d.SupportsStructuredOutputs != nil {
		out.StructuredOutput = support(*d.SupportsStructuredOutputs)
	}
	if d.SupportsPromptCaching != nil {
		out.PromptCaching = support(*d.SupportsPromptCaching)
	}
	if d.SupportsStatefulResponses != nil {
		out.StatefulResponses = support(*d.SupportsStatefulResponses)
	}
	return out
}

func explicitBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "supported", "yes":
			return true, true
		case "false", "unsupported", "no":
			return false, true
		}
	}
	return false, false
}

func normalizeCapabilityName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(value)
}
