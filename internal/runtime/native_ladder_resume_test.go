package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeLadderSessionResumeBindsSelectedRow(t *testing.T) {
	ctx := context.Background()
	store := agentcore.NewMemorySessionStore()
	first := testNativeLadder(t)
	binding, _, stream := first.sessionBinding()
	session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: first, Store: store, SessionID: "resume-ladder"})
	if err != nil {
		t.Fatal(err)
	}
	if err = session.selectNativeRung(session.ctx, 0, 1); err != nil {
		t.Fatal(err)
	}
	// Close does not manufacture a checkpoint: resume must consume the selection.
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := store.Log(ctx, "resume-ladder")
	if err != nil {
		t.Fatal(err)
	}
	if entries[len(entries)-1].Kind != agentcore.EntryPiModelSelection {
		t.Fatal("missing crash boundary")
	}
	fresh := testNativeLadder(t)
	var primary, fallback, wrapped atomic.Int32
	for i := range fresh.rungs {
		index := i
		fresh.rungs[i].stream = func(_ context.Context, model json.RawMessage, _ ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
			if index == 0 {
				primary.Add(1)
				return nil, errors.New("primary should not run")
			}
			fallback.Add(1)
			if options["apiKey"] != "secret-b-fresh" {
				t.Error("request used wrong row credential")
			}
			if !nativeModelIdentityEqual(model, fresh.rungs[1].model) {
				t.Error("request used wrong model")
			}
			var message ai.Message
			if err := json.Unmarshal([]byte(nativeStreamReply), &message); err != nil {
				return nil, err
			}
			out := ai.NewAssistantMessageEventStream()
			out.Push(ai.AssistantMessageEvent{Type: "start", Partial: &message})
			out.Push(ai.AssistantMessageEvent{Type: "done", Reason: message.StopReason, Message: &message})
			out.End()
			return out, nil
		}
	}
	binding, _, stream = fresh.sessionBinding()
	original := binding.Callback
	binding.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		wrapped.Add(1)
		return original(ctx, method, params, emit)
	}
	resumed, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: fresh, Store: store, SessionID: "resume-ladder", Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if err = resumed.Prompt(ctx, json.RawMessage(`"continue"`)); err != nil {
		t.Fatal(err)
	}
	state, err := resumed.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if primary.Load() != 0 || fallback.Load() != 1 || wrapped.Load() == 0 {
		t.Fatalf("binding routing: primary=%d fallback=%d wrappers=%d state=%s", primary.Load(), fallback.Load(), wrapped.Load(), state)
	}
	if fresh.selection().Generation != 1 || fresh.selection().Rung != 1 {
		t.Fatal("resume reset selection")
	}
	if strings.Contains(string(state), "secret-") {
		t.Fatal("credential entered state")
	}
	// A request retaining the former model must fail before dispatch.
	if _, err = stream(ctx, fresh.rungs[0].model, ai.TranscriptContext{}, nil); err == nil {
		t.Fatal("stale request reached selected provider")
	}
	if fallback.Load() != 1 {
		t.Fatal("stale request dispatched")
	}
}

func TestNativeLadderSessionResumeRejectsMissingOrChangedBinding(t *testing.T) {
	ctx := context.Background()
	store := agentcore.NewMemorySessionStore()
	ladder := testNativeLadder(t)
	binding, _, stream := ladder.sessionBinding()
	session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, Store: store, SessionID: "invalid-binding"})
	if err != nil {
		t.Fatal(err)
	}
	if err = session.selectNativeRung(session.ctx, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing", "changed-row", "changed-model"} {
		t.Run(mode, func(t *testing.T) {
			fresh := testNativeLadder(t)
			binding, _, stream := fresh.sessionBinding()
			cfg := PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: fresh, Store: store, SessionID: "invalid-binding", Resume: true}
			switch mode {
			case "missing":
				cfg.nativeLadder = nil
			case "changed-row":
				fresh.rungs[1].providerID = "replaced-row"
			case "changed-model":
				fresh.rungs[1].model = json.RawMessage(`{"id":"replacement","api":"openai-responses","provider":"openai"}`)
			}
			resumed, err := NewPiSession(ctx, cfg)
			if err == nil {
				resumed.Close()
				t.Fatal("unsafe resume accepted")
			}
			if !strings.Contains(err.Error(), "native") {
				t.Fatalf("wrong failure: %v", err)
			}
		})
	}
}

func TestNativeLadderSessionRegistersFallbackCapabilityHooks(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "primary", APIKey: "fixture-key", Fallback: &TierConfig{Model: "fallback", Capabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported}}}}
	ladder, err := newNativeModelLadder(tier, NativeAgentConfig{}, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	binding, _, stream := ladder.sessionBinding()
	session, err := NewPiSession(context.Background(), PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, Policy: agentcore.NewAllowList("write")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err = session.selectNativeRung(session.ctx, 0, 1); err != nil {
		t.Fatal(err)
	}
	result, err := session.callback(session.ctx, "beforeToolCall", json.RawMessage(`{"toolCall":{"id":"one","name":"write"},"args":{}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var decision struct{ Block bool }
	if json.Unmarshal(result, &decision) != nil || !decision.Block {
		t.Fatalf("fallback capability hook bypassed: %s", result)
	}
}
