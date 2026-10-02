package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPiLifecycleHostCloseCancelsBlockedStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	a := piHostAgent(t, Config{StepGate: func(ctx context.Context, _ int) error { close(started); <-ctx.Done(); return ctx.Err() }})
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	done := make(chan error, 1)
	go func() { _, err := host.PreparePiTurn(ctx, StepInfo{Turn: 1}); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closed := make(chan struct{})
	go func() { _ = host.Close(); close(closed) }()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("host close did not cancel lifecycle hook")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("step returned %v", err)
	}
}

func TestPiLifecycleMetersChildSpendAndEmitsTerminalObserverOnce(t *testing.T) {
	ctx := context.Background()
	var endCalls int
	var observed RunResult
	a := piHostAgent(t, Config{BudgetGate: func(_ context.Context, u Usage) bool { return u.InputTokens >= 10 }, Hooks: Hooks{AgentEnd: []AgentEndHook{func(_ context.Context, result RunResult) { endCalls++; observed = result }}}})
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	a.addChildUsage(Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, CostUSD: 0.25, CostUnpriced: true})
	prepared, err := host.PreparePiTurn(ctx, StepInfo{Turn: 2, Usage: Usage{InputTokens: 1}})
	if err != nil || prepared.StopReason != "budget_exhausted" {
		t.Fatalf("child spend bypassed parent budget: %+v %v", prepared, err)
	}
	result := host.CompletePiRun(ctx, RunResult{Usage: Usage{InputTokens: 1, OutputTokens: 1}})
	if result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 3 || result.Usage.CacheReadTokens != 3 || result.Usage.CostUSD != 0.25 || !result.Usage.CostUnpriced || observed.Usage != result.Usage {
		t.Fatalf("lost child metering: %+v observed=%+v", result, observed)
	}
	_ = host.CompletePiRun(ctx, result)
	if endCalls != 1 || a.peekChildUsage() != (Usage{}) {
		t.Fatal("child usage or terminal observer was repeated")
	}
}

func TestPiContextHooksPreserveNativeMessagesOnFailure(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"panic", "error", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			var reports int
			a := piHostAgent(t, Config{Hooks: Hooks{ErrorPolicy: HookThrow, OnError: func(string, error) { reports++ }, PiContext: []PiContextHook{
				func(_ context.Context, messages []json.RawMessage) ([]json.RawMessage, error) {
					messages[0][0] = '!'
					switch kind {
					case "panic":
						panic("bad extension")
					case "error":
						return nil, errors.New("bad extension")
					default:
						return messages, nil
					}
				},
				func(_ context.Context, messages []json.RawMessage) ([]json.RawMessage, error) {
					return append(messages, json.RawMessage(`{"role":"system","content":"kept valid hook","timestamp":1}`)), nil
				},
			}}})
			host, err := a.OpenPiTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			input := json.RawMessage(`[{"role":"assistant","content":[{"type":"thinking","thinking":"private","thinkingSignature":"native-signature"}],"opaque":{"preserve":true}}]`)
			output := host.TransformPiContext(ctx, input)
			if reports != 1 || !json.Valid(output) || !strings.Contains(string(output), "native-signature") || !strings.Contains(string(output), "kept valid hook") || input[1] != '{' {
				t.Fatalf("native transform lost its last valid view: reports=%d out=%s input=%s", reports, output, input)
			}
		})
	}
}
