//go:build pi

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

const piCompactionRevision = "eeac84ca92498ac18b6832754d01aef1d3c5f654"

func TestPiCompactionUsesNativeProviderWithoutHistoricalTools(t *testing.T) {
	for _, finish := range []string{"stop", "length", "tool_calls"} {
		t.Run(finish, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body struct {
					Messages []struct {
						Role    string
						Content json.RawMessage
					}
					Tools     []json.RawMessage
					MaxTokens int `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body.Tools) != 0 || body.MaxTokens != 1024 || r.Header.Get("Authorization") != "Bearer refreshed" {
					t.Errorf("summary policy missing: %+v", body)
				}
				raw := string(piSessionJSON(body.Messages))
				if !strings.Contains(raw, "running summary") || !strings.Contains(raw, "old question") || !strings.Contains(raw, "cGk=") {
					t.Errorf("summary lost native source: %s", raw)
				}
				for _, m := range body.Messages {
					if m.Role == "system" || m.Role == "developer" {
						if strings.Contains(string(m.Content), "host policy") {
							t.Error("historical instructions remained active")
						}
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if finish == "tool_calls" {
					piChildSSE(w, "write", "{}", "")
					return
				}
				fmt.Fprintf(w, "data: {\"id\":\"summary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Saved decisions\"},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", finish)
			}))
			defer server.Close()
			tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "summary", BaseURL: server.URL + "/v1", APIKey: "stale"}}
			var source []json.RawMessage
			for _, raw := range piCompactionFixture() {
				source = append(source, json.RawMessage(raw))
			}
			summary, err := summarizePiHistory(piSessionContext(t), PiRuntimeConfig{Worker: piSessionWorker(t)}, tier, piSessionJSON(source), piCompactionRevision, func(context.Context, string) (string, error) { return "refreshed", nil }, nil)
			if finish == "stop" {
				if err != nil || summary != "Saved decisions" {
					t.Fatalf("native summary: %q %v", summary, err)
				}
			} else if err == nil || summary != "" {
				t.Fatalf("accepted incomplete summary: %q %v", summary, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("summary ran %d provider calls", calls.Load())
			}
		})
	}
}

func TestPiCompactionSQLBranchesRacesAndNextNativeTurn(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL for native compaction SQL verification")
	}
	ctx := piSessionContext(t)
	st, err := storage.Open(ctx, config.Config{PostgresURL: url, DuckDBPath: filepath.Join(t.TempDir(), "compaction.duckdb")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx, "pi-compaction-"+uuid.NewString()+"@test.local", "Pi compaction", "password1234", "Pi compaction", "Pi compaction")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, boot.User.ID, boot.Project.ID, "", "Native compaction")
	if err != nil {
		t.Fatal(err)
	}
	seed := piConvDelta("", "", piCompactionRevision, piCompactionFixture()...)
	seed.ConversationID = conv.ID
	seed, err = st.AppendConversationEntryAtLeaf(ctx, seed, "")
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildPiHistory(ctx, st, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	summaryCalls := 0
	summarize := func(_ context.Context, raw json.RawMessage, rev string) (string, error) {
		summaryCalls++
		if !strings.Contains(string(raw), "old answer") || strings.Contains(string(raw), "recent question") || rev != piCompactionRevision {
			t.Fatalf("wrong summary source: %s (%s)", raw, rev)
		}
		return "Older decision saved.", nil
	}
	done, err := compactPiConversation(ctx, st, conv.ID, 0, true, summarize)
	if err != nil || !done || summaryCalls != 1 {
		t.Fatalf("SQL compaction: %v %v", done, err)
	}
	compacted, err := BuildPiHistory(ctx, st, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(piConvMessages(t, compacted)) != 7 || strings.Contains(string(compacted.Messages), "old answer") || !strings.Contains(string(compacted.Messages), "opaque-signature") {
		t.Fatalf("SQL changed native tail: %s", compacted.Messages)
	}
	done, err = compactPiConversation(ctx, st, conv.ID, 0, true, summarize)
	if err != nil || done || summaryCalls != 1 {
		t.Fatal("repeated compaction spent a summary call")
	}
	if _, err := buildPiResumeHistory(ctx, st, conv.ID); err == nil {
		t.Fatal("resume crossed compaction into obsolete durable history")
	}
	stale := agentcore.RunResult{NativeRevision: piCompactionRevision, NativeState: piSessionJSON(map[string]any{"messages": base.Messages})}
	if err := appendPiConversationTurn(ctx, st, conv.ID, "", "", base, stale); !errors.Is(err, storage.ErrConversationLeafChanged) {
		t.Fatalf("stale turn published over compaction: %v", err)
	}
	// Send the folded checkpoint through the original provider on the next turn.
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		var body struct{ Messages json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw := string(body.Messages)
		for _, want := range []string{"Older decision saved.", "recent question", "native result", "next question"} {
			if !strings.Contains(raw, want) {
				t.Errorf("provider missing %q: %s", want, raw)
			}
		}
		if strings.Contains(raw, "old answer") {
			t.Error("provider replayed compacted history")
		}
		piChildSSE(w, "", "", "next answer")
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "after-compaction", BaseURL: server.URL + "/v1", APIKey: "test"}}
	user, err := AppendMessageEntryAtLeaf(ctx, st, conv.ID, "user", "next question", "", boot.User.ID, compacted.LeafID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "next question", InputID: user.ID, NativeHistory: compacted.Messages, NativeHistoryRevision: compacted.Revision}, tier, nil)
	if err != nil || result.Final != "next answer" || providerCalls.Load() != 1 {
		t.Fatalf("post-compaction run: %+v %v", result, err)
	}
	if err := appendPiConversationTurn(ctx, st, conv.ID, "", "", compacted, result); err != nil {
		t.Fatal(err)
	}
	continued, err := BuildPiHistory(ctx, st, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	var native struct{ Messages json.RawMessage }
	_ = json.Unmarshal(result.NativeState, &native)
	if !samePiJSON(native.Messages, continued.Messages) {
		t.Fatal("post-compaction native delta did not round trip")
	}
	// A new input during summarization wins. The summary cannot consume it or
	// create a checkpoint on an unexpected branch.
	done, err = compactPiConversation(ctx, st, conv.ID, 0, true, func(context.Context, json.RawMessage, string) (string, error) {
		_, err := AppendMessageEntry(ctx, st, conv.ID, "user", "racing input", "", boot.User.ID, "", 0)
		return "stale summary", err
	})
	if done || !errors.Is(err, storage.ErrConversationLeafChanged) {
		t.Fatalf("stale summary committed: %v %v", done, err)
	}
	latest, err := BuildPiHistory(ctx, st, conv.ID)
	if err != nil || !strings.Contains(string(latest.Messages), "racing input") || strings.Contains(string(latest.Messages), "stale summary") {
		t.Fatalf("racing input lost: %s %v", latest.Messages, err)
	}
	// Branching back recovers every original block. No old entry was rewritten.
	if err := st.SetConversationLeaf(ctx, conv.ID, seed.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := BuildPiHistory(ctx, st, conv.ID)
	if err != nil || !samePiJSON(restored.Messages, base.Messages) {
		t.Fatal("compaction destroyed old branch")
	}
	for _, failure := range []error{context.Canceled, errors.New("provider failure"), nil} {
		done, err := compactPiConversation(ctx, st, conv.ID, 0, true, func(context.Context, json.RawMessage, string) (string, error) { return "", failure })
		if done || err == nil {
			t.Fatal("failed/empty summary published")
		}
		after, err := BuildPiHistory(ctx, st, conv.ID)
		if err != nil || after.LeafID != base.LeafID {
			t.Fatal("summary failure moved branch")
		}
	}
}

func TestPiCompactCommandResolvesWorkspaceTierAndPersistsNativeCheckpoint(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGENTRAY_TEST_DATABASE_URL for native compaction command verification")
	}
	ctx := piSessionContext(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model    string
			Messages json.RawMessage
			Tools    []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "workspace-summary" || len(body.Tools) != 0 || r.Header.Get("Authorization") != "Bearer workspace-key" {
			t.Errorf("workspace tier not honored: %+v", body)
		}
		if !strings.Contains(string(body.Messages), "old question") || strings.Contains(string(body.Messages), "recent question") {
			t.Errorf("wrong compacted prefix: %s", body.Messages)
		}
		piChildSSE(w, "", "", "Remember the earlier decision.")
	}))
	defer server.Close()
	st, err := storage.Open(ctx, config.Config{PostgresURL: url, DuckDBPath: filepath.Join(t.TempDir(), "command.duckdb"), DefaultModelProvider: "openai", DefaultModelBaseURL: server.URL + "/v1", DefaultModelAPIKey: "workspace-key", DefaultModelFlash: "workspace-summary", DefaultModelLite: "workspace-summary", DefaultModelPro: "workspace-summary"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	boot, err := st.CreateAccount(ctx, "pi-command-"+uuid.NewString()+"@test.local", "Pi command", "password1234", "Pi command", "Pi command")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAgentConfig(ctx, boot.User.ID, boot.Project.ID, storage.AgentConfigInput{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, boot.User.ID, boot.Project.ID, "", "Native command")
	if err != nil {
		t.Fatal(err)
	}
	seed := piConvDelta("", "", piCompactionRevision, piCompactionFixture()...)
	seed.ConversationID = conv.ID
	if _, err := st.AppendConversationEntryAtLeaf(ctx, seed, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendCommandEntry(ctx, st, conv.ID, "user", "/compact", "", boot.User.ID); err != nil {
		t.Fatal(err)
	}
	svc := NewChatService(st, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	svc.classify = func(context.Context, string, []agentcore.Message, string) (chatDecision, error) {
		t.Fatal("compact invoked legacy classifier")
		return chatDecision{}, nil
	}
	result, err := svc.Chat(ctx, ChatOptions{ProjectID: boot.Project.ID, ConversationID: conv.ID, Message: "/compact"}, nil)
	if err != nil || !strings.HasPrefix(result.Final, "Compacted.") || calls.Load() != 1 {
		t.Fatalf("command did not compact: %+v %v calls=%d", result, err, calls.Load())
	}
	history, err := BuildPiHistory(ctx, st, conv.ID)
	if err != nil || !strings.Contains(string(history.Messages), "Remember the earlier decision.") || strings.Contains(string(history.Messages), "/compact") || strings.Contains(string(history.Messages), "Compacted.") {
		t.Fatalf("command display entered model history: %s %v", history.Messages, err)
	}
	result, err = svc.Chat(ctx, ChatOptions{ProjectID: boot.Project.ID, ConversationID: conv.ID, Message: "/compact"}, nil)
	if err != nil || !strings.HasPrefix(result.Final, "Nothing new") || calls.Load() != 1 {
		t.Fatalf("repeated command spent a call: %+v %v", result, err)
	}
}
