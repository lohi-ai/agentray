package ai

import "github.com/lohi-ai/agentray/agentcore"

func cloneOptionalBool(value *bool, toolsPresent bool) *bool {
	if value == nil || !toolsPresent {
		return nil
	}
	cloned := *value
	return &cloned
}

func openAIChatToolChoice(choice agentcore.ToolChoice, toolsPresent bool) any {
	if !toolsPresent {
		return nil
	}
	switch choice.Mode {
	case agentcore.ToolChoiceAuto, agentcore.ToolChoiceNone, agentcore.ToolChoiceRequired:
		return string(choice.Mode)
	case agentcore.ToolChoiceNamed:
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": choice.Name},
		}
	default:
		return nil
	}
}

func openAIResponsesToolChoice(choice agentcore.ToolChoice, toolsPresent bool) any {
	if !toolsPresent {
		return nil
	}
	switch choice.Mode {
	case agentcore.ToolChoiceAuto, agentcore.ToolChoiceNone, agentcore.ToolChoiceRequired:
		return string(choice.Mode)
	case agentcore.ToolChoiceNamed:
		return map[string]any{"type": "function", "name": choice.Name}
	default:
		return nil
	}
}

func anthropicToolChoice(choice agentcore.ToolChoice, parallel *bool, toolsPresent bool) *antToolChoice {
	if !toolsPresent {
		return nil
	}
	var out *antToolChoice
	switch choice.Mode {
	case agentcore.ToolChoiceAuto:
		out = &antToolChoice{Type: "auto"}
	case agentcore.ToolChoiceNone:
		out = &antToolChoice{Type: "none"}
	case agentcore.ToolChoiceRequired:
		out = &antToolChoice{Type: "any"}
	case agentcore.ToolChoiceNamed:
		out = &antToolChoice{Type: "tool", Name: choice.Name}
	}
	if parallel != nil {
		if out == nil {
			out = &antToolChoice{Type: "auto"}
		}
		disabled := !*parallel
		out.DisableParallelToolUse = &disabled
	}
	return out
}

func googleToolChoice(choice agentcore.ToolChoice, toolsPresent bool) *agFunctionCallingConfig {
	if !toolsPresent {
		return nil
	}
	switch choice.Mode {
	case agentcore.ToolChoiceAuto:
		return &agFunctionCallingConfig{Mode: "AUTO"}
	case agentcore.ToolChoiceNone:
		return &agFunctionCallingConfig{Mode: "NONE"}
	case agentcore.ToolChoiceRequired:
		return &agFunctionCallingConfig{Mode: "ANY"}
	case agentcore.ToolChoiceNamed:
		return &agFunctionCallingConfig{Mode: "ANY", AllowedFunctionNames: []string{choice.Name}}
	default:
		return nil
	}
}
