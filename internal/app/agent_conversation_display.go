package app

import (
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	agentruntime "github.com/lohi-ai/agentray/internal/runtime"
)

// Native replay data stays server-side. The client still gets every branch node
// and uses its existing compaction marker for either runtime's summary seam.
func conversationDisplayEntry(entry storage.AgentConversationEntry) storage.AgentConversationEntry {
	switch entry.Kind {
	case agentruntime.ConvKindPiHistory:
		entry.PayloadJSON = "{}"
	case agentruntime.ConvKindPiCompaction:
		entry.Kind = agentruntime.ConvKindCompaction
		entry.PayloadJSON = "{}"
	}
	return entry
}
