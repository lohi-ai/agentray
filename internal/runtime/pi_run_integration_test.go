package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestPiRunReturnsServerProjectionAndNativeTranscript(t *testing.T) {
	ctx := piSessionContext(t)
	var effects, streams atomic.Int32
	p := representativeBuildParams()
	p.Tools = []agentcore.Tool{piComposedTool{&effects}}
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
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
	options := piSessionJSON(map[string]any{"initialState": map[string]any{"systemPrompt": "test", "tools": definitions}})
	var events []agentcore.StreamEventType
	result, err := RunPi(ctx, PiRunConfig{
		Input: piSessionJSON("execute"), PricingKnown: true,
		Sink: func(event agentcore.StreamEvent) { events = append(events, event.Type) },
		Session: PiSessionConfig{Store: p.Session, SessionID: p.SessionID, Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{
			Options: options,
			Callback: func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
				if method == "stream" {
					if streams.Add(1) == 1 {
						return piSessionReply(true), nil
					}
					var reply map[string]any
					_ = json.Unmarshal(piSessionReply(false), &reply)
					reply["content"] = []any{map[string]any{"type": "thinking", "thinking": "reasoning", "thinkingSignature": "preserve-native-signature"}, map[string]any{"type": "text", "text": "done"}}
					reply["extension"] = map[string]any{"native": "kept"}
					return piSessionJSON(reply), nil
				}
				if method != "tool" {
					return nil, fmt.Errorf("unexpected %s", method)
				}
				value, audit, err := host.Execute(ctx, params, emit)
				if audit.Parked || len(audit.AdditionalContexts) > 0 {
					return nil, errors.New("unexpected tool control")
				}
				return value, err
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := result.Projection
	if got.Final != "done" || got.Turns != 2 || got.StopReason != "stop" || got.Usage.InputTokens != 2 || got.Usage.OutputTokens != 2 || got.Usage.CostUnpriced {
		t.Fatalf("incorrect server result: %+v", got)
	}
	if len(got.Tools) != 1 || !got.Tools[0].Allowed || effects.Load() != 1 {
		t.Fatalf("lost server tool trace: %+v", got.Tools)
	}
	if !strings.Contains(string(result.State), "preserve-native-signature") || !strings.Contains(string(result.State), `"extension":{"native":"kept"}`) {
		t.Fatalf("native transcript changed: %s", result.State)
	}
	if len(events) == 0 || events[0] != agentcore.StreamAgentStart || events[len(events)-1] != agentcore.StreamAgentEnd {
		t.Fatalf("incomplete streamed lifecycle: %v", events)
	}
	if len(result.Telemetry) == 0 {
		t.Fatal("native telemetry not returned")
	}
	// RunPi owns cleanup, including the durable lease.
	lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil || lease.Err() != nil {
		t.Fatalf("run leaked lease: %v", err)
	}
	_ = release()
}

func TestPiRunSurfacesNativeProviderError(t *testing.T) {
	result, err := RunPi(piSessionContext(t), PiRunConfig{Input: piSessionJSON("fail"), Session: PiSessionConfig{Pi: agentcore.PiConfig{
		Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
			return nil, errors.New("provider rejected request")
		},
	}}})
	if err == nil || !strings.Contains(err.Error(), "provider rejected request") || result.Projection.StopReason != "error" {
		t.Fatalf("native error reported success: %+v %v", result, err)
	}
}

func TestPiRunResumeBillsOnlyNewCalls(t *testing.T) {
	ctx := piSessionContext(t)
	cfg := PiRunConfig{Input: piSessionJSON("first"), Session: PiSessionConfig{Store: agentcore.NewMemorySessionStore(), SessionID: "resume-billing", Pi: agentcore.PiConfig{
		Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
			return piSessionReply(false), nil
		},
	}}}
	first, err := RunPi(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Session.Resume = true
	cfg.Input = piSessionJSON("second")
	second, err := RunPi(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Projection.Usage.InputTokens != 1 || second.Projection.Usage.InputTokens != 1 || !second.Projection.Usage.CostUnpriced || len(second.Projection.Messages) != 4 {
		t.Fatalf("resume billing/history mismatch: %+v", second.Projection)
	}
}

func TestPiRunCancellationRetainsTerminalState(t *testing.T) {
	ctx := piSessionContext(t)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := make(chan struct{})
	type outcome struct {
		result PiRunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := RunPi(runCtx, PiRunConfig{Input: piSessionJSON("cancel"), Session: PiSessionConfig{Store: agentcore.NewMemorySessionStore(), SessionID: "cancel", Pi: agentcore.PiConfig{
			Callback: func(ctx context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}}})
		done <- outcome{result, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.result.Projection.StopReason != "aborted" || !strings.Contains(string(got.result.State), `"isStreaming":false`) {
			t.Fatalf("cancel lost terminal state: %+v %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
