package engine

import "github.com/lohi-ai/agentray/ai"

// MessagePointers exposes the entries of a value slice to the native loop.
// The loop and its callbacks retain these objects; callers must synchronize
// any concurrent writes. Replacing a list entry does not replace that object.
func MessagePointers(messages []ai.Message) []*ai.Message {
	result := make([]*ai.Message, len(messages))
	for i := range messages {
		result[i] = &messages[i]
	}
	return result
}

// MessageValues projects shared message objects at a snapshot or provider
// boundary. Nested payloads remain shared and must be treated as immutable.
func MessageValues(messages []*ai.Message) []ai.Message {
	result := make([]ai.Message, len(messages))
	for i, message := range messages {
		result[i] = *message
	}
	return result
}
