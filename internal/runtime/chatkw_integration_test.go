package agentruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// TestChatKeywordEntryPersists is the integration half of the magic-keyword
// contract: a turn that fires "ultrathink" must leave a ConvKindKeyword entry
// in the durable conversation log (the raw user text stays as the message
// entry), while the forwarded prompt is stripped and the run carries
// reasoning_effort=high. Needs a real Postgres — the append is SQL — so it
// skips without AGENTRAY_TEST_DATABASE_URL, like the store's own suites.
func TestChatKeywordEntryPersists(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL to run the durable-log integration test")
	}
	ctx := context.Background()
	st, err := storage.Open(ctx, config.Config{
		PostgresURL: url,
		DuckDBPath:  filepath.Join(t.TempDir(), "kw.duckdb"),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)

	boot, err := st.CreateAccount(ctx, "kw-qa@example.com", "QA", "password1234", "kw ws", "kw proj")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	conv, err := st.CreateConversation(ctx, boot.User.ID, boot.Project.ID, "", "kw")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	var got chatWork
	svc := NewChatService(st)
	svc.classify = func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
		return chatDecision{Route: routeData}, nil
	}
	svc.handle = func(_ context.Context, w chatWork, _ agentcore.StreamSink) (ChatResult, error) {
		got = w
		return ChatResult{Final: "done"}, nil
	}
	_, err = svc.Chat(ctx, ChatOptions{
		ProjectID: boot.Project.ID, Message: "ultrathink why did signups drop?",
		SessionID: conv.ID, ConversationID: conv.ID,
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got.ReasoningEffort != "high" || got.Message != "why did signups drop?" {
		t.Fatalf("forwarded turn wrong: effort=%q message=%q", got.ReasoningEffort, got.Message)
	}

	entries, err := st.ConversationEntries(ctx, boot.User.ID, boot.Project.ID, conv.ID, 0)
	if err != nil {
		t.Fatalf("ConversationEntries: %v", err)
	}
	var kwEntry *storage.AgentConversationEntry
	for i := range entries {
		if entries[i].Kind == ConvKindKeyword {
			kwEntry = &entries[i]
		}
	}
	if kwEntry == nil {
		t.Fatalf("no keyword entry in durable log: %+v", entries)
	}
	var payload convKeywordPayload
	if err := json.Unmarshal([]byte(kwEntry.PayloadJSON), &payload); err != nil {
		t.Fatalf("keyword payload: %v", err)
	}
	if len(payload.Keywords) != 1 || payload.Keywords[0] != "ultrathink" {
		t.Fatalf("keywords = %+v, want [ultrathink]", payload.Keywords)
	}
}
