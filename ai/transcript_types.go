package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Message is Pi's transcript union. Role selects the applicable fields. Unlike
// agentcore.Message, content order, signatures, tool results, and system deltas
// remain in their original representation. Extra retains application metadata.
type Message struct {
	Role                  string                     `json:"role"`
	Content               MessageContent             `json:"content"`
	Timestamp             int64                      `json:"timestamp"`
	Sections              SystemSections             `json:"sections,omitempty"`
	ToolsAdded            []Tool                     `json:"toolsAdded,omitempty"`
	ToolsRemoved          []ToolReference            `json:"toolsRemoved,omitempty"`
	API                   string                     `json:"api,omitempty"`
	Provider              string                     `json:"provider,omitempty"`
	Model                 string                     `json:"model,omitempty"`
	ResponseModel         *string                    `json:"responseModel,omitempty"`
	ResponseID            *string                    `json:"responseId,omitempty"`
	ProviderThinkingLevel *string                    `json:"providerThinkingLevel,omitempty"`
	ThinkingLevel         *string                    `json:"thinkingLevel,omitempty"`
	Diagnostics           json.RawMessage            `json:"diagnostics,omitempty"`
	Usage                 *Usage                     `json:"usage,omitempty"`
	StopReason            string                     `json:"stopReason,omitempty"`
	Deferred              json.RawMessage            `json:"deferred,omitempty"`
	ErrorMessage          *string                    `json:"errorMessage,omitempty"`
	RawStopReason         *string                    `json:"rawStopReason,omitempty"`
	EndTurn               *bool                      `json:"endTurn,omitempty"`
	ToolCallID            string                     `json:"toolCallId,omitempty"`
	ToolName              string                     `json:"toolName,omitempty"`
	Details               json.RawMessage            `json:"details,omitempty"`
	NestedCalls           json.RawMessage            `json:"nestedCalls,omitempty"`
	IsError               bool                       `json:"isError,omitempty"`
	Extra                 map[string]json.RawMessage `json:"-"`
}

// ContentBlock is Pi's text/thinking/image/toolCall union. Optional signatures
// use pointers so an explicit empty signature survives JSON round trips.
type ContentBlock struct {
	Type              string                     `json:"type"`
	Text              string                     `json:"text,omitempty"`
	TextSignature     *string                    `json:"textSignature,omitempty"`
	Thinking          string                     `json:"thinking,omitempty"`
	ThinkingSignature *string                    `json:"thinkingSignature,omitempty"`
	Redacted          *bool                      `json:"redacted,omitempty"`
	Data              string                     `json:"data,omitempty"`
	MIMEType          string                     `json:"mimeType,omitempty"`
	ID                string                     `json:"id,omitempty"`
	Name              string                     `json:"name,omitempty"`
	Arguments         json.RawMessage            `json:"arguments,omitempty"`
	ThoughtSignature  *string                    `json:"thoughtSignature,omitempty"`
	Namespace         *string                    `json:"namespace,omitempty"`
	Extra             map[string]json.RawMessage `json:"-"`
}

// MessageContent preserves the distinction between string and array content.
// Its zero value is JSON null, as accepted by transformMessages for old logs.
type MessageContent struct {
	Text   *string
	Blocks []ContentBlock
}

func TextContent(text string) MessageContent { return MessageContent{Text: &text} }
func BlockContent(blocks ...ContentBlock) MessageContent {
	if blocks == nil {
		blocks = []ContentBlock{}
	}
	return MessageContent{Blocks: blocks}
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.Text != nil {
		return json.Marshal(*c.Text)
	}
	return json.Marshal(c.Blocks)
}
func (c *MessageContent) UnmarshalJSON(data []byte) error {
	*c = MessageContent{}
	if len(bytes.TrimSpace(data)) > 0 && bytes.TrimSpace(data)[0] == '"' {
		return json.Unmarshal(data, &c.Text)
	}
	return json.Unmarshal(data, &c.Blocks)
}

type Usage struct {
	Input        float64   `json:"input"`
	Output       float64   `json:"output"`
	CacheRead    float64   `json:"cacheRead"`
	CacheWrite   float64   `json:"cacheWrite"`
	CacheWrite1h *float64  `json:"cacheWrite1h,omitempty"`
	Reasoning    *float64  `json:"reasoning,omitempty"`
	TotalTokens  float64   `json:"totalTokens"`
	Cost         UsageCost `json:"cost"`
}
type UsageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

type Tool struct {
	Name                string                     `json:"name"`
	Description         string                     `json:"description"`
	Parameters          json.RawMessage            `json:"parameters"`
	ConstrainedSampling json.RawMessage            `json:"constrainedSampling,omitempty"`
	Extra               map[string]json.RawMessage `json:"-"`
}
type ToolReference struct {
	Name string `json:"name"`
}

type Context struct {
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Messages     []Message `json:"messages"`
	Tools        []Tool    `json:"tools,omitempty"`
}

// TranscriptContext can only be constructed through NormalizeContext. Provider
// implementations receive the prompt and tools through system messages.
type TranscriptContext struct{ messages []Message }

func (c TranscriptContext) Messages() []Message { return c.messages }
func (c TranscriptContext) MarshalJSON() ([]byte, error) {
	messages := c.messages
	if messages == nil {
		messages = []Message{}
	}
	return json.Marshal(struct {
		Messages []Message `json:"messages"`
	}{messages})
}

// SystemSections is an ordered JSON object, not a Go map: section order changes
// the actual model prompt. A nil Value removes a section. As in JavaScript,
// array-index names enumerate before other names, in numeric order.
type SystemSection struct {
	Name  string
	Value *string
}
type SystemSections []SystemSection

func (s SystemSections) ordered() SystemSections {
	result := make(SystemSections, 0, len(s))
	indices := map[string]int{}
	for _, section := range s {
		if i, ok := indices[section.Name]; ok {
			result[i] = section
		} else {
			indices[section.Name] = len(result)
			result = append(result, section)
		}
	}
	index := func(name string) (uint64, bool) {
		n, err := strconv.ParseUint(name, 10, 32)
		return n, err == nil && n < 4294967295 && strconv.FormatUint(n, 10) == name
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, ai := index(result[i].Name)
		b, bi := index(result[j].Name)
		if ai && bi {
			return a < b
		}
		return ai && !bi
	})
	return result
}
func (s SystemSections) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for i, section := range s.ordered() {
		if i > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(section.Name)
		value, _ := json.Marshal(section.Value)
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
func (s *SystemSections) UnmarshalJSON(data []byte) error {
	*s = nil
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil {
		return err
	}
	if start != json.Delim('{') {
		return fmt.Errorf("ai: sections must be an object")
	}
	for d.More() {
		name, err := d.Token()
		if err != nil {
			return err
		}
		var value *string
		if err := d.Decode(&value); err != nil {
			return err
		}
		*s = append(*s, SystemSection{Name: name.(string), Value: value})
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	*s = s.ordered()
	return nil
}

// JSON union encoding retains required empty fields and unknown metadata. The
// latter is necessary for Pi's spread-based transcript transformations.
func marshalTranscriptObject(value any, extra map[string]json.RawMessage, required map[string]any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key, value := range extra {
		if _, exists := fields[key]; !exists {
			fields[key] = value
		}
	}
	for key, value := range required {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}

func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	required := map[string]any{}
	if m.Role == "assistant" {
		required["api"], required["provider"], required["model"] = m.API, m.Provider, m.Model
		required["stopReason"] = m.StopReason
		if m.Usage == nil {
			required["usage"] = Usage{}
		}
	}
	if m.Role == "toolResult" {
		required["isError"], required["toolCallId"], required["toolName"] = m.IsError, m.ToolCallID, m.ToolName
	}
	if m.ToolsAdded != nil {
		required["toolsAdded"] = m.ToolsAdded
	}
	if m.ToolsRemoved != nil {
		required["toolsRemoved"] = m.ToolsRemoved
	}
	if m.Sections != nil {
		required["sections"] = m.Sections
	}
	return marshalTranscriptObject(plain(m), m.Extra, required)
}
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type plain ContentBlock
	required := map[string]any{}
	switch b.Type {
	case "text":
		required["text"] = b.Text
	case "thinking":
		required["thinking"] = b.Thinking
	case "image":
		required["data"], required["mimeType"] = b.Data, b.MIMEType
	case "toolCall":
		required["id"], required["name"], required["arguments"] = b.ID, b.Name, b.Arguments
	}
	return marshalTranscriptObject(plain(b), b.Extra, required)
}
func (t Tool) MarshalJSON() ([]byte, error) {
	type plain Tool
	return marshalTranscriptObject(plain(t), t.Extra, nil)
}

func decodeTranscriptObject(data []byte, target any, known string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, target); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(data, &extra); err != nil {
		return nil, err
	}
	for _, key := range bytes.Fields([]byte(known)) {
		delete(extra, string(key))
	}
	if len(extra) == 0 {
		extra = nil
	}
	return extra, nil
}
func (m *Message) UnmarshalJSON(data []byte) error {
	type plain Message
	*m = Message{}
	extra, err := decodeTranscriptObject(data, (*plain)(m), "role content timestamp sections toolsAdded toolsRemoved api provider model responseModel responseId providerThinkingLevel thinkingLevel diagnostics usage stopReason deferred errorMessage rawStopReason endTurn toolCallId toolName details nestedCalls isError")
	m.Extra = extra
	return err
}
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type plain ContentBlock
	*b = ContentBlock{}
	extra, err := decodeTranscriptObject(data, (*plain)(b), "type text textSignature thinking thinkingSignature redacted data mimeType id name arguments thoughtSignature namespace")
	b.Extra = extra
	return err
}
func (t *Tool) UnmarshalJSON(data []byte) error {
	type plain Tool
	*t = Tool{}
	extra, err := decodeTranscriptObject(data, (*plain)(t), "name description parameters constrainedSampling")
	t.Extra = extra
	return err
}
