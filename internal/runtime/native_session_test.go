package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/ask"
	"strings"
)

const nativeSessionOptions = `{"initialState":{"model":{"id":"test","api":"test","provider":"test"},"tools":[{"name":"write","label":"Write","description":"Write once","parameters":{"type":"object","properties":{},"additionalProperties":false}}]}}`

func nativeSessionToolReply() json.RawMessage {
	var message map[string]json.RawMessage
	_ = json.Unmarshal([]byte(nativeStreamReply), &message)
	message["content"] = json.RawMessage(`[{"type":"toolCall","id":"write-1","name":"write","arguments":{}}]`)
	message["stopReason"] = json.RawMessage(`"toolUse"`)
	raw, _ := json.Marshal(message)
	return raw
}

func TestNativeSessionDurableRoundTripAndPolicy(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			store := agentcore.NewMemorySessionStore()
			var streams, effects atomic.Int32
			cfg := PiSessionConfig{NativeGo: true, Store: store, SessionID: "go-session", Pi: agentcore.PiConfig{
				// Deliberately unusable paths prove this path cannot launch the worker.
				Runtime: "/missing/runtime", Worker: "/missing/worker", Options: json.RawMessage(nativeSessionOptions),
				Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
					switch method {
					case "stream":
						if streams.Add(1) == 1 {
							return nativeSessionToolReply(), nil
						}
						return json.RawMessage(nativeStreamReply), nil
					case "tool":
						effects.Add(1)
						return json.RawMessage(`{"content":[{"type":"text","text":"written > &"}],"details":{"opaque":"signature"}}`), nil
					default:
						return nil, fmt.Errorf("unexpected callback %s", method)
					}
				},
			}}
			if allowed {
				cfg.Policy = agentcore.NewAllowList("write")
			}
			session, err := NewPiSession(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Prompt(ctx, json.RawMessage(`"write"`)); err != nil {
				t.Fatal(err)
			}
			state, err := session.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if allowed {
				want = 1
			}
			if effects.Load() != want {
				t.Fatalf("effects=%d allowed=%v", effects.Load(), allowed)
			}
			entries, err := store.Log(ctx, cfg.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			starts, receipts := 0, 0
			for _, entry := range entries {
				if entry.Kind == piEffectStart {
					starts++
				}
				if entry.Kind == piEffectDone {
					receipts++
				}
			}
			if starts != int(want) || receipts != starts {
				t.Fatalf("effect journal mismatch: starts=%d receipts=%d", starts, receipts)
			}
			cfg.Resume = true
			resumed, err := NewPiSession(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			restored, err := resumed.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var original, next struct{ Messages []json.RawMessage }
			_ = json.Unmarshal(state, &original)
			_ = json.Unmarshal(restored, &next)
			var a, b any
			originalJSON, _ := json.Marshal(original.Messages)
			nextJSON, _ := json.Marshal(next.Messages)
			_ = json.Unmarshal(originalJSON, &a)
			_ = json.Unmarshal(nextJSON, &b)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("native transcript changed on resume:\n%s\n%s", originalJSON, nextJSON)
			}
			if effects.Load() != want {
				t.Fatal("resume replayed a completed effect")
			}
		})
	}
}

func TestNativeSessionCancellationRetainsUnsettledEffect(t *testing.T) {
	life, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	run, cancel := context.WithCancel(life)
	defer cancel()
	store := agentcore.NewMemorySessionStore()
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	cfg := PiSessionConfig{NativeGo: true, Store: store, SessionID: "unsettled", Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{Options: json.RawMessage(nativeSessionOptions), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method == "stream" {
			return nativeSessionToolReply(), nil
		}
		if method == "tool" {
			close(entered)
			<-release
			defer close(returned)
			return json.RawMessage(`{"content":[]}`), nil
		}
		return nil, fmt.Errorf("unexpected %s", method)
	}}}
	session, err := NewPiSession(life, cfg)
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() { completed <- session.Prompt(run, json.RawMessage(`"write"`)) }()
	select {
	case <-entered:
	case <-life.Done():
		t.Fatal(life.Err())
	}
	cancel()
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("cancelled run succeeded")
		}
	case <-life.Done():
		t.Fatal(life.Err())
	}
	if _, err := session.agent.Call(life, "waitForIdle", nil); err != nil {
		t.Fatal(err)
	}
	finish()
	select {
	case <-returned:
	case <-life.Done():
		t.Fatal(life.Err())
	}
	_ = session.Close()
	cfg.Resume = true
	if _, err := NewPiSession(life, cfg); !errors.Is(err, ErrPiUnsettledEffect) {
		t.Fatalf("unsafe effect resumed: %v", err)
	}
}

func TestNativeSessionCustomMetadataSurvivesJournal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := agentcore.NewMemorySessionStore()
	cfg := PiSessionConfig{NativeGo: true, Store: store, SessionID: "metadata", Pi: agentcore.PiConfig{Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		return json.RawMessage(nativeStreamReply), nil
	}}}
	session, err := NewPiSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	prompt := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hello","textSignature":"sig"}],"timestamp":12,"opaque":{"large":9007199254740993,"escaped":"<&>"}}`)
	if err := session.Prompt(ctx, prompt); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	cfg.Resume = true
	resumed, err := NewPiSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	state, err := resumed.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct{ Messages []map[string]json.RawMessage }
	if err := json.Unmarshal(state, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Messages[0]["opaque"]) != `{"large":9007199254740993,"escaped":"\u003c\u0026\u003e"}` {
		t.Fatalf("metadata changed: %s", stored.Messages[0]["opaque"])
	}
}

func TestNativeRunParksAndResumesHumanAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := agentcore.NewMemorySessionStore()
	var requests atomic.Int32
	run := func(resume bool) (PiRunResult, error) {
		composed, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(), Model: "test", Tools: agentcore.NewToolSet(ask.Tool{}), Policy: agentcore.NewAllowList("ask"), Session: store, SessionID: "go-ask", ResumeSession: resume})
		if err != nil {
			return PiRunResult{}, err
		}
		host, err := composed.OpenPiTools(ctx)
		if err != nil {
			return PiRunResult{}, err
		}
		defer host.Close()
		cfg := PiRunConfig{Host: host, Session: PiSessionConfig{NativeGo: true, Store: store, SessionID: "go-ask", Resume: resume, Policy: agentcore.NewAllowList("ask"), Pi: agentcore.PiConfig{Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method != "stream" {
				return nil, fmt.Errorf("unexpected callback %s", method)
			}
			if requests.Add(1) == 1 {
				var reply map[string]json.RawMessage
				_ = json.Unmarshal(nativeSessionToolReply(), &reply)
				reply["content"] = json.RawMessage(`[{"type":"toolCall","id":"question-1","name":"ask","arguments":{"Question":"  Which tier?  ","options":[{"label":" Pro "}]}}]`)
				return json.Marshal(reply)
			}
			if strings.Count(string(params), "agentrayAnswerId") != 1 || !strings.Contains(string(params), "Human answer to question") {
				return nil, errors.New("answer was lost or duplicated")
			}
			return json.RawMessage(nativeStreamReply), nil
		}}}}
		if !resume {
			cfg.Input = json.RawMessage(`"choose a tier"`)
		}
		return RunPi(ctx, cfg)
	}
	first, err := run(false)
	if err != nil || !first.Projection.Parked || requests.Load() != 1 {
		t.Fatalf("park failed: err=%v result=%+v requests=%d", err, first.Projection, requests.Load())
	}
	entries, err := store.Log(ctx, "go-ask")
	if err != nil {
		t.Fatal(err)
	}
	id, _, ok := agentcore.PendingQuestion(entries)
	if !ok {
		t.Fatal("question not recorded")
	}
	unanswered, err := run(true)
	if err != nil || !unanswered.Projection.Parked || requests.Load() != 1 {
		t.Fatalf("unanswered resume called provider: %v requests=%d", err, requests.Load())
	}
	lease, release, err := agentcore.AcquireSessionLease(ctx, store, "go-ask")
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := agentcore.RecordSessionAnswer(lease, store, "go-ask", id, "Pro")
	releaseErr := release()
	if err != nil || !recorded || releaseErr != nil {
		t.Fatalf("answer failed: %v %v %v", recorded, err, releaseErr)
	}
	final, err := run(true)
	if err != nil || final.Projection.Parked || final.Projection.Final != "done" || requests.Load() != 2 {
		t.Fatalf("answered resume failed: err=%v result=%+v requests=%d", err, final.Projection, requests.Load())
	}
	if strings.Count(string(final.State), "agentrayAnswerId") != 1 {
		t.Fatal("answer not delivered exactly once")
	}
}
