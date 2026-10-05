package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/agentcoretest"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// TestSessionStoreConformancePostgres runs the same semantic contract as the
// laptop-friendly in-memory store. It is opt-in because it creates real rows
// and needs the repository's migrated PostgreSQL test database.
func TestSessionStoreConformancePostgres(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL to run PostgreSQL SessionStore conformance")
	}
	ctx := context.Background()
	st, err := storage.Open(ctx, config.Config{
		PostgresURL: url,
		DuckDBPath:  filepath.Join(t.TempDir(), "session-store-conformance.duckdb"),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)

	stamp := time.Now().UnixNano()
	boot, err := st.CreateAccount(ctx,
		fmt.Sprintf("session-conformance-%d@example.com", stamp),
		"Session conformance", "password1234", "conformance workspace", "conformance project",
	)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	agentcoretest.RunSessionStoreConformance(t, agentcoretest.SessionStoreHarness{
		Store: NewSessionStore(st),
		NewSessionID: func(t *testing.T) string {
			t.Helper()
			id, err := st.CreateAgentRun(ctx, boot.Project.ID, "", "manual", "")
			if err != nil {
				t.Fatalf("CreateAgentRun: %v", err)
			}
			return id
		},
	})
}

// TestPostgresSessionStoreExternalizesRichImages proves the representation
// optimization at the real database boundary: JSON rows stay small, artifact
// reads are session-fenced, and agentcore receives the original message shape.
func TestPostgresSessionStoreExternalizesRichImages(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL to run PostgreSQL rich-session integration")
	}
	ctx := context.Background()
	st, err := storage.Open(ctx, config.Config{
		PostgresURL: url,
		DuckDBPath:  filepath.Join(t.TempDir(), "session-rich-images.duckdb"),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx,
		fmt.Sprintf("session-rich-%d@example.com", time.Now().UnixNano()),
		"Session rich image", "password1234", "rich workspace", "rich project",
	)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	runID, err := st.CreateAgentRun(ctx, boot.Project.ID, "", "manual", "")
	if err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}

	data := largeSessionPNG()
	want := agentcore.Message{Role: agentcore.RoleTool, Name: "eval", ToolCallID: "call-1", Content: "plot", ContentParts: []agentcore.ContentPart{{
		Type: agentcore.ContentPartImage, MIMEType: "image/png", Data: data,
	}}}
	adapter := &pgSessionStore{store: st}
	if err := adapter.Append(ctx, runID, agentcore.SessionEntry{Kind: agentcore.EntryMessage, Message: &want}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	raw, err := st.AgentSessionLog(ctx, runID)
	if err != nil || len(raw) != 1 {
		t.Fatalf("raw log: rows=%d err=%v", len(raw), err)
	}
	var persisted agentcore.SessionEntry
	if err := json.Unmarshal([]byte(raw[0].PayloadJSON), &persisted); err != nil {
		t.Fatalf("decode raw payload: %v", err)
	}
	part := persisted.Message.ContentParts[0]
	if part.Data != "" || part.DataRef == "" {
		t.Fatalf("raw row retained inline image: %+v", part)
	}
	if len(raw[0].PayloadJSON) >= len(data)/2 {
		t.Fatalf("raw row was not materially reduced: row=%d base64=%d", len(raw[0].PayloadJSON), len(data))
	}
	artifact, err := st.AgentSpillWindowForSession(ctx, part.DataRef, runID, 0, len(data)+1)
	if err != nil || string(artifact.Content) != data {
		t.Fatalf("artifact round trip: total=%d err=%v", artifact.Total, err)
	}
	if _, err := st.AgentSpillWindowForSession(ctx, part.DataRef, "another-session", 0, 1); !errors.Is(err, storage.ErrAgentSpillNotFound) {
		t.Fatalf("cross-session artifact read error = %v, want not found", err)
	}

	log, err := adapter.Log(ctx, runID)
	if err != nil || len(log) != 1 {
		t.Fatalf("hydrated log: rows=%d err=%v", len(log), err)
	}
	if got := log[0].Message; got == nil || got.ContentParts[0].Data != data || got.ContentParts[0].DataRef != "" {
		t.Fatalf("hydrated message = %+v", got)
	}
}

func TestPostgresSessionLeaseFencesStaleOwner(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL to run PostgreSQL lease integration")
	}
	ctx := context.Background()
	st, err := storage.Open(ctx, config.Config{
		PostgresURL: url,
		DuckDBPath:  filepath.Join(t.TempDir(), "session-lease.duckdb"),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx,
		fmt.Sprintf("session-lease-%d@example.com", time.Now().UnixNano()),
		"Session lease", "password1234", "lease workspace", "lease project",
	)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := st.CreateAgentRun(ctx, boot.Project.ID, "", "manual", "")
	if err != nil {
		t.Fatal(err)
	}

	owner1 := uuid.NewString()
	epoch1, ok, err := st.AcquireAgentSessionLease(ctx, runID, runID, owner1, 80*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("owner1 acquire: epoch=%d ok=%v err=%v", epoch1, ok, err)
	}
	if _, ok, err := st.AcquireAgentSessionLease(ctx, runID, runID, uuid.NewString(), time.Second); err != nil || ok {
		t.Fatalf("live takeover: ok=%v err=%v", ok, err)
	}

	adapter := &pgSessionStore{store: st}
	owner1Ctx := context.WithValue(ctx, sessionLeaseContextKey{}, sessionLeaseToken{sessionID: runID, ownerID: owner1, epoch: epoch1})
	if err := adapter.Append(owner1Ctx, runID, agentcore.SessionEntry{Kind: agentcore.EntryGoal, Goal: "first"}); err != nil {
		t.Fatalf("current owner append: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	owner2 := uuid.NewString()
	epoch2, ok, err := st.AcquireAgentSessionLease(ctx, runID, runID, owner2, time.Second)
	if err != nil || !ok || epoch2 <= epoch1 {
		t.Fatalf("owner2 takeover: epoch=%d ok=%v err=%v", epoch2, ok, err)
	}
	if err := adapter.Append(owner1Ctx, runID, agentcore.SessionEntry{Kind: agentcore.EntryGoal, Goal: "stale"}); !errors.Is(err, agentcore.ErrSessionLeaseLost) {
		t.Fatalf("stale append error = %v, want ErrSessionLeaseLost", err)
	}
	owner2Ctx := context.WithValue(ctx, sessionLeaseContextKey{}, sessionLeaseToken{sessionID: runID, ownerID: owner2, epoch: epoch2})
	if err := adapter.Append(owner2Ctx, runID, agentcore.SessionEntry{Kind: agentcore.EntryGoal, Goal: "current"}); err != nil {
		t.Fatalf("new owner append: %v", err)
	}
	if err := st.ReleaseAgentSessionLease(ctx, runID, owner1, epoch1); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	if ok, err := st.RenewAgentSessionLease(ctx, runID, owner2, epoch2, time.Second); err != nil || !ok {
		t.Fatalf("new owner lost after stale release: ok=%v err=%v", ok, err)
	}
}

func TestPostgresChainedAskResumeKeepsDurableSession(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL to run chained ask/resume integration")
	}
	ctx := context.Background()
	st, err := storage.Open(ctx, config.Config{
		PostgresURL: url,
		DuckDBPath:  filepath.Join(t.TempDir(), "chained-ask.duckdb"),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx,
		fmt.Sprintf("chained-ask-%d@example.com", time.Now().UnixNano()),
		"Chained ask", "password1234", "ask workspace", "ask project",
	)
	if err != nil {
		t.Fatal(err)
	}
	const conversationID = "chained-ask-session"
	rootRunID, err := st.CreateAgentRun(ctx, boot.Project.ID, "", "chat", conversationID)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewSessionStore(st)
	if err := adapter.Append(ctx, rootRunID, agentcore.SessionEntry{Kind: agentcore.EntryQuestion, CallID: "question-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkAgentRun(ctx, rootRunID, "first question", 0, 0, 0, false); err != nil {
		t.Fatal(err)
	}

	svc := NewChatService(st, WithSessionStore(adapter))
	step := 0
	var continuationID string
	svc.handle = func(ctx context.Context, req chatWork, _ agentcore.StreamSink) (ChatResult, error) {
		step++
		if req.ResumeFromRunID != rootRunID {
			t.Fatalf("step %d resumed %q, want root durable session %q", step, req.ResumeFromRunID, rootRunID)
		}
		if step == 1 {
			continuationID, err = st.CreateAgentRunWithDurableSession(ctx, boot.Project.ID, req.AgentID, "chat", conversationID, req.ResumeFromRunID)
			if err != nil {
				return ChatResult{}, err
			}
			if err := adapter.Append(ctx, rootRunID, agentcore.SessionEntry{Kind: agentcore.EntryQuestion, CallID: "question-2"}); err != nil {
				return ChatResult{}, err
			}
			if err := st.ParkAgentRun(ctx, continuationID, "second question", 0, 0, 0, false); err != nil {
				return ChatResult{}, err
			}
			return ChatResult{RunID: continuationID, Waiting: true}, nil
		}
		return ChatResult{RunID: uuid.NewString(), Final: "done"}, nil
	}

	opts := AnswerOptions{UserID: boot.User.ID, ProjectID: boot.Project.ID, SessionID: conversationID, CallID: "question-1", Answer: "first answer"}
	first, err := svc.AnswerQuestion(ctx, opts, nil)
	if err != nil || !first.Waiting {
		t.Fatalf("first answer: result=%+v err=%v", first, err)
	}
	waiting, err := st.LatestWaitingRunForSession(ctx, boot.User.ID, boot.Project.ID, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.ID != continuationID || waiting.DurableSessionID != rootRunID {
		t.Fatalf("continuation identity = %+v, want id=%s durable=%s", waiting, continuationID, rootRunID)
	}

	opts.CallID, opts.Answer = "question-2", "second answer"
	second, err := svc.AnswerQuestion(ctx, opts, nil)
	if err != nil || second.Waiting || second.Final != "done" {
		t.Fatalf("second answer: result=%+v err=%v", second, err)
	}
	log, err := adapter.Log(ctx, rootRunID)
	if err != nil {
		t.Fatal(err)
	}
	answers := map[string]int{}
	for _, entry := range log {
		if entry.Kind == agentcore.EntryAnswer {
			answers[entry.CallID]++
		}
	}
	if answers["question-1"] != 1 || answers["question-2"] != 1 {
		t.Fatalf("answer counts = %+v, want exactly one per question", answers)
	}
}
