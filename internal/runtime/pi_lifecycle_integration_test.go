//go:build pi

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

type piTurnExtension struct{ stops int }

func (*piTurnExtension) Name() string { return "native-turn-policy" }
func (e *piTurnExtension) BeginRun(context.Context, agentcore.RunInfo) (agentcore.Extension, error) {
	return e, nil
}

func TestPiHostPlanContextSurvivesNativeResumeWithoutRewritingHistory(t *testing.T) {
	ctx := piSessionContext(t)
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal = ""
	var requests atomic.Int32
	for attempt := 0; attempt < 2; attempt++ {
		p.Todo = todo.NewStore()
		a, err := Build(p)
		if err != nil {
			t.Fatal(err)
		}
		host, err := a.OpenPiTools(ctx)
		if err != nil {
			t.Fatal(err)
		}
		result, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("continue"), Session: PiSessionConfig{
			Store: p.Session, SessionID: p.SessionID, Resume: attempt == 1, Policy: agentcore.NewAllowList(todo.ToolName), Pi: agentcore.PiConfig{Worker: piSessionWorker(t), Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "stream" {
					return nil, fmt.Errorf("unexpected %s", method)
				}
				n := requests.Add(1)
				if n > 1 {
					if strings.Count(string(params), todo.ContextPrefix) != 1 || !strings.Contains(string(params), "verify native persistence") || !strings.Contains(string(params), "signed-plan-message") {
						t.Errorf("native request lost plan/signature or accumulated reminders: %s", params)
					}
				}
				var reply map[string]any
				_ = json.Unmarshal(piSessionReply(n == 1), &reply)
				if n == 1 {
					reply["content"] = []any{
						map[string]any{"type": "thinking", "thinking": "plan", "thinkingSignature": "signed-plan-message"},
						map[string]any{"type": "toolCall", "id": "plan-1", "name": todo.ToolName, "arguments": map[string]any{"items": []any{map[string]any{"content": "verify native persistence", "status": "in_progress"}}}},
					}
				}
				return piSessionJSON(reply), nil
			}},
		}})
		_ = host.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Todo.List()) != 1 {
			t.Fatalf("attempt %d lost plan store", attempt)
		}
		if strings.Contains(string(result.State), todo.ContextPrefix) {
			t.Fatal("request-only plan reminder leaked into native transcript")
		}
	}
	if requests.Load() != 3 {
		t.Fatalf("unexpected provider calls %d", requests.Load())
	}
}

func TestPiHostSteeringDuringAnswerAndFollowUpReachDurableHistory(t *testing.T) {
	ctx := piSessionContext(t)
	store := agentcore.NewMemorySessionStore()
	var requests atomic.Int32
	var firstSteer, lateSteer, follow bool
	a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test", Session: store, SessionID: "live",
		GetSteeringMessages: func(context.Context) []agentcore.Message {
			if !firstSteer {
				firstSteer = true
				return []agentcore.Message{{Role: agentcore.RoleUser, Content: "initial correction", InputID: "initial-entry"}}
			}
			if requests.Load() == 1 && !lateSteer {
				lateSteer = true
				return []agentcore.Message{{Role: agentcore.RoleUser, Content: "correction during answer", InputID: "late-entry"}}
			}
			return nil
		},
		GetFollowUpMessages: func(context.Context) []agentcore.Message {
			if requests.Load() == 2 && !follow {
				follow = true
				return []agentcore.Message{{Role: agentcore.RoleUser, Content: "one more question", InputID: "follow-entry"}}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	result, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("original question"), Session: PiSessionConfig{Store: store, SessionID: "live", Pi: agentcore.PiConfig{Worker: piSessionWorker(t), Callback: func(_ context.Context, _ string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		n := requests.Add(1)
		var request struct {
			Context struct{ Messages []json.RawMessage }
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, err
		}
		last := request.Context.Messages[len(request.Context.Messages)-1]
		want := map[int32]string{1: "initial correction", 2: "correction during answer", 3: "one more question"}[n]
		if want == "" || !strings.Contains(string(last), want) {
			t.Errorf("request %d used wrong final input: %s", n, last)
		}
		return piSessionReply(false), nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("live input was dropped: calls=%d", requests.Load())
	}
	for _, want := range []string{"original question", "initial correction", "correction during answer", "one more question"} {
		if strings.Count(string(result.State), want) != 1 {
			t.Fatalf("live input not retained exactly once: %s", want)
		}
	}
	entries, err := store.Log(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverPiState(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"initial-entry", "late-entry", "follow-entry"} {
		if strings.Count(string(recovered), `"agentrayInputId":"`+id+`"`) != 1 {
			t.Fatalf("lost durable native input identity %q: %s", id, recovered)
		}
	}
	if !strings.Contains(string(recovered), "one more question") {
		t.Fatal("follow-up missing after recovery")
	}
}
func (*piTurnExtension) SystemPrompt() string { return "native extension instructions" }
func (*piTurnExtension) BeforeStep(_ context.Context, step agentcore.StepInfo) agentcore.StepDecision {
	return agentcore.StepDecision{AdditionalContexts: []agentcore.Message{{Role: agentcore.RoleUser, Content: fmt.Sprintf("step-%d", step.Turn)}}}
}
func (*piTurnExtension) InterceptToolResult(context.Context, agentcore.ToolCall, string, error) agentcore.ToolResultDecision {
	return agentcore.ToolResultDecision{AdditionalContexts: []agentcore.Message{{Role: agentcore.RoleUser, Content: "tool-extra"}}}
}
func (*piTurnExtension) InterceptBatch(context.Context, []agentcore.ToolCall) agentcore.BatchDecision {
	return agentcore.BatchDecision{AdditionalContexts: []agentcore.Message{{Role: agentcore.RoleUser, Content: "batch-extra"}}}
}
func (e *piTurnExtension) TurnStopping(_ context.Context, info agentcore.StopInfo) agentcore.StopDecision {
	e.stops++
	if info.Attempt == 0 {
		return agentcore.StopDecision{Continue: true, Inject: []agentcore.Message{{Role: agentcore.RoleUser, Content: "verify-before-finish"}}}
	}
	return agentcore.StopDecision{}
}

func TestPiHostLifecyclePreservesNativeHistoryAndDurableInjections(t *testing.T) {
	ctx := piSessionContext(t)
	store := agentcore.NewMemorySessionStore()
	var effects, streams atomic.Int32
	var gates, starts, ends []int
	ext := &piTurnExtension{}
	a, err := agentcore.New(agentcore.Config{
		Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test",
		Tools: agentcore.NewToolSet(piComposedTool{&effects}), Policy: agentcore.NewAllowList("write"),
		Session: store, SessionID: "lifecycle", Extensions: []agentcore.ExtensionFactory{ext},
		Definition: agentcore.AgentDefinition{Soul: "native persona"},
		StepGate:   func(_ context.Context, turn int) error { gates = append(gates, turn); return nil },
		Hooks: agentcore.Hooks{
			TurnStart: []agentcore.TurnHook{func(_ context.Context, info agentcore.TurnInfo) { starts = append(starts, info.Turn) }},
			TurnEnd:   []agentcore.TurnHook{func(_ context.Context, info agentcore.TurnInfo) { ends = append(ends, info.Turn) }},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	result, err := RunPi(ctx, PiRunConfig{Host: host, Task: "execute then verify", Input: piSessionJSON("execute"), PricingKnown: true,
		Session: PiSessionConfig{Store: store, SessionID: "lifecycle", Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{
			Worker: piSessionWorker(t), Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "stream" {
					return nil, fmt.Errorf("unexpected callback %s", method)
				}
				n := streams.Add(1)
				for _, want := range []string{"native persona", "native extension instructions", fmt.Sprintf("step-%d", n)} {
					if !strings.Contains(string(params), want) {
						t.Errorf("request %d missing %s", n, want)
					}
				}
				if n >= 2 {
					for _, want := range []string{"tool-extra", "batch-extra", "native-signature"} {
						if !strings.Contains(string(params), want) {
							t.Errorf("request %d missing %s", n, want)
						}
					}
				}
				if n == 3 && !strings.Contains(string(params), "verify-before-finish") {
					t.Error("stop guard did not reach native request")
				}
				var reply map[string]any
				_ = json.Unmarshal(piSessionReply(n == 1), &reply)
				if n == 1 {
					reply["content"] = append([]any{map[string]any{"type": "thinking", "thinking": "private", "thinkingSignature": "native-signature"}}, reply["content"].([]any)...)
					reply["opaque"] = map[string]any{"keep": true}
				}
				return piSessionJSON(reply), nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if streams.Load() != 3 || effects.Load() != 1 || ext.stops != 2 || result.Projection.Turns != 3 {
		t.Fatalf("wrong lifecycle: streams=%d effects=%d stops=%d result=%+v", streams.Load(), effects.Load(), ext.stops, result.Projection)
	}
	for _, got := range [][]int{gates, starts, ends} {
		if !reflect.DeepEqual(got, []int{1, 2, 3}) {
			t.Fatalf("missing turn boundary: %v", got)
		}
	}
	entries, err := store.Log(ctx, "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverPiState(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tool-extra", "batch-extra", "verify-before-finish", "step-3", "native-signature", `"opaque":{"keep":true}`} {
		if !strings.Contains(string(recovered), want) {
			t.Fatalf("durable native state lost %s", want)
		}
	}
}

type piEndingTool struct {
	effects *atomic.Int32
	parked  bool
}

func (piEndingTool) Name() string { return "write" }
func (piEndingTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: "write", Parameters: map[string]any{"type": "object"}}
}
func (t piEndingTool) Run(context.Context, string) (string, error) {
	t.effects.Add(1)
	if t.parked {
		return "", agentcore.ErrParked
	}
	return "written", nil
}

func TestPiHostLifecycleCeilingsAndTerminalTools(t *testing.T) {
	for _, kind := range []string{"max_turns", "max_tool_calls", "budget_exhausted", "initial_budget", "parked", "terminal"} {
		t.Run(kind, func(t *testing.T) {
			ctx := piSessionContext(t)
			var effects, streams atomic.Int32
			limits := agentcore.DefaultLimits()
			if kind == "max_turns" {
				limits.MaxTurns = 1
			}
			if kind == "max_tool_calls" {
				limits.MaxToolCalls = 1
			}
			cfg := agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test", Limits: &limits, Policy: agentcore.NewAllowList("write"), Tools: agentcore.NewToolSet(piEndingTool{&effects, kind == "parked"})}
			if kind == "budget_exhausted" || kind == "initial_budget" {
				cfg.BudgetGate = func(_ context.Context, u agentcore.Usage) bool { return kind == "initial_budget" || u.InputTokens > 0 }
			}
			if kind == "terminal" {
				cfg.Hooks.After = []agentcore.AfterToolCall{func(_ context.Context, _ agentcore.ToolCall, result string, _ error) (string, bool) {
					return result, true
				}}
			}
			a, err := agentcore.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			host, err := a.OpenPiTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			result, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("execute"), Session: PiSessionConfig{Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{Worker: piSessionWorker(t), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "stream" {
					return nil, fmt.Errorf("unexpected callback %s", method)
				}
				streams.Add(1)
				return piSessionReply(true), nil
			}}}})
			if err != nil {
				t.Fatal(err)
			}
			wantStreams, wantEffects := int32(2), int32(1)
			if kind == "parked" || kind == "terminal" {
				wantStreams = 1
			}
			if kind == "initial_budget" {
				wantStreams, wantEffects = 1, 0
			}
			if streams.Load() != wantStreams || effects.Load() != wantEffects {
				t.Fatalf("ceiling allowed extra work: streams=%d effects=%d", streams.Load(), effects.Load())
			}
			if (kind == "parked") != result.Projection.Parked {
				t.Fatalf("wrong parked result: %+v", result.Projection)
			}
			if kind == "max_turns" || kind == "max_tool_calls" || kind == "budget_exhausted" {
				if result.Projection.StopReason != kind {
					t.Fatalf("lost ceiling reason: %+v", result.Projection)
				}
			}
		})
	}
}

func TestPiHostLifecycleCancellationDuringStepGate(t *testing.T) {
	ctx := piSessionContext(t)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := make(chan struct{})
	var effects, streams atomic.Int32
	a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test", Policy: agentcore.NewAllowList("write"), Tools: agentcore.NewToolSet(piEndingTool{effects: &effects}), StepGate: func(ctx context.Context, turn int) error {
		if turn == 2 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	done := make(chan error, 1)
	go func() {
		_, err := RunPi(runCtx, PiRunConfig{Host: host, Input: piSessionJSON("execute"), Session: PiSessionConfig{Policy: agentcore.NewAllowList("write"), Pi: agentcore.PiConfig{Worker: piSessionWorker(t), Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
			streams.Add(1)
			return piSessionReply(true), nil
		}}}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if streams.Load() != 1 || effects.Load() != 1 {
		t.Fatal("canceled step performed more work")
	}
}

func TestPiHostLifecycleRunsServerGoalPlugin(t *testing.T) {
	ctx := piSessionContext(t)
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	a, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	var calls atomic.Int32
	result, err := RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("finish the task"), Session: PiSessionConfig{
		Store: p.Session, SessionID: p.SessionID, Pi: agentcore.PiConfig{Worker: piSessionWorker(t), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method != "stream" {
				return nil, fmt.Errorf("unexpected %s", method)
			}
			var reply map[string]any
			_ = json.Unmarshal(piSessionReply(false), &reply)
			if calls.Add(1) > 1 {
				reply["content"] = []any{map[string]any{"type": "text", "text": "STATUS: DONE"}}
			}
			return piSessionJSON(reply), nil
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.Projection.Final != "STATUS: DONE" {
		t.Fatalf("server goal gate was bypassed: calls=%d result=%+v", calls.Load(), result.Projection)
	}
}
