package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

func piValidateToolChoice(choice agentcore.ToolChoice, names []string) error {
	if err := choice.Validate(); err != nil {
		return err
	}
	if choice.Mode == agentcore.ToolChoiceNamed && !slices.Contains(names, choice.Name) {
		return fmt.Errorf("Pi named tool choice %q is not present in this request", choice.Name)
	}
	if choice.Mode == agentcore.ToolChoiceRequired && len(names) == 0 {
		return errors.New("Pi required tool choice requires at least one tool")
	}
	return nil
}

// piControlledPayload adds host generation controls through Pi's onPayload
// hook. Pi still owns all message encoding, signatures, caching, and streaming.
// Raw fields preserve extension values and numbers without a float64 roundtrip.
func piControlledPayload(api string, raw json.RawMessage, opts PiModelOptions) (json.RawMessage, error) {
	payload := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return nil, errors.New("Pi provider payload must be an object")
	}
	set := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err == nil {
			payload[key] = encoded
		}
		return err
	}
	var tools []struct {
		Name     string
		Function struct{ Name string }
	}
	if raw := payload["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, err
		}
	}
	// Initial forced choices are checked against the advertised catalogue by
	// the runner. A later tool-free ceiling wrap-up must not force another call.
	if len(tools) == 0 && (opts.ToolChoice.Mode != agentcore.ToolChoiceDefault || opts.ParallelToolCalls != nil) {
		delete(payload, "tool_choice")
		delete(payload, "parallel_tool_calls")
	}
	if len(tools) > 0 {
		names := make([]string, len(tools))
		for i, tool := range tools {
			names[i] = tool.Name
			if api == "openai-completions" {
				names[i] = tool.Function.Name
			}
		}
		if err := piValidateToolChoice(opts.ToolChoice, names); err != nil {
			return nil, err
		}
		choice := opts.ToolChoice
		switch api {
		case "openai-completions", "openai-responses":
			if choice.Mode == agentcore.ToolChoiceNamed {
				value := map[string]any{"type": "function", "name": choice.Name}
				if api == "openai-completions" {
					delete(value, "name")
					value["function"] = map[string]string{"name": choice.Name}
				}
				_ = set("tool_choice", value)
			} else if choice.Mode != agentcore.ToolChoiceDefault {
				_ = set("tool_choice", choice.Mode)
			}
			if opts.ParallelToolCalls != nil {
				_ = set("parallel_tool_calls", *opts.ParallelToolCalls)
			}
		case "anthropic-messages":
			value := map[string]any{}
			if existing := payload["tool_choice"]; len(existing) > 0 {
				if err := json.Unmarshal(existing, &value); err != nil || value == nil {
					return nil, errors.New("invalid native Anthropic tool choice")
				}
			}
			if choice.Mode != agentcore.ToolChoiceDefault {
				delete(value, "name")
				value["type"] = choice.Mode
				switch choice.Mode {
				case agentcore.ToolChoiceRequired:
					value["type"] = "any"
				case agentcore.ToolChoiceNamed:
					value["type"], value["name"] = "tool", choice.Name
				}
			}
			if opts.ParallelToolCalls != nil {
				if value["type"] == nil {
					value["type"] = "auto"
				}
				value["disable_parallel_tool_use"] = !*opts.ParallelToolCalls
			}
			if len(value) > 0 {
				_ = set("tool_choice", value)
			}
		default:
			return nil, fmt.Errorf("Pi generation controls do not support API %q", api)
		}
	}
	if spec := opts.OutputSchema; spec != nil {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = "output"
		}
		format := map[string]any{"type": "json_schema", "name": name, "strict": spec.Strict, "schema": spec.Schema}
		var err error
		switch api {
		case "openai-completions":
			err = set("response_format", map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": name, "strict": spec.Strict, "schema": spec.Schema}})
		case "openai-responses", "anthropic-messages":
			key := "text"
			if api == "anthropic-messages" {
				key = "output_config"
				delete(format, "name")
				delete(format, "strict")
			}
			value := map[string]json.RawMessage{}
			if existing := payload[key]; len(existing) > 0 {
				if err := json.Unmarshal(existing, &value); err != nil || value == nil {
					return nil, fmt.Errorf("invalid native Pi %s", key)
				}
			}
			value["format"], err = json.Marshal(format)
			if err == nil {
				err = set(key, value)
			}
		default:
			return nil, fmt.Errorf("Pi structured outputs do not support API %q", api)
		}
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(payload)
}
