package agentruntime

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/agentcore/plugins/subagent"
	"github.com/2found/2ai/ai"
)

func TestNativeLadderForkOwnsSelectionAndJSON(t *testing.T) {
	parent := testNativeLadder(t)
	first, second := parent.fork(), parent.fork()
	persist := func(context.Context, nativeLadderSelection) error { return nil }
	if err := parent.selectRung(context.Background(), 0, 1, persist); err != nil {
		t.Fatal(err)
	}
	if first.selection().Rung != 0 || second.selection().Generation != 0 {
		t.Fatal("parent selection leaked into child")
	}
	if err := first.selectRung(context.Background(), 0, 1, persist); err != nil {
		t.Fatal(err)
	}
	if err := parent.selectRung(context.Background(), 1, 0, persist); err != nil {
		t.Fatal(err)
	}
	if first.selection().Rung != 1 || first.selection().Generation != 1 || second.selection().Rung != 0 {
		t.Fatal("sibling selection leaked")
	}
	first.rungs[0].model[0] = '!'
	first.rungs[0].config.Options[0] = '!'
	if !json.Valid(parent.rungs[0].model) || !json.Valid(second.rungs[0].config.Options) {
		t.Fatal("child JSON aliases another owner")
	}
	late := parent.fork()
	if late.selection().Generation != 0 || late.selection().Rung != 0 {
		t.Fatal("new child inherited parent's journal generation")
	}
}

func TestNativeForkRunnerUsesIndependentLadderBindings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := agentcore.NewMemorySessionStore()
	parent, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(), Model: "test", Session: store, SessionID: "fork-parent"})
	if err != nil {
		t.Fatal(err)
	}
	ladder := testNativeLadder(t)
	var primary, fallback, traces atomic.Int32
	for i := range ladder.rungs {
		index := i
		ladder.rungs[i].stream = func(_ context.Context, model json.RawMessage, _ ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
			if index != 0 {
				fallback.Add(1)
			} else {
				primary.Add(1)
			}
			if !nativeModelIdentityEqual(model, ladder.rungs[0].model) || options["apiKey"] != "secret-a-fresh" {
				t.Error("child borrowed parent's selected model/key")
			}
			return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "child complete"}), nil
		}
	}
	binding, known, stream := ladder.sessionBinding()
	binding.OnTrace = func(context.Context, json.RawMessage) { traces.Add(1) }
	fork := piForkRunner(PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder}, known, store, agentcore.ToolChoice{}, nil)
	// Switch after the fork closure is created: it must not capture the parent's
	// mutable dispatcher or inherit the parent's durable generation.
	if err := ladder.selectRung(ctx, 0, 1, func(context.Context, nativeLadderSelection) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"fork-parent/first", "fork-parent/second"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			req := subagent.ForkRequest{SessionID: id, Task: "complete child", Prompt: "complete child"}
			result, err := fork(ctx, parent.Fork(id), req, nil)
			if err != nil || result.Final != "child complete" {
				t.Errorf("child failed: %v final=%q", err, result.Final)
			}
		}(id)
	}
	wg.Wait()
	if primary.Load() != 2 || fallback.Load() != 0 || traces.Load() != 2 {
		t.Fatalf("child routing: primary=%d fallback=%d trace=%d", primary.Load(), fallback.Load(), traces.Load())
	}
	if ladder.selection().Rung != 1 || ladder.selection().Generation != 1 {
		t.Fatal("child rewrote parent's selection")
	}
}
