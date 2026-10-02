package ai

import (
	"slices"
	"strings"
	"time"
)

const nonVisionUserImagePlaceholder = "(image omitted: model does not support images)"
const nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"

type ToolCallIDNormalizer func(id string, model Model, source Message) string

// TransformMessages ports Pi's api/transform-messages.ts: replay signatures
// only on the originating model, repair unanswered tool calls, and downgrade
// images for models that accept only text. Input messages are not mutated.
func TransformMessages(messages []Message, model Model, normalize ToolCallIDNormalizer) []Message {
	return transformMessagesAt(messages, model, normalize, func() int64 { return time.Now().UnixMilli() })
}

func replaceImagesWithPlaceholder(content []ContentBlock, placeholder string) []ContentBlock {
	result := []ContentBlock{}
	previousWasPlaceholder := false
	for _, block := range content {
		if block.Type == "image" {
			if !previousWasPlaceholder {
				result = append(result, ContentBlock{Type: "text", Text: placeholder})
			}
			previousWasPlaceholder = true
			continue
		}
		result = append(result, block)
		previousWasPlaceholder = block.Text == placeholder
	}
	return result
}

func transformMessagesAt(messages []Message, model Model, normalize ToolCallIDNormalizer, now func() int64) []Message {
	ids := map[string]string{}
	transformed := make([]Message, 0, len(messages))
	for _, original := range messages {
		message := original
		if message.Content.Text == nil && message.Content.Blocks == nil {
			message.Content = BlockContent()
		}
		if !slices.Contains(model.Input, "image") {
			if message.Role == "user" && message.Content.Text == nil {
				message.Content = BlockContent(replaceImagesWithPlaceholder(message.Content.Blocks, nonVisionUserImagePlaceholder)...)
			} else if message.Role == "toolResult" {
				message.Content = BlockContent(replaceImagesWithPlaceholder(message.Content.Blocks, nonVisionToolImagePlaceholder)...)
			}
		}
		if message.Role == "toolResult" {
			if id := ids[message.ToolCallID]; id != "" && id != message.ToolCallID {
				message.ToolCallID = id
			}
		}
		if message.Role == "assistant" {
			same := message.Provider == model.Provider && message.API == model.API && message.Model == model.ID
			blocks := []ContentBlock{}
			for _, block := range message.Content.Blocks {
				switch block.Type {
				case "thinking":
					if block.Redacted != nil && *block.Redacted {
						if same {
							blocks = append(blocks, block)
						}
						continue
					}
					if same && block.ThinkingSignature != nil && *block.ThinkingSignature != "" {
						blocks = append(blocks, block)
						continue
					}
					if strings.TrimFunc(block.Thinking, jsWhitespace) == "" {
						continue
					}
					if !same {
						block = ContentBlock{Type: "text", Text: block.Thinking}
					}
				case "text":
					if !same {
						block = ContentBlock{Type: "text", Text: block.Text}
					}
				case "toolCall":
					if !same && block.ThoughtSignature != nil && *block.ThoughtSignature != "" {
						block.ThoughtSignature = nil
					}
					if !same && normalize != nil {
						id := normalize(block.ID, model, message)
						if id != block.ID {
							ids[block.ID] = id
							block.ID = id
						}
					}
				}
				blocks = append(blocks, block)
			}
			message.Content = BlockContent(blocks...)
		}
		transformed = append(transformed, message)
	}

	result := []Message{}
	pending := []ContentBlock{}
	answered := map[string]bool{}
	held := []Message{}
	closePending := func() {
		if len(pending) > 0 {
			for _, call := range pending {
				if !answered[call.ID] {
					result = append(result, Message{Role: "toolResult", ToolCallID: call.ID, ToolName: call.Name,
						Content: BlockContent(ContentBlock{Type: "text", Text: "No result provided"}), IsError: true, Timestamp: now()})
				}
			}
			pending = nil
			answered = map[string]bool{}
		}
		result = append(result, held...)
		held = nil
	}
	for _, message := range transformed {
		switch message.Role {
		case "assistant":
			closePending()
			if message.StopReason == "error" || message.StopReason == "aborted" {
				continue
			}
			for _, block := range message.Content.Blocks {
				if block.Type == "toolCall" {
					pending = append(pending, block)
				}
			}
			if len(pending) > 0 {
				answered = map[string]bool{}
			}
		case "toolResult":
			answered[message.ToolCallID] = true
		case "system":
			if len(pending) > 0 {
				held = append(held, message)
				continue
			}
		case "user":
			closePending()
		}
		result = append(result, message)
	}
	closePending()
	return result
}

// ECMAScript trim includes BOM, excludes U+0085, and is not Go's TrimSpace.
func jsWhitespace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' || r == ' ' ||
		r == '\u00a0' || r == '\u1680' || (r >= '\u2000' && r <= '\u200a') ||
		r == '\u2028' || r == '\u2029' || r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}
