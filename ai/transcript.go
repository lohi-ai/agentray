package ai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The transcript functions port packages/ai/src/utils/{text,transcript}.ts.
// They do not mutate the supplied history. Like Pi, unchanged message content
// and tool declarations may share storage with the input.

func ContentText(content MessageContent, separator ...string) string {
	if content.Text != nil {
		return *content.Text
	}
	sep := "\n"
	if len(separator) > 0 {
		sep = separator[0]
	}
	parts := []string{}
	for _, block := range content.Blocks {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, sep)
}

func GetSystemMessageText(message Message) string {
	parts := []string{ContentText(message.Content)}
	for _, section := range message.Sections.ordered() {
		if section.Value != nil {
			parts = append(parts, *section.Value)
		}
	}
	return joinPromptParts(parts)
}

func RenderSystemMessageUpdate(message Message) string {
	parts := []string{}
	if text := ContentText(message.Content); text != "" {
		parts = append(parts, text)
	}
	for _, section := range message.Sections.ordered() {
		if section.Value == nil {
			parts = append(parts, `Removed system prompt section "`+section.Name+`".`)
		} else {
			parts = append(parts, `Updated system prompt section "`+section.Name+`":`+"\n\n"+*section.Value)
		}
	}
	return strings.Join(parts, "\n\n")
}

func joinPromptParts(parts []string) string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return strings.Join(result, "\n\n")
}

func CreateInitialSystemMessage(systemPrompt string, tools []Tool) *Message {
	if systemPrompt == "" && len(tools) == 0 {
		return nil
	}
	message := &Message{Role: "system", Content: TextContent(systemPrompt)}
	if len(tools) > 0 {
		message.ToolsAdded = tools
	}
	return message
}

func NormalizeContext(context Context) TranscriptContext {
	messages := context.Messages
	if head := CreateInitialSystemMessage(context.SystemPrompt, context.Tools); head != nil {
		messages = append([]Message{*head}, messages...)
	}
	return TranscriptContext{messages: messages}
}

func GetInitialSystemMessage(messages []Message) *Message {
	if len(messages) > 0 && messages[0].Role == "system" {
		return &messages[0]
	}
	return nil
}

func WithoutInitialSystemMessage(messages []Message) []Message {
	if GetInitialSystemMessage(messages) != nil {
		return messages[1:]
	}
	return messages
}

func GetCurrentTools(messages []Message) []Tool {
	tools := []Tool{}
	for _, message := range messages {
		if message.Role != "system" {
			continue
		}
		for _, removed := range message.ToolsRemoved {
			for i, tool := range tools {
				if tool.Name == removed.Name {
					tools = append(tools[:i], tools[i+1:]...)
					break
				}
			}
		}
		for _, added := range message.ToolsAdded {
			tools = setTranscriptTool(tools, added)
		}
	}
	return tools
}

func setTranscriptTool(tools []Tool, tool Tool) []Tool {
	for i := range tools {
		if tools[i].Name == tool.Name {
			tools[i] = tool
			return tools
		}
	}
	return append(tools, tool)
}

func GetCurrentSystemMessage(messages []Message) *Message {
	parts := []string{}
	sections := SystemSections{}
	var timestamp *int64
	for _, message := range messages {
		if message.Role != "system" {
			continue
		}
		if timestamp == nil {
			value := message.Timestamp
			timestamp = &value
		}
		if text := ContentText(message.Content); text != "" {
			parts = append(parts, text)
		}
		for _, section := range message.Sections.ordered() {
			index := -1
			for i := range sections {
				if sections[i].Name == section.Name {
					index = i
					break
				}
			}
			if section.Value == nil {
				if index >= 0 {
					sections = append(sections[:index], sections[index+1:]...)
				}
			} else if index >= 0 {
				sections[index] = section
			} else {
				sections = append(sections, section)
			}
		}
	}
	tools := GetCurrentTools(messages)
	if timestamp == nil && len(tools) == 0 {
		return nil
	}
	message := &Message{Role: "system", Content: TextContent(strings.Join(parts, "\n\n"))}
	if timestamp != nil {
		message.Timestamp = *timestamp
	}
	if len(sections) > 0 {
		message.Sections = sections.ordered()
	}
	if len(tools) > 0 {
		message.ToolsAdded = tools
	}
	return message
}

func GetCurrentSystemPrompt(messages []Message) string {
	if message := GetCurrentSystemMessage(messages); message != nil {
		return GetSystemMessageText(*message)
	}
	return ""
}

func CollapseSystemMessages(context TranscriptContext) TranscriptContext {
	messages := []Message{}
	if head := GetCurrentSystemMessage(context.messages); head != nil {
		messages = append(messages, *head)
	}
	for _, message := range context.messages {
		if message.Role != "system" {
			messages = append(messages, message)
		}
	}
	return TranscriptContext{messages: messages}
}

func ResolveTranscript(context TranscriptContext, supportsMidConvoSystemMessages bool) TranscriptContext {
	if supportsMidConvoSystemMessages {
		return context
	}
	return CollapseSystemMessages(context)
}

func ToToolDeclaration(tool Tool) Tool {
	return Tool{Name: tool.Name, Description: tool.Description,
		Parameters: bytes.Clone(tool.Parameters), ConstrainedSampling: bytes.Clone(tool.ConstrainedSampling)}
}

func DeclarationsEqual(left, right Tool) bool {
	if left.Name != right.Name || left.Description != right.Description {
		return false
	}
	return declarationJSONEqual(left.Parameters, right.Parameters) && declarationJSONEqual(left.ConstrainedSampling, right.ConstrainedSampling)
}

// Compare JSON after parsing/stringifying, retaining property insertion order.
// JSON object key order is significant in Pi's declaration comparison.
func declarationJSONEqual(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == len(right)
	}
	l, err := stringifyDeclarationJSON(left)
	if err != nil {
		return false
	}
	r, err := stringifyDeclarationJSON(right)
	return err == nil && bytes.Equal(l, r)
}

func stringifyDeclarationJSON(data []byte) ([]byte, error) {
	// Decode each object's keys in source order. SystemSections implements the
	// same integer-key ordering and duplicate-key replacement as JS objects.
	d := json.NewDecoder(bytes.NewReader(data))
	var value func() ([]byte, error)
	value = func() ([]byte, error) {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch token {
		case json.Delim('{'):
			keys := SystemSections{}
			values := map[string][]byte{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return nil, err
				}
				encoded, err := value()
				if err != nil {
					return nil, err
				}
				name := key.(string)
				keys = append(keys, SystemSection{Name: name})
				values[name] = encoded
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			parts := [][]byte{}
			for _, key := range keys.ordered() {
				name, _ := json.Marshal(key.Name)
				parts = append(parts, append(append(name, ':'), values[key.Name]...))
			}
			return append(append([]byte{'{'}, bytes.Join(parts, []byte{','})...), '}'), nil
		case json.Delim('['):
			parts := [][]byte{}
			for d.More() {
				encoded, err := value()
				if err != nil {
					return nil, err
				}
				parts = append(parts, encoded)
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			return append(append([]byte{'['}, bytes.Join(parts, []byte{','})...), ']'), nil
		default:
			return json.Marshal(token)
		}
	}
	if !json.Valid(data) {
		return nil, &json.SyntaxError{}
	}
	return value()
}

type ToolStateChanges struct {
	ToolsAdded   []Tool          `json:"toolsAdded"`
	ToolsRemoved []ToolReference `json:"toolsRemoved"`
}

func GetToolStateChanges(previous, current []Tool) ToolStateChanges {
	before, after := map[string]Tool{}, map[string]Tool{}
	for _, tool := range previous {
		before[tool.Name] = tool
	}
	for _, tool := range current {
		after[tool.Name] = tool
	}
	changes := ToolStateChanges{ToolsAdded: []Tool{}, ToolsRemoved: []ToolReference{}}
	for _, tool := range current {
		old, ok := before[tool.Name]
		if !ok || !DeclarationsEqual(old, tool) {
			changes.ToolsAdded = append(changes.ToolsAdded, ToToolDeclaration(tool))
		}
	}
	for _, tool := range previous {
		next, ok := after[tool.Name]
		if !ok || !DeclarationsEqual(tool, next) {
			changes.ToolsRemoved = append(changes.ToolsRemoved, ToolReference{Name: tool.Name})
		}
	}
	return changes
}

func GetDeclaredTools(messages []Message) []Tool {
	tools := []Tool{}
	for _, message := range messages {
		if message.Role != "system" {
			continue
		}
		for _, tool := range message.ToolsAdded {
			tools = setTranscriptTool(tools, tool)
		}
	}
	return tools
}

func HasToolRedefinitions(messages []Message) bool {
	declared := map[string]Tool{}
	for _, message := range messages {
		if message.Role != "system" {
			continue
		}
		for _, tool := range message.ToolsAdded {
			if previous, ok := declared[tool.Name]; ok && !DeclarationsEqual(previous, tool) {
				return true
			}
			declared[tool.Name] = tool
		}
	}
	return false
}

func HasNonAdditiveToolChanges(messages []Message) bool {
	declared := map[string]bool{}
	for _, message := range messages {
		if message.Role != "system" {
			continue
		}
		if len(message.ToolsRemoved) > 0 {
			return true
		}
		for _, tool := range message.ToolsAdded {
			if declared[tool.Name] {
				return true
			}
			declared[tool.Name] = true
		}
	}
	return false
}

type TranscriptTools struct {
	RequestTools     []Tool `json:"requestTools"`
	AnchorsAdditions bool   `json:"anchorsAdditions"`
}

func ResolveTranscriptTools(messages []Message, supportsToolAdditions bool) TranscriptTools {
	anchors := supportsToolAdditions && !HasNonAdditiveToolChanges(messages)
	tools := []Tool{}
	if anchors {
		if head := GetInitialSystemMessage(messages); head != nil && head.ToolsAdded != nil {
			tools = head.ToolsAdded
		}
	} else {
		tools = GetCurrentTools(messages)
	}
	return TranscriptTools{RequestTools: tools, AnchorsAdditions: anchors}
}
