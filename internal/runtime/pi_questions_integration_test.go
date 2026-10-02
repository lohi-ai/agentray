//go:build pi

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/ask"
)

func TestPiAskPreparationMatchesGoPlugin(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"Question":"  Which tier?  ","OPTIONS":[{"LABEL":"  Pro  ","DESCRIPTION":" detail "}],"MULTI":true,"ignored":1}`),
		piSessionJSON(map[string]any{"question": "\u0085" + strings.Repeat("三", 1000) + "\u0085", "options": []any{map[string]string{"label": strings.Repeat("🙂", 100), "description": strings.Repeat("ü", 150)}}}),
		piSessionJSON(map[string]any{"question": "Pick", "options": make([]any, 12)}),
		json.RawMessage(`{"question":null,"options":null,"multi":null}`),
		json.RawMessage(`{"Question":"First","question":null,"Multi":true,"multi":null,"options":[{"Label":"Choice","label":null}]}`),
		json.RawMessage(`null`),
	} {
		t.Run(fmt.Sprintf("%d", len(raw)), func(t *testing.T) {
			ctx := piSessionContext(t)
			a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(), Model: "test", Tools: agentcore.NewToolSet(ask.Tool{}), Policy: agentcore.NewAllowList("ask")})
			if err != nil {
				t.Fatal(err)
			}
			host, err := a.OpenPiTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			var requests int
			var prepared json.RawMessage
			worker := agentcore.PiConfig{Worker: piSessionWorker(t), Options: json.RawMessage(`{"callbacks":["beforeToolCall"]}`), Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				switch method {
				case "beforeToolCall":
					var call struct{ Args json.RawMessage }
					_ = json.Unmarshal(params, &call)
					prepared = call.Args
					return nil, nil
				case "stream":
					requests++
					if requests == 1 {
						var message map[string]any
						_ = json.Unmarshal(piSessionReply(true), &message)
						message["content"] = []any{map[string]any{"type": "toolCall", "id": "ask-1", "name": "ask", "arguments": raw}}
						return piSessionJSON(message), nil
					}
					return piSessionReply(false), nil
				default:
					return nil, fmt.Errorf("unexpected callback %s", method)
				}
			}}
			_, err = RunPi(ctx, PiRunConfig{Host: host, Input: piSessionJSON("ask"), Session: PiSessionConfig{Pi: worker, Policy: agentcore.NewAllowList("ask")}})
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			_ = json.Unmarshal(prepared, &actual)
			_ = json.Unmarshal([]byte((ask.Tool{}).PrepareArguments(string(raw))), &expected)
			if !reflect.DeepEqual(actual, expected) || actual == nil {
				t.Fatalf("native prep=%s Go=%v", prepared, expected)
			}
			var question struct {
				Question string
				Options  []struct{ Label, Description string }
			}
			_ = json.Unmarshal(prepared, &question)
			if !utf8.ValidString(question.Question) || len(question.Question) > 2000 {
				t.Fatal("broken question boundary")
			}
			if (ask.Tool{}).PrepareArguments(string(prepared)) != (ask.Tool{}).PrepareArguments((ask.Tool{}).PrepareArguments(string(prepared))) {
				t.Fatal("preparer is not idempotent")
			}
		})
	}
}

func TestPiRunnerAskResumeKeepsGoalAndNativeHistory(t *testing.T) {
	ctx := piSessionContext(t)
	var requests atomic.Int32
	const condition = "Finish after the human selects a tier"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		var body struct {
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if n == 2 || n == 3 {
			answerCount := 0
			for _, m := range body.Messages {
				if m.Role == "user" && strings.Contains(string(m.Content), "Human answer to question") {
					answerCount++
				}
			}
			if answerCount != 1 || !strings.Contains(string(piSessionJSON(body.Messages)), condition) {
				t.Errorf("resume lost answer or goal: answers=%d", answerCount)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 || n == 4 {
			fmt.Fprint(w, "data: "+`{"id":"ask","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"reused-provider-id","type":"function","function":{"name":"ask","arguments":"{\"Question\":\"  Which tier?  \",\"options\":[{\"label\":\" Pro \"}]}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			answer := "All set."
			if n == 3 {
				answer += "\nSTATUS: DONE"
			}
			fmt.Fprintf(w, "data: {\"id\":\"answer\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s},\"finish_reason\":\"stop\"}]}\n\n", piSessionJSON(answer))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.PrepareNextTurn, p.RefreshKey = nil, nil
	p.Goal = condition
	p.Tools = []agentcore.Tool{ask.Tool{}}
	r := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{Worker: piSessionWorker(t)}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "ask-test", BaseURL: server.URL + "/v1", APIKey: "test"}}
	var questions int
	sink := func(event agentcore.StreamEvent) {
		if event.Type == agentcore.StreamQuestion {
			questions++
		}
	}
	result, err := r.runModelLoop(ctx, p, RunOptions{Prompt: "choose a tier", NativeHistory: json.RawMessage(`[{"role":"user","content":"prior","timestamp":1,"extension":{"signature":"keep"}}]`)}, tier, sink)
	if err != nil || !result.Parked || result.StopReason != "parked" || requests.Load() != 1 || questions != 1 {
		t.Fatalf("did not park: err=%v parked=%v stop=%s calls=%d questions=%d", err, result.Parked, result.StopReason, requests.Load(), questions)
	}
	if string(result.Question) != `{"question":"Which tier?","options":[{"label":"Pro"}]}` {
		t.Fatalf("question was not prepared: %s", result.Question)
	}
	log, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	id, _, found := agentcore.PendingQuestion(log)
	if !found || id == "reused-provider-id" {
		t.Fatalf("question lacks physical workflow ID: %q", id)
	}
	resume := RunOptions{ResumeFromRunID: p.SessionID}
	p.Goal = "" // Recovery must re-arm the stored goal without caller help.
	result, err = r.runModelLoop(ctx, p, resume, tier, sink)
	if err != nil || !result.Parked || requests.Load() != 1 || questions != 2 {
		t.Fatalf("unanswered resume spent model calls: %v calls=%d", err, requests.Load())
	}
	lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		appended, err := agentcore.RecordSessionAnswer(lease, p.Session, p.SessionID, id, "Pro")
		if err != nil || appended != (i == 0) {
			t.Fatalf("answer retry: appended=%v err=%v", appended, err)
		}
	}
	if _, err := agentcore.RecordSessionAnswer(lease, p.Session, p.SessionID, id, "Other"); !errors.Is(err, agentcore.ErrAnswerConflict) {
		t.Fatalf("conflicting answer accepted: %v", err)
	}
	_ = release()
	result, err = r.runModelLoop(ctx, p, resume, tier, sink)
	if err != nil || result.Parked || requests.Load() != 3 || !strings.HasSuffix(result.Final, "STATUS: DONE") {
		t.Fatalf("goal was not restored: err=%v final=%q calls=%d", err, result.Final, requests.Load())
	}
	if !strings.Contains(string(result.NativeState), `"extension":{"signature":"keep"}`) || strings.Count(string(result.NativeState), `"agentrayAnswerId"`) != 1 || !strings.Contains(string(result.NativeState), "Waiting for the user's answer.") || !strings.Contains(string(result.NativeState), `"Question":"  Which tier?  "`) {
		t.Fatal("native answer mutated history or delivered twice")
	}
	resume.Prompt = "Ask one more question"
	result, err = r.runModelLoop(ctx, p, resume, tier, sink)
	if err != nil || !result.Parked || requests.Load() != 4 {
		t.Fatalf("second question: %v calls=%d", err, requests.Load())
	}
	log, err = p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	next, _, found := agentcore.PendingQuestion(log)
	if !found || next == id {
		t.Fatal("reused provider call ID reused the old human answer")
	}
	if _, err := recoverPiState(log); err != nil {
		t.Fatal(err)
	}
}
