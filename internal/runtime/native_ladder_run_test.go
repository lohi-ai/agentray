package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeLadderRunHTTPRetryEscalationAndRetention(t *testing.T) {
	var primary, fallback atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model == "primary" {
			primary.Add(1)
			if r.Header.Get("Authorization") != "Bearer primary-key" {
				t.Error("wrong primary credential")
			}
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		fallback.Add(1)
		if body.Model != "fallback" || r.Header.Get("Authorization") != "Bearer fallback-key" {
			t.Error("wrong fallback binding")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"answer\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fallback answer\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", ProviderID: "primary-row", Model: "primary", BaseURL: server.URL, APIKey: "primary-key", Fallback: &TierConfig{Provider: "openai", ProviderID: "fallback-row", Model: "fallback", BaseURL: server.URL, APIKey: "fallback-key"}}}
	ladder, err := newNativeModelLadder(tier, NativeAgentConfig{}, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	store := agentcore.NewMemorySessionStore()
	binding, _, stream := ladder.sessionBinding()
	var traceMu sync.Mutex
	var traces []json.RawMessage
	binding.OnTrace = func(_ context.Context, raw json.RawMessage) {
		traceMu.Lock()
		defer traceMu.Unlock()
		traces = append(traces, append(json.RawMessage(nil), raw...))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, Store: store, SessionID: "ladder-run"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	waits, observed := 0, 0
	run := nativeLadderRun{
		policy: agentcore.RetryPolicy{MaxAttempts: 2, MaxDelay: time.Millisecond},
		open: func(ctx context.Context, rung nativeBoundRung, index, number int) (*ai.AssistantMessageEventStream, error) {
			if index == 1 && ladder.selection().Rung != 0 && fallback.Load() == 0 {
				t.Error("candidate published before response")
			}
			return session.openNativeAttempt(ctx, rung, ai.TranscriptContext{}, map[string]any{"apiKey": "stale-parent-key", "maxRetries": 0})
		},
		wait: func(_ context.Context, d time.Duration) error {
			waits++
			if d != time.Millisecond {
				t.Error("retry hint lost", d)
			}
			return nil
		},
		observe: func(_ context.Context, index int, a ai.FallbackAttempt) error {
			observed++
			if index == 0 && !agentcore.IsRetryable(a.Failure) {
				t.Error("primary HTTP error untyped")
			}
			return nil
		},
	}
	for n := 0; n < 2; n++ {
		out := ai.NewAssistantMessageEventStream()
		result, err := session.runNativeLadder(session.ctx, out, run)
		if err != nil || !result.Committed {
			t.Fatal("ladder did not commit successful rung", err)
		}
		out.End()
		message, err := out.SnapshotResult(ctx)
		if err != nil || message.StopReason != "stop" || message.Model != "fallback" {
			t.Fatal("fallback response lost", message, err)
		}
	}
	session.agent.flushTraces()
	traceMu.Lock()
	if len(traces) != 4 {
		t.Fatalf("expected four attempt traces, got %d", len(traces))
	}
	// Trace recording is passive: the next attempt can begin before the prior
	// attempt's observer enqueues its diagnostic packet. Identity, not callback
	// arrival order, owns attribution.
	seen := map[int]bool{}
	for _, raw := range traces {
		var packet struct {
			RequestID int
			Model     struct{ ID string }
			Response  struct{ StopReason string }
			Attempt   struct {
				Rung       int
				ProviderID string
				Generation uint64
			}
		}
		if err := json.Unmarshal(raw, &packet); err != nil {
			t.Fatal(err)
		}
		if packet.RequestID < 1 || packet.RequestID > 4 || seen[packet.RequestID] {
			t.Fatalf("invalid or duplicate attempt trace identity: %s", raw)
		}
		seen[packet.RequestID] = true
		i := packet.RequestID - 1
		model, stop := "primary", "error"
		if i >= 2 {
			model, stop = "fallback", "stop"
		}
		rung, row, generation := 0, "primary-row", uint64(0)
		if i >= 2 {
			rung, row = 1, "fallback-row"
		}
		if i == 3 {
			generation = 1
		}
		if packet.Attempt.Rung != rung || packet.Attempt.ProviderID != row || packet.Attempt.Generation != generation {
			t.Fatal("attempt row/generation missing", string(raw))
		}
		if packet.Model.ID != model || packet.Response.StopReason != stop {
			t.Fatalf("attempt trace attribution lost: %s", raw)
		}
	}
	traceMu.Unlock()
	if primary.Load() != 2 || fallback.Load() != 2 || waits != 1 || observed != 4 {
		t.Fatalf("wrong attempt ownership: primary=%d fallback=%d waits=%d observed=%d", primary.Load(), fallback.Load(), waits, observed)
	}
	if ladder.selection().Rung != 1 || ladder.selection().Generation != 1 {
		t.Fatal("successful rung not retained")
	}
	entries, err := store.Log(ctx, "ladder-run")
	if err != nil {
		t.Fatal(err)
	}
	selections := 0
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiModelSelection {
			selections++
		}
	}
	if selections != 1 {
		t.Fatal("selection was not committed exactly once", selections)
	}
	if _, err = recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLadderRunStopsAtVisibleContentAndHostFailure(t *testing.T) {
	for _, mode := range []string{"visible", "callback", "cancelled", "append-failed", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ladder := testNativeLadder(t)
			memory := agentcore.NewMemorySessionStore()
			var store agentcore.SessionStore = memory
			if mode == "append-failed" {
				store = &nativeSelectionFailStore{MemorySessionStore: memory, selectionError: errors.New("append failed")}
			}
			binding, _, stream := ladder.sessionBinding()
			session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, Store: store, SessionID: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			out := ai.NewAssistantMessageEventStream()
			calls := []int{}
			terminal := &ai.Message{Role: "assistant", StopReason: "error"}
			run := nativeLadderRun{policy: agentcore.RetryPolicy{MaxAttempts: 1},
				open: func(ctx context.Context, _ nativeBoundRung, index, number int) (*ai.AssistantMessageEventStream, error) {
					calls = append(calls, index)
					events := []ai.AssistantMessageEvent{}
					if mode == "visible" || (mode == "append-failed" && index == 1) {
						events = append(events, ai.AssistantMessageEvent{Type: "toolcall_start", Partial: terminal})
					}
					return attemptFixture(append(events, ai.AssistantMessageEvent{Type: "error", Error: terminal})...)(ctx)
				},
				observe: func(context.Context, int, ai.FallbackAttempt) error {
					if mode == "callback" {
						return errors.New("host observer failed")
					}
					if mode == "cancelled" {
						cancel()
					}
					return nil
				},
			}
			result, err := session.runNativeLadder(session.ctx, out, run)
			if mode == "visible" || mode == "exhausted" {
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if mode == "exhausted" {
					want = 2
				}
				if len(calls) != want || result.Terminal.Error != terminal {
					t.Fatal("wrong escalation/final identity", calls)
				}
			} else {
				if err == nil {
					t.Fatal("host failure did not stop ladder")
				}
				want := 1
				if mode == "append-failed" {
					want = 2
				}
				if len(calls) != want {
					t.Fatal("continued after host failure", calls)
				}
				assertAttemptEmpty(t, out)
			}
			if ladder.selection().Rung != 0 {
				t.Fatal("failed candidate became active")
			}
		})
	}
}

func TestNativeLadderRunCandidateIsolationAndGenerationFence(t *testing.T) {
	for _, mode := range []string{"candidate-isolation", "stale-generation"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			ladder := testNativeLadder(t)
			binding, _, stream := ladder.sessionBinding()
			session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			out := ai.NewAssistantMessageEventStream()
			calls := 0
			run := nativeLadderRun{policy: agentcore.RetryPolicy{MaxAttempts: 2}, wait: func(context.Context, time.Duration) error { return nil },
				open: func(ctx context.Context, rung nativeBoundRung, index, number int) (*ai.AssistantMessageEventStream, error) {
					calls++
					if mode == "stale-generation" {
						if err := session.selectNativeRung(ctx, 0, 1); err != nil {
							return nil, err
						}
					} else if number == 1 {
						rung.model[0] = '!'
						rung.config.Options[0] = '!'
						return nil, &agentcore.ProviderError{Status: 503, Message: "retry"}
					} else if !json.Valid(rung.model) || !json.Valid(rung.config.Options) {
						t.Error("candidate mutation escaped into retry")
					}
					return attemptFixture(ai.AssistantMessageEvent{Type: "text_delta", Delta: "answer"}, ai.AssistantMessageEvent{Type: "done", Message: &ai.Message{Role: "assistant", StopReason: "stop"}})(ctx)
				},
			}
			result, err := session.runNativeLadder(ctx, out, run)
			if mode == "stale-generation" {
				if err == nil || result.Committed || calls != 1 {
					t.Fatal("stale attempt admitted or escalated", calls, err)
				}
				assertAttemptEmpty(t, out)
				if ladder.selection().Rung != 1 || ladder.selection().Generation != 1 {
					t.Fatal("stale attempt overwrote newer selection")
				}
			} else {
				if err != nil || calls != 2 || !result.Committed {
					t.Fatal("isolated retry failed", calls, err)
				}
				if !json.Valid(ladder.rungs[0].model) || !json.Valid(ladder.rungs[0].config.Options) {
					t.Fatal("candidate mutation reached host binding")
				}
			}
		})
	}
}
