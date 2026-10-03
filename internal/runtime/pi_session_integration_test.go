//go:build pi || pi_native

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

func piSessionJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func piSessionReply(tools bool) json.RawMessage {
	content := []any{map[string]any{"type": "text", "text": "done"}}
	stop := "stop"
	if tools {
		content = []any{map[string]any{"type": "toolCall", "id": "write-1", "name": "write", "arguments": map[string]any{}}}
		stop = "toolUse"
	}
	return piSessionJSON(map[string]any{"role": "assistant", "content": content, "stopReason": stop,
		"api": "test", "provider": "test", "model": "test", "timestamp": 100,
		"usage": map[string]any{"input": 1, "output": 1, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 2,
			"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}})
}

func piSessionOptions() json.RawMessage {
	return json.RawMessage(`{"initialState":{"systemPrompt":"session test","tools":[{"name":"write","label":"Write","description":"write once","parameters":{"type":"object","properties":{},"additionalProperties":false}}]}}`)
}

func piSessionWorker(t *testing.T) string {
	t.Helper()
	if piTestNative {
		return ""
	}
	p, err := filepath.Abs("../../third_party/pi/dist/worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("Build Pi first: make pi-build")
	}
	return p
}

func piSessionContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPiSessionNativeRoundTripAndPolicy(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			ctx := piSessionContext(t)
			store := agentcore.NewMemorySessionStore()
			var streams, effects atomic.Int32
			var mu sync.Mutex
			var events []string
			cfg := PiSessionConfig{Store: store, SessionID: "native", Pi: agentcore.PiConfig{
				Worker: piSessionWorker(t), Options: piSessionOptions(),
				Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
					if method == "stream" {
						return piSessionReply(streams.Add(1) == 1), nil
					}
					if method == "tool" {
						effects.Add(1)
						return json.RawMessage(`{"content":[{"type":"text","text":"written > &"}],"details":{"opaque":"keep"}}`), nil
					}
					return nil, fmt.Errorf("unexpected callback %s", method)
				},
				OnEvent: func(_ context.Context, raw json.RawMessage) error {
					mu.Lock()
					defer mu.Unlock()
					events = append(events, string(raw))
					return nil
				},
			}}
			if allowed {
				cfg.Policy = agentcore.NewAllowList("write")
			}
			s, err := NewPiSession(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if err := s.Prompt(ctx, piSessionJSON("write")); err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if allowed {
				want = 1
			}
			if effects.Load() != want {
				t.Fatalf("effects=%d allowed=%v", effects.Load(), allowed)
			}
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			entries, _ := store.Log(ctx, "native")
			mu.Lock()
			for _, entry := range entries {
				if entry.Kind == piEventEntry {
					found := false
					for _, event := range events {
						if event == entry.Content {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("native event changed during storage: %s", entry.Content)
					}
				}
			}
			mu.Unlock()
			cfg.Resume = true
			resumed, err := NewPiSession(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			after, err := resumed.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var first, second map[string]any
			_ = json.Unmarshal(before, &first)
			_ = json.Unmarshal(after, &second)
			if !reflect.DeepEqual(first["messages"], second["messages"]) {
				t.Fatalf("resume changed transcript\nbefore %s\nafter %s", before, after)
			}
			if effects.Load() != want || streams.Load() != 2 {
				t.Fatal("resume repeated completed work")
			}
		})
	}
}

type piFailStore struct {
	*agentcore.MemorySessionStore
	kind agentcore.SessionEntryKind
}

func (s *piFailStore) Append(ctx context.Context, id string, entry agentcore.SessionEntry) error {
	if entry.Kind == s.kind {
		return errors.New("injected durable write failure")
	}
	return s.MemorySessionStore.Append(ctx, id, entry)
}

func TestPiSessionDoesNotExecuteBeforeDurableIntent(t *testing.T) {
	ctx := piSessionContext(t)
	store := &piFailStore{MemorySessionStore: agentcore.NewMemorySessionStore(), kind: piEffectStart}
	var effects atomic.Int32
	s, err := NewPiSession(ctx, PiSessionConfig{Store: store, SessionID: "failing", Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{
		Worker: piSessionWorker(t), Options: piSessionOptions(),
		Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "stream" {
				return piSessionReply(true), nil
			}
			effects.Add(1)
			return json.RawMessage(`{"content":[],"details":{}}`), nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Prompt(ctx, piSessionJSON("write")); err == nil {
		t.Fatal("write failure reported success")
	}
	if effects.Load() != 0 {
		t.Fatal("tool executed without durable intent")
	}
	if err := s.Prompt(ctx, piSessionJSON("retry")); err == nil {
		t.Fatal("faulted session accepted more work")
	}
}

func TestPiSessionInterruptedEffectCannotBeReplayed(t *testing.T) {
	ctx := piSessionContext(t)
	store := agentcore.NewMemorySessionStore()
	started, stopped := make(chan struct{}), make(chan struct{})
	var effects atomic.Int32
	cfg := PiSessionConfig{Store: store, SessionID: "interrupted", Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{
		Worker: piSessionWorker(t), Options: piSessionOptions(),
		Callback: func(ctx context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "stream" {
				return piSessionReply(true), nil
			}
			effects.Add(1)
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		},
	}}
	s, err := NewPiSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	done := make(chan error, 1)
	go func() { done <- s.Prompt(ctx, piSessionJSON("write")) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_ = s.Close()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("host callback leaked")
	}
	if err := <-done; err == nil {
		t.Fatal("interrupted run reported success")
	}
	cfg.Resume = true
	_, err = NewPiSession(ctx, cfg)
	if !errors.Is(err, ErrPiUnsettledEffect) {
		t.Fatalf("recovery error: %v", err)
	}
	if effects.Load() != 1 {
		t.Fatal("replayed ambiguous external write")
	}
}

func TestPiSessionLeaseExcludesConcurrentOwners(t *testing.T) {
	ctx := piSessionContext(t)
	cfg := PiSessionConfig{Store: agentcore.NewMemorySessionStore(), SessionID: "owned", Pi: agentcore.PiConfig{Worker: piSessionWorker(t)}}
	s, err := NewPiSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	cfg.Resume = true
	_, err = NewPiSession(other, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second owner acquired lease: %v", err)
	}
	_ = s.Close()
	next, err := NewPiSession(ctx, cfg)
	if err != nil {
		t.Fatalf("Close leaked lease: %v", err)
	}
	_ = next.Close()
}

func TestPiSessionRecoveryAtWorkerPersistenceBoundaries(t *testing.T) {
	ctx := piSessionContext(t)
	store := agentcore.NewMemorySessionStore()
	var streams atomic.Int32
	cfg := PiSessionConfig{Store: store, SessionID: "boundaries", Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{
		Worker: piSessionWorker(t), Options: piSessionOptions(),
		Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "stream" {
				return piSessionReply(streams.Add(1) == 1), nil
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"written"}],"details":{"opaque":"receipt"}}`), nil
		},
	}}
	s, err := NewPiSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Prompt(ctx, piSessionJSON("write")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	entries, _ := store.Log(ctx, cfg.SessionID)
	for i, entry := range entries {
		var event struct {
			Type    string
			Message json.RawMessage
		}
		_ = json.Unmarshal([]byte(entry.Content), &event)
		var message struct{ Role string }
		_ = json.Unmarshal(event.Message, &message)
		wantError := entry.Kind == piEffectDone || event.Type == "tool_execution_end"
		wantResult := (event.Type == "message_start" || event.Type == "message_end") && message.Role == "toolResult"
		if !wantError && !wantResult {
			continue
		}
		t.Run(fmt.Sprintf("%d_%s_%s", i, entry.Kind, event.Type), func(t *testing.T) {
			prefix := agentcore.NewMemorySessionStore()
			for _, record := range entries[:i+1] {
				if err := prefix.Append(ctx, cfg.SessionID, record); err != nil {
					t.Fatal(err)
				}
			}
			resumeCfg := cfg
			resumeCfg.Store, resumeCfg.Resume = prefix, true
			var calls atomic.Int32
			resumeCfg.Pi.Callback = func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
				calls.Add(1)
				return nil, errors.New("recovery must not invoke callbacks")
			}
			recovered, err := NewPiSession(ctx, resumeCfg)
			if wantError {
				if !errors.Is(err, ErrPiUnsettledEffect) {
					t.Fatalf("incomplete native message accepted: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer recovered.Close()
				raw, err := recovered.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var state struct{ Messages []json.RawMessage }
				_ = json.Unmarshal(raw, &state)
				if len(state.Messages) != 4 || string(state.Messages[3]) != string(event.Message) {
					t.Fatalf("lost exact native result: %s", raw)
				}
			}
			if calls.Load() != 0 {
				t.Fatal("resume performed provider or tool work")
			}
		})
	}
}

func TestPiSessionRejectsRevisionDrift(t *testing.T) {
	ctx := piSessionContext(t)
	for _, revision := range []string{"", "different-upstream-revision"} {
		t.Run(revision, func(t *testing.T) {
			store := agentcore.NewMemorySessionStore()
			_ = store.Append(ctx, "revision", agentcore.SessionEntry{Kind: piStateEntry, Model: revision, Content: `{"messages":[]}`})
			_, err := NewPiSession(ctx, PiSessionConfig{Store: store, SessionID: "revision", Resume: true, Pi: agentcore.PiConfig{Worker: piSessionWorker(t)}})
			if err == nil || !strings.Contains(err.Error(), "revision") {
				t.Fatalf("accepted mismatched revision: %v", err)
			}
			entries, _ := store.Log(ctx, "revision")
			if len(entries) != 1 {
				t.Fatal("failed resume changed session")
			}
		})
	}
}

type piRuntimeDenyPolicy struct{ agentcore.Policy }

func (piRuntimeDenyPolicy) Allow(context.Context, agentcore.ToolCall) agentcore.Decision {
	return agentcore.Blocked("permission revoked")
}

func TestPiSessionRechecksPermissionBeforeExecution(t *testing.T) {
	ctx := piSessionContext(t)
	var effects, streams atomic.Int32
	s, err := NewPiSession(ctx, PiSessionConfig{Policy: piRuntimeDenyPolicy{agentcore.NewAllowList("write")}, Pi: agentcore.PiConfig{
		Worker: piSessionWorker(t), Options: piSessionOptions(),
		Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "stream" {
				return piSessionReply(streams.Add(1) == 1), nil
			}
			effects.Add(1)
			return json.RawMessage(`{"content":[]}`), nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Prompt(ctx, piSessionJSON("write")); err != nil {
		t.Fatal(err)
	}
	state, _ := s.State(ctx)
	if effects.Load() != 0 || !strings.Contains(string(state), "permission revoked") {
		t.Fatalf("execution-time policy bypassed: %s", state)
	}
}

type piComposedTool struct{ calls *atomic.Int32 }

func (piComposedTool) Name() string { return "write" }
func (piComposedTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: "write", Description: "composed server tool", Parameters: map[string]any{"type": "object"}}
}
func (t piComposedTool) Run(context.Context, string) (string, error) {
	t.calls.Add(1)
	return "production tool boundary", nil
}

func TestPiSessionUsesProductionToolComposition(t *testing.T) {
	ctx := piSessionContext(t)
	var effects, streams atomic.Int32
	p := representativeBuildParams()
	p.Tools = []agentcore.Tool{piComposedTool{&effects}}
	p.Sandbox = nil
	p.HTTPTool = nil
	p.Subagents = nil
	composed, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	host, err := composed.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	definitions, err := host.Definitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var options map[string]any
	_ = json.Unmarshal(piSessionOptions(), &options)
	options["initialState"].(map[string]any)["tools"] = definitions
	s, err := NewPiSession(ctx, PiSessionConfig{Store: p.Session, SessionID: p.SessionID, Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{
		Worker: piSessionWorker(t), Options: piSessionJSON(options),
		Callback: func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "stream" {
				return piSessionReply(streams.Add(1) == 1), nil
			}
			if method != "tool" {
				return nil, fmt.Errorf("unexpected callback %s", method)
			}
			value, audit, err := host.Execute(ctx, params, emit)
			if audit.Parked || len(audit.AdditionalContexts) > 0 {
				return nil, errors.New("unexpected workflow control from test tool")
			}
			return value, err
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Prompt(ctx, piSessionJSON("execute composed tool")); err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 1 || streams.Load() != 2 || !strings.Contains(string(state), "production tool boundary") || !strings.Contains(string(state), `"idempotency_key"`) {
		t.Fatalf("Pi did not execute the composed server tool: effects=%d streams=%d state=%s", effects.Load(), streams.Load(), state)
	}
}
