package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
	"github.com/lohi-ai/agentray/agentcore/plugins/ask"
	"github.com/lohi-ai/agentray/telemetry/llm"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/telemetry"
)

type nativeChildWrite struct{ effects *atomic.Int32 }

func (nativeChildWrite) Name() string { return "write" }
func (nativeChildWrite) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: "write", Description: "Write", Parameters: map[string]any{"type": "object"}}
}
func (t nativeChildWrite) Run(context.Context, string) (string, error) {
	t.effects.Add(1)
	return "written", nil
}

func nativeChildResponse(content ...ai.ContentBlock) *ai.AssistantMessageEventStream {
	var message ai.Message
	_ = json.Unmarshal([]byte(nativeStreamReply), &message)
	message.Content = ai.BlockContent(content...)
	message.Usage.Input = 1
	message.Usage.Output = 1
	message.Usage.TotalTokens = 2
	for _, block := range content {
		if block.Type == "toolCall" {
			message.StopReason = "toolUse"
		}
	}
	stream := ai.NewAssistantMessageEventStream()
	stream.Push(ai.AssistantMessageEvent{Type: "start", Partial: &message})
	stream.Push(ai.AssistantMessageEvent{Type: "done", Reason: message.StopReason, Message: &message})
	stream.End()
	return stream
}
func nativeChildCall(id, name, args string) ai.ContentBlock {
	return ai.ContentBlock{Type: "toolCall", ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func TestNativeRunnerChildrenUseGoIsolationReceiptsAndTrace(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var parents, children, effects atomic.Int32
			var firstChildren atomic.Int32
			bothStarted := make(chan struct{})
			var mu sync.Mutex
			perChild := map[string]int{}
			traces := []llm.TraceRecord{}
			p := representativeBuildParams()
			p.Sandbox, p.HTTPTool = nil, nil
			p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
			p.Tools = []agentcore.Tool{nativeChildWrite{&effects}}
			p.Tracer = llm.SinkFunc(func(record llm.TraceRecord) { mu.Lock(); defer mu.Unlock(); traces = append(traces, record) })
			provider := func(ctx context.Context, _ json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
				raw := string(passiveNativeJSON(transcript))
				spawn := false
				for _, tool := range ai.GetCurrentTools(transcript.Messages()) {
					if tool.Name == subagent.ToolSpawnSubagent {
						spawn = true
					}
				}
				if strings.Contains(raw, "PARENT-ONLY") {
					if !spawn {
						t.Error("parent lost delegation tool")
					}
					if parents.Add(1) == 1 {
						calls := []ai.ContentBlock{nativeChildCall("child-one", subagent.ToolSpawnSubagent, `{"task":"isolated-child-task"}`)}
						if parallel {
							calls = append(calls, nativeChildCall("child-two", subagent.ToolSpawnSubagent, `{"task":"isolated-child-task"}`))
						}
						return nativeChildResponse(calls...), nil
					}
					return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "parent answer"}), nil
				}
				if !strings.Contains(raw, "isolated-child-task") || strings.Contains(raw, "parent private history") {
					t.Errorf("child context leaked: %s", raw)
				}
				if spawn {
					t.Error("child advertised delegation past depth limit")
				}
				if agentcore.DelegationDepth(ctx) != 1 {
					t.Error("child lost delegation depth")
				}
				identity := agentcore.RunSessionFrom(ctx)
				if options["sessionId"] != identity {
					t.Errorf("wrong provider session: %v/%s", options["sessionId"], identity)
				}
				children.Add(1)
				mu.Lock()
				perChild[identity]++
				n := perChild[identity]
				mu.Unlock()
				if n == 1 {
					if parallel {
						if firstChildren.Add(1) == 2 {
							close(bothStarted)
						}
						select {
						case <-bothStarted:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return nativeChildResponse(nativeChildCall("same-write", "write", `{}`)), nil
				}
				return nativeChildResponse(nativeChildThinking("child-opaque-signature"), ai.ContentBlock{Type: "text", Text: "child answer"}), nil
			}
			runtime := PiRuntimeConfig{NativeStream: provider}
			runner := NewRunner(nil, WithPiRuntime(runtime))
			tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "test"}}
			result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY", NativeHistory: json.RawMessage(`[{"role":"user","content":"parent private history","timestamp":1}]`)}, tier, nil)
			if err != nil {
				t.Fatal(err)
			}
			count := int32(1)
			if parallel {
				count = 2
			}
			if result.Final != "parent answer" || parents.Load() != 2 || children.Load() != count*2 || effects.Load() != count {
				t.Fatalf("Go delegation failed: final=%q requests=%d/%d effects=%d", result.Final, parents.Load(), children.Load(), effects.Load())
			}
			if result.Usage.InputTokens != int(2+count*2) || result.Usage.OutputTokens != int(2+count*2) {
				t.Fatalf("child usage lost/duplicated: %+v", result.Usage)
			}
			store := p.Session.(*agentcore.MemorySessionStore)
			if len(store.Sessions()) != int(count+1) {
				t.Fatalf("wrong child sessions: %v", store.Sessions())
			}
			parent, err := Build(p)
			if err != nil {
				t.Fatal(err)
			}
			reattach := piForkRunner(PiSessionConfig{Pi: NativeAgentConfig{}}, true, store, p.ToolChoice, nil)
			for _, id := range store.Sessions() {
				if id == p.SessionID {
					continue
				}
				log, err := store.Log(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				completed, done, err := piCompletedChild(log)
				if err != nil || !done || completed.Final != "child answer" || !strings.Contains(string(completed.NativeState), "child-opaque-signature") {
					t.Fatalf("child receipt/state: %+v %v", completed, err)
				}
				req := subagent.ForkRequest{SessionID: id, Prompt: "isolated-child-task", Task: "isolated-child-task"}
				attached, err := reattach(ctx, parent.Fork(id), req, nil)
				if err != nil || attached.StopReason != "reattached" || attached.Usage != (agentcore.Usage{}) {
					t.Fatalf("reattach failed: %+v %v", attached, err)
				}
				req.Task = "different task"
				if _, err := reattach(ctx, parent.Fork(id), req, nil); err == nil {
					t.Fatal("child receipt reused for different request")
				}
			}
			if children.Load() != count*2 || effects.Load() != count {
				t.Fatal("reattach repeated work")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(traces) != int(2+count*2) {
				t.Fatalf("trace count: %d", len(traces))
			}
			for _, trace := range traces {
				if trace.SessionKey == p.SessionID {
					if trace.Depth != 0 {
						t.Error("parent trace depth changed")
					}
				} else if trace.Depth != 1 || perChild[trace.SessionKey] != 2 {
					t.Errorf("child trace attribution: %+v", trace)
				}
			}
		})
	}
}

func TestNativeChildParkReattachAndAnswerResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := agentcore.NewMemorySessionStore()
	parent, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(), Model: "test", Session: store, SessionID: "parent", Tools: agentcore.NewToolSet(ask.Tool{}), Policy: agentcore.NewAllowList("ask")})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	runtime := PiSessionConfig{Pi: NativeAgentConfig{Options: json.RawMessage(`{"initialState":{}}`), Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "stream" {
			return nil, fmt.Errorf("unexpected callback: %s", method)
		}
		n := requests.Add(1)
		if n == 1 {
			stream := nativeChildResponse(nativeChildThinking("child-ask-signature"), nativeChildCall("ask-1", "ask", `{"question":"Which scope?"}`))
			message, err := stream.Result(ctx)
			if err != nil {
				return nil, err
			}
			return json.Marshal(message)
		}
		if !strings.Contains(string(params), "child-ask-signature") || strings.Count(string(params), `"agentrayAnswerId"`) != 1 || !strings.Contains(string(params), "approved scope") {
			return nil, errors.New("native answer/signature lost or duplicated")
		}
		return json.RawMessage(nativeStreamReply), nil
	}}}
	fork := piForkRunner(runtime, true, store, agentcore.ToolChoice{}, nil)
	req := subagent.ForkRequest{SessionID: "parent/child", Prompt: "child task", Task: "child task"}
	parked, err := fork(ctx, parent.Fork(req.SessionID), req, nil)
	var question *PiChildQuestionError
	if !errors.As(err, &question) || !parked.Parked || question.QuestionID == "" {
		t.Fatalf("child did not park: %+v %v", parked, err)
	}
	attached, err := fork(ctx, parent.Fork(req.SessionID), req, nil)
	if !errors.As(err, &question) || !attached.Parked || attached.Usage != (agentcore.Usage{}) || requests.Load() != 1 {
		t.Fatalf("parked reattach repeated work: %+v %v", attached, err)
	}
	lease, release, err := agentcore.AcquireSessionLease(ctx, store, req.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agentcore.RecordSessionAnswer(lease, store, req.SessionID, question.QuestionID, "approved scope")
	_ = release()
	if err != nil {
		t.Fatal(err)
	}
	completed, err := fork(ctx, parent.Fork(req.SessionID), req, nil)
	if err != nil || completed.Final != "done" || requests.Load() != 2 {
		t.Fatalf("answered child failed: %+v %v", completed, err)
	}
}

func TestNativeParentReceiptRetainsChildQuestionRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	var effects atomic.Int32
	p.Tools = []agentcore.Tool{ask.Tool{}, nativeChildWrite{&effects}}
	var parentCalls, childCalls int
	provider := func(_ context.Context, _ json.RawMessage, transcript ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
		if strings.Contains(string(passiveNativeJSON(transcript)), "PARENT-ONLY") {
			parentCalls++
			if parentCalls == 1 {
				return nativeChildResponse(nativeChildCall("spawn", subagent.ToolSpawnSubagent, `{"task":"child task"}`)), nil
			}
			raw := string(passiveNativeJSON(transcript))
			if !strings.Contains(raw, "child completed work") || strings.Count(raw, `"agentrayDelegationId"`) != 1 {
				t.Errorf("parent lost or duplicated the resumed child result: %s", raw)
			}
			return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "parent completed work"}), nil
		}
		childCalls++
		if childCalls == 1 {
			return nativeChildResponse(nativeChildCall("ask", "ask", `{"question":"Which scope?"}`)), nil
		}
		raw := string(passiveNativeJSON(transcript))
		if !strings.Contains(raw, "approved scope") || strings.Count(raw, `"agentrayAnswerId"`) != 1 {
			t.Errorf("child lost or duplicated the forwarded answer: %s", raw)
		}
		if childCalls == 2 {
			return nativeChildResponse(nativeChildCall("write", "write", `{}`)), nil
		}
		return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "child completed work"}), nil
	}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{NativeStream: provider}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Parked || result.StopReason != "parked" || parentCalls != 1 || !strings.Contains(string(result.Question), "Which scope?") {
		t.Fatalf("parent continued past the child question: %+v calls=%d", result, parentCalls)
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var route *agentcore.ChildQuestionError
	var originalTrace agentcore.ToolTrace
	for _, entry := range entries {
		if entry.Kind != agentcore.EntryPiEffectDone || entry.CallID != "spawn" {
			continue
		}
		var receipt struct {
			Result struct{ Details agentcore.PiToolOutcome }
		}
		if err := json.Unmarshal([]byte(entry.Content), &receipt); err != nil {
			t.Fatal(err)
		}
		route = receipt.Result.Details.ChildQuestion
		originalTrace = receipt.Result.Details.Trace
	}
	if route == nil || route.SessionID == p.SessionID || route.QuestionID == "" || childCalls != 1 {
		t.Fatalf("parent journal lost child question identity: %+v child calls=%d", route, childCalls)
	}
	childEntries, err := p.Session.Log(ctx, route.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	id, question, pending := agentcore.PendingQuestion(childEntries)
	if !pending || id != route.QuestionID || !nativehost.SameJSON(question, route.Question) {
		t.Fatalf("parent receipt does not address the durable child question: %+v id=%s question=%s", route, id, question)
	}
	parentID, parentQuestion, pending := agentcore.PendingQuestion(entries)
	if !pending || parentID == id || !nativehost.SameJSON(parentQuestion, question) {
		t.Fatalf("parent waiting on wrong question: id=%s question=%s", parentID, parentQuestion)
	}
	p.ResumeSession = true
	reattached, err := runner.runModelLoop(ctx, p, RunOptions{}, tier, nil)
	if err != nil || !reattached.Parked || parentCalls != 1 || childCalls != 1 {
		t.Fatalf("parked parent reattach repeated work: %+v %v calls=%d/%d", reattached, err, parentCalls, childCalls)
	}
	lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	appended, err := agentcore.RecordSessionAnswer(lease, p.Session, p.SessionID, parentID, "approved scope")
	_ = release()
	if err != nil || !appended {
		t.Fatalf("parent answer was not forwarded: appended=%v err=%v", appended, err)
	}
	for sessionID, questionID := range map[string]string{p.SessionID: parentID, route.SessionID: route.QuestionID} {
		log, err := p.Session.Log(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		answers := 0
		for _, entry := range log {
			if entry.Kind == agentcore.EntryPiAnswer {
				answers++
				if entry.CallID != questionID || entry.Answer != "approved scope" {
					t.Fatalf("answer addressed wrong workflow: %+v", entry)
				}
			}
		}
		if answers != 1 {
			t.Fatalf("session %s has %d answers", sessionID, answers)
		}
	}
	var eventMu sync.Mutex
	var events []agentcore.StreamEvent
	completed, err := runner.runModelLoop(ctx, p, RunOptions{}, tier, func(event agentcore.StreamEvent) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if event.Tool != nil {
			trace := *event.Tool
			event.Tool = &trace
		}
		events = append(events, event)
	})
	if err != nil || completed.Parked || completed.Final != "parent completed work" || parentCalls != 2 || childCalls != 3 || effects.Load() != 1 {
		t.Fatalf("parent did not resume the original delegation: %+v %v calls=%d/%d", completed, err, parentCalls, childCalls)
	}
	if len(p.Session.(*agentcore.MemorySessionStore).Sessions()) != 2 {
		t.Fatal("resume created another child session")
	}
	if len(completed.Tools) != 1 || completed.Tools[0].CallID != originalTrace.CallID || completed.Tools[0].IdempotencyKey != originalTrace.IdempotencyKey || completed.Tools[0].Error != "" || !completed.Tools[0].Allowed {
		t.Fatalf("resume lost its governed tool projection: %+v", completed.Tools)
	}
	start, update, end := -1, -1, -1
	for i, event := range events {
		if event.Tool == nil || event.Tool.Tool != subagent.ToolSpawnSubagent {
			continue
		}
		switch event.Type {
		case agentcore.StreamToolExecStart:
			if start != -1 {
				t.Fatal("duplicate continuation start")
			}
			start = i
		case agentcore.StreamToolExecUpdate:
			if strings.Contains(event.Note, "running write") {
				update = i
			}
		case agentcore.StreamToolExecEnd:
			if end != -1 {
				t.Fatal("duplicate continuation end")
			}
			end = i
		}
	}
	if start < 0 || update <= start || end <= update {
		t.Fatalf("missing/out-of-order continuation progress: %+v", events)
	}
	entries, err = p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	nativeStarts := 0
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiEvent && strings.Contains(entry.Content, `"type":"tool_execution_start"`) {
			nativeStarts++
		}
	}
	if nativeStarts != 1 {
		t.Fatalf("host continuation fabricated native tool events: %d", nativeStarts)
	}
}

type nativeDelegationFaultStore struct {
	*agentcore.MemorySessionStore
	mu                 sync.Mutex
	parent, mode       string
	afterDelivery, hit bool
}

func (s *nativeDelegationFaultStore) Append(ctx context.Context, id string, entry agentcore.SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.parent && !s.hit {
		fail := s.mode == "before receipt" && entry.Kind == agentcore.EntryPiDelegation
		fail = fail || (s.mode == "before batch receipt" && entry.Kind == agentcore.EntryPiDelegationBatch)
		fail = fail || (s.mode == "terminal checkpoint" && entry.Kind == agentcore.EntryPiState && (strings.Contains(entry.Content, `"agentrayDelegationId"`) || strings.Contains(entry.Content, `"agentrayAnswerId"`)))
		var event struct {
			Type    string
			Message struct{ AgentrayDelegationID, AgentrayAnswerID string }
		}
		if entry.Kind == agentcore.EntryPiEvent {
			_ = json.Unmarshal([]byte(entry.Content), &event)
			fail = fail || (s.mode == "before delivery" && event.Type == "message_start" && (event.Message.AgentrayDelegationID != "" || event.Message.AgentrayAnswerID != "")) || (s.mode == "after delivery" && s.afterDelivery)
			if event.Type == "message_end" && (event.Message.AgentrayDelegationID != "" || event.Message.AgentrayAnswerID != "") {
				s.afterDelivery = true
			}
		}
		if fail {
			s.hit = true
			return errors.New("simulated delegation persistence failure")
		}
	}
	return s.MemorySessionStore.Append(ctx, id, entry)
}

func TestNativeDelegationResumeReparksAndRecoversDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, fault string
		questions   int
	}{
		{name: "two questions", questions: 2},
		{name: "crash before repark", fault: "before receipt", questions: 2},
		{name: "crash before receipt", fault: "before receipt", questions: 1},
		{name: "crash before delivery", fault: "before delivery", questions: 1},
		{name: "crash after delivery", fault: "after delivery", questions: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			p := representativeBuildParams()
			p.Sandbox, p.HTTPTool = nil, nil
			p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
			var effects atomic.Int32
			p.Tools = []agentcore.Tool{nativeChildWrite{&effects}, ask.Tool{}}
			store := &nativeDelegationFaultStore{MemorySessionStore: agentcore.NewMemorySessionStore(), parent: p.SessionID, mode: tc.fault}
			p.Session = store
			var parentCalls, childCalls int
			provider := func(_ context.Context, _ json.RawMessage, transcript ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
				raw := string(passiveNativeJSON(transcript))
				if strings.Contains(raw, "PARENT-ONLY") {
					parentCalls++
					if parentCalls == 1 {
						return nativeChildResponse(nativeChildCall("spawn", subagent.ToolSpawnSubagent, `{"task":"child task"}`)), nil
					}
					if !strings.Contains(raw, "child completed") || strings.Count(raw, `"agentrayDelegationId"`) != 1 || strings.Contains(raw, `"agentrayAnswerId"`) {
						t.Errorf("parent received an incomplete/duplicate child result: %s", raw)
					}
					return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "parent completed"}), nil
				}
				childCalls++
				if childCalls <= tc.questions {
					calls := []ai.ContentBlock{nativeChildThinking("child-opaque")}
					if childCalls == 1 {
						calls = append(calls, nativeChildCall("write", "write", `{}`))
					}
					calls = append(calls, nativeChildCall("same-ask-id", "ask", fmt.Sprintf(`{"question":"Question %d?"}`, childCalls)))
					return nativeChildResponse(calls...), nil
				}
				if strings.Count(raw, `"agentrayAnswerId"`) != tc.questions || !strings.Contains(raw, "child-opaque") || strings.Contains(raw, "PARENT-ONLY") {
					t.Errorf("resumed child lost native answers/signatures or isolation: %s", raw)
				}
				return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "child completed"}), nil
			}
			runtime := PiRuntimeConfig{NativeStream: provider}
			runner := NewRunner(nil, WithPiRuntime(runtime))
			tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "test"}}
			result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
			if err != nil || !result.Parked || parentCalls != 1 {
				t.Fatalf("initial park failed: %+v %v", result, err)
			}
			p.ResumeSession = true
			previousQuestion := ""
			for n := 1; n <= tc.questions; n++ {
				entries, err := store.Log(ctx, p.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				id, question, pending := agentcore.PendingQuestion(entries)
				if !pending || id == previousQuestion || !strings.Contains(string(question), fmt.Sprintf("Question %d?", n)) {
					t.Fatalf("wrong continued question: %s %s", id, question)
				}
				previousQuestion = id
				reattached, err := runner.runModelLoop(ctx, p, RunOptions{}, tier, nil)
				if err != nil || !reattached.Parked || childCalls != n || parentCalls != 1 {
					t.Fatalf("waiting reattach repeated work: %+v %v calls=%d/%d", reattached, err, parentCalls, childCalls)
				}
				lease, release, err := agentcore.AcquireSessionLease(ctx, store, p.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = agentcore.RecordSessionAnswer(lease, store, p.SessionID, id, fmt.Sprintf("answer %d", n))
				_ = release()
				if err != nil {
					t.Fatal(err)
				}
				var endMu sync.Mutex
				var endTrace *agentcore.ToolTrace
				result, err = runner.runModelLoop(ctx, p, RunOptions{}, tier, func(event agentcore.StreamEvent) {
					if event.Type == agentcore.StreamToolExecEnd && event.Tool != nil && event.Tool.Tool == subagent.ToolSpawnSubagent {
						endMu.Lock()
						defer endMu.Unlock()
						copy := *event.Tool
						endTrace = &copy
					}
				})
				if tc.fault != "" && err != nil {
					if !store.hit || parentCalls != 1 || childCalls != n+1 {
						t.Fatalf("fault did not stop at the intended boundary: %v hit=%v calls=%d/%d", err, store.hit, parentCalls, childCalls)
					}
					if tc.fault == "before receipt" {
						endMu.Lock()
						ended := endTrace != nil && strings.Contains(endTrace.Error, "simulated delegation persistence failure")
						endMu.Unlock()
						if !ended || len(result.Tools) != 1 || !strings.Contains(result.Tools[0].Error, "simulated delegation persistence failure") {
							t.Fatalf("failed continuation was reported as settled: end=%+v tools=%+v", endTrace, result.Tools)
						}
					}
					// Recreate the runner: only the durable journals survive.
					runner = NewRunner(nil, WithPiRuntime(runtime))
					result, err = runner.runModelLoop(ctx, p, RunOptions{}, tier, nil)
					if tc.fault != "before receipt" && len(result.Tools) != 0 {
						t.Fatalf("delivery-only retry fabricated fresh tool work: %+v", result.Tools)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if n < tc.questions && (!result.Parked || parentCalls != 1) {
					t.Fatalf("second question did not park parent: %+v calls=%d", result, parentCalls)
				}
			}
			if result.Parked || result.Final != "parent completed" || parentCalls != 2 || childCalls != tc.questions+1 || effects.Load() != 1 || len(store.Sessions()) != 2 || (tc.fault != "" && !store.hit) {
				t.Fatalf("resume duplicated/lost work: %+v calls=%d/%d effects=%d sessions=%v", result, parentCalls, childCalls, effects.Load(), store.Sessions())
			}
		})
	}
}

func TestNativeNestedDelegationQuestionsResumeOriginalTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{ask.Tool{}}
	p.Subagents = &subagent.Plugin{MaxDepth: 2}
	calls := [3]int{}
	provider := func(ctx context.Context, _ json.RawMessage, transcript ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
		depth := agentcore.DelegationDepth(ctx)
		if depth < 0 || depth > 2 {
			return nil, fmt.Errorf("unexpected delegation depth %d", depth)
		}
		calls[depth]++
		if depth < 2 {
			if calls[depth] == 1 {
				return nativeChildResponse(nativeChildCall("same-spawn", subagent.ToolSpawnSubagent, `{"task":"nested task"}`)), nil
			}
			raw := string(passiveNativeJSON(transcript))
			if !strings.Contains(raw, fmt.Sprintf("depth %d complete", depth+1)) || strings.Count(raw, `"agentrayDelegationId"`) != 1 {
				t.Errorf("depth %d lost child completion: %s", depth, raw)
			}
		} else if calls[depth] <= 2 {
			return nativeChildResponse(nativeChildCall("same-ask", "ask", fmt.Sprintf(`{"question":"Nested question %d?"}`, calls[depth]))), nil
		}
		return nativeChildResponse(ai.ContentBlock{Type: "text", Text: fmt.Sprintf("depth %d complete", depth)}), nil
	}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{NativeStream: provider}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "parent task"}, tier, nil)
	if err != nil || !result.Parked {
		t.Fatalf("nested tree did not park: %+v %v", result, err)
	}
	p.ResumeSession = true
	for n := 1; n <= 2; n++ {
		entries, err := p.Session.Log(ctx, p.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		id, question, pending := agentcore.PendingQuestion(entries)
		if !pending || !strings.Contains(string(question), fmt.Sprintf("Nested question %d?", n)) {
			t.Fatalf("wrong nested question: %s", question)
		}
		lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = agentcore.RecordSessionAnswer(lease, p.Session, p.SessionID, id, fmt.Sprintf("answer %d", n))
		_ = release()
		if err != nil {
			t.Fatal(err)
		}
		result, err = runner.runModelLoop(ctx, p, RunOptions{}, tier, nil)
		if err != nil || (n == 1 && (!result.Parked || calls != [3]int{1, 1, 2})) {
			t.Fatalf("nested resume failed: %+v %v calls=%v", result, err, calls)
		}
	}
	if result.Parked || result.Final != "depth 0 complete" || calls != [3]int{2, 2, 3} || len(p.Session.(*agentcore.MemorySessionStore).Sessions()) != 3 {
		t.Fatalf("nested tree was lost/duplicated: %+v calls=%v", result, calls)
	}
}

func TestNativeSchemaRetryQuestionResumesCorrectiveChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{ask.Tool{}}
	var parents, originals, retries int
	provider := func(ctx context.Context, _ json.RawMessage, transcript ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
		if agentcore.DelegationDepth(ctx) == 0 {
			parents++
			if parents == 1 {
				return nativeChildResponse(nativeChildCall("spawn", subagent.ToolSpawnSubagent, `{"task":"structured task","output_schema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}}`)), nil
			}
			if !strings.Contains(string(passiveNativeJSON(transcript)), "corrected value") {
				t.Error("parent did not receive validated corrective answer")
			}
			return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "done"}), nil
		}
		if !strings.HasSuffix(agentcore.RunSessionFrom(ctx), "/retry") {
			originals++
			return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "invalid JSON answer"}), nil
		}
		retries++
		if retries == 1 {
			return nativeChildResponse(nativeChildCall("ask", "ask", `{"question":"Which value?"}`)), nil
		}
		return nativeChildResponse(ai.ContentBlock{Type: "text", Text: `{"value":"corrected value"}`}), nil
	}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{NativeStream: provider}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "parent task"}, tier, nil)
	if err != nil || !result.Parked || parents != 1 || originals != 1 || retries != 1 {
		t.Fatalf("corrective child's question was lost: %+v %v calls=%d/%d/%d", result, err, parents, originals, retries)
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	id, _, pending := agentcore.PendingQuestion(entries)
	if !pending {
		t.Fatal("missing corrective question")
	}
	lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agentcore.RecordSessionAnswer(lease, p.Session, p.SessionID, id, "corrected value")
	_ = release()
	if err != nil {
		t.Fatal(err)
	}
	p.ResumeSession = true
	result, err = runner.runModelLoop(ctx, p, RunOptions{}, tier, nil)
	if err != nil || result.Parked || result.Final != "done" || parents != 2 || originals != 1 || retries != 2 || len(p.Session.(*agentcore.MemorySessionStore).Sessions()) != 3 {
		t.Fatalf("corrective resume duplicated/lost a child: %+v %v calls=%d/%d/%d", result, err, parents, originals, retries)
	}
}

func TestNativeParallelChildQuestionsCompleteBeforeParentContinues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{ask.Tool{}}
	var mu sync.Mutex
	parents := 0
	children := map[string]int{}
	provider := func(ctx context.Context, _ json.RawMessage, transcript ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
		mu.Lock()
		defer mu.Unlock()
		if agentcore.DelegationDepth(ctx) == 0 {
			parents++
			if parents == 1 {
				return nativeChildResponse(nativeChildCall("spawn-one", subagent.ToolSpawnSubagent, `{"task":"first task"}`), nativeChildCall("spawn-two", subagent.ToolSpawnSubagent, `{"task":"second task"}`)), nil
			}
			if strings.Count(string(passiveNativeJSON(transcript)), `"agentrayDelegationId"`) != 2 {
				t.Error("parent did not receive exactly two completed delegations")
			}
			return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "parent done"}), nil
		}
		id := agentcore.RunSessionFrom(ctx)
		children[id]++
		if children[id] == 1 {
			args := piModelJSON(map[string]any{"question": "Which scope for " + id + "?"})
			return nativeChildResponse(nativeChildCall("same-ask", "ask", string(args))), nil
		}
		return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "child done"}), nil
	}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{NativeStream: provider}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "test"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "parent task"}, tier, nil)
	if err != nil || !result.Parked || len(children) != 2 || parents != 1 {
		t.Fatalf("parallel children did not park: %+v %v calls=%v", result, err, children)
	}
	p.ResumeSession = true
	for n := 0; n < 2; n++ {
		entries, err := p.Session.Log(ctx, p.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		id, question, pending := agentcore.PendingQuestion(entries)
		if !pending || !nativehost.SameJSON(question, result.Question) {
			t.Fatalf("displayed question differs from the pending workflow: %s %s", question, result.Question)
		}
		lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = agentcore.RecordSessionAnswer(lease, p.Session, p.SessionID, id, fmt.Sprintf("answer %d", n))
		_ = release()
		if err != nil {
			t.Fatal(err)
		}
		result, err = runner.runModelLoop(ctx, p, RunOptions{}, tier, nil)
		if err != nil || (n == 0 && (!result.Parked || parents != 1)) {
			t.Fatalf("parent continued before all children finished: %+v %v", result, err)
		}
	}
	if result.Parked || result.Final != "parent done" || parents != 2 || len(p.Session.(*agentcore.MemorySessionStore).Sessions()) != 3 {
		t.Fatalf("parallel delegation did not finish: %+v", result)
	}
	for id, count := range children {
		if count != 2 {
			t.Fatalf("child %s ran %d requests", id, count)
		}
	}
}

type nativeDelegationContextExtension struct {
	terminal bool
	calls    *atomic.Int32
}

func (*nativeDelegationContextExtension) Name() string { return "delegation-context" }
func (e *nativeDelegationContextExtension) BeginRun(context.Context, agentcore.RunInfo) (agentcore.Extension, error) {
	return e, nil
}
func (e *nativeDelegationContextExtension) InterceptToolResult(_ context.Context, call agentcore.ToolCall, _ string, err error) agentcore.ToolResultDecision {
	if call.Name != subagent.ToolSpawnSubagent || err != nil {
		return agentcore.ToolResultDecision{}
	}
	if e.calls != nil {
		e.calls.Add(1)
	}
	return agentcore.ToolResultDecision{Terminate: e.terminal, AdditionalContexts: []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "delegation user context"},
		{Role: agentcore.RoleSystem, Content: "delegation system context"},
	}}
}

func TestNativeDelegationContinuationHonorsToolContextsAndTermination(t *testing.T) {
	for _, tc := range []struct{ terminal, failCheckpoint bool }{{}, {terminal: true}, {terminal: true, failCheckpoint: true}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			terminal := tc.terminal
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store := &nativeDelegationFaultStore{MemorySessionStore: agentcore.NewMemorySessionStore(), parent: "parent"}
			if tc.failCheckpoint {
				store.mode = "terminal checkpoint"
			}
			var parents, children int
			var hooks atomic.Int32
			runtime := PiSessionConfig{Pi: NativeAgentConfig{Options: json.RawMessage(`{"initialState":{}}`)}}
			runtime.Pi.Callback = func(ctx context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "stream" {
					return nil, fmt.Errorf("unexpected callback %s", method)
				}
				var response *ai.AssistantMessageEventStream
				if agentcore.DelegationDepth(ctx) == 0 {
					parents++
					if parents == 1 {
						response = nativeChildResponse(nativeChildThinking("parent-opaque"), nativeChildCall("spawn", subagent.ToolSpawnSubagent, `{"task":"child task"}`))
					} else {
						if terminal {
							t.Error("terminal continuation scheduled another parent model request")
						}
						for _, want := range []string{"delegation user context", "delegation system context", "child final"} {
							if !strings.Contains(string(params), want) {
								t.Errorf("parent request lost %s", want)
							}
						}
						response = nativeChildResponse(ai.ContentBlock{Type: "text", Text: "parent final"})
					}
				} else {
					children++
					if children == 1 {
						response = nativeChildResponse(nativeChildCall("ask", "ask", `{"question":"Which?"}`))
					} else {
						response = nativeChildResponse(ai.ContentBlock{Type: "text", Text: "child final"})
					}
				}
				message, err := response.Result(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(message)
			}
			plugin := &subagent.Plugin{RunFork: piForkRunner(runtime, true, store, agentcore.ToolChoice{}, nil)}
			a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(), Model: "test", Session: store, SessionID: "parent",
				Tools: agentcore.NewToolSet(ask.Tool{}), Policy: agentcore.NewAllowList("ask", subagent.ToolSpawnSubagent), Extensions: []agentcore.ExtensionFactory{plugin, &nativeDelegationContextExtension{terminal: terminal, calls: &hooks}}})
			if err != nil {
				t.Fatal(err)
			}
			run := func(resume bool) (PiRunResult, error) {
				host, err := a.OpenPiTools(ctx)
				if err != nil {
					return PiRunResult{}, err
				}
				defer host.Close()
				cfg := runtime
				cfg.Store, cfg.SessionID, cfg.Resume = store, "parent", resume
				cfg.Policy = agentcore.NewAllowList("ask", subagent.ToolSpawnSubagent)
				var input json.RawMessage
				if !resume {
					input = json.RawMessage(`"parent task"`)
				}
				return RunPi(ctx, PiRunConfig{Host: host, Session: cfg, Input: input, PricingKnown: true})
			}
			result, err := run(false)
			if err != nil || !result.Projection.Parked {
				t.Fatalf("initial park: %+v %v", result, err)
			}
			entries, _ := store.Log(ctx, "parent")
			id, _, pending := agentcore.PendingQuestion(entries)
			if !pending {
				t.Fatal("missing question")
			}
			lease, release, err := agentcore.AcquireSessionLease(ctx, store, "parent")
			if err != nil {
				t.Fatal(err)
			}
			_, err = agentcore.RecordSessionAnswer(lease, store, "parent", id, "approved")
			_ = release()
			if err != nil {
				t.Fatal(err)
			}
			result, err = run(true)
			if tc.failCheckpoint {
				if err == nil || !store.hit || parents != 1 || children != 2 || hooks.Load() != 1 {
					t.Fatalf("terminal checkpoint fault missed its boundary: %v calls=%d/%d hooks=%d", err, parents, children, hooks.Load())
				}
				result, err = run(true)
			}
			if err != nil || result.Projection.Parked || children != 2 || hooks.Load() != 1 {
				t.Fatalf("continuation failed: %+v %v", result, err)
			}
			wantParents, wantContexts := 2, 2
			if terminal {
				wantParents, wantContexts = 1, 0
				if result.Projection.StopReason != "toolUse" {
					t.Fatalf("terminal tool lost its stop reason: %+v", result.Projection)
				}
			}
			if parents != wantParents || strings.Count(string(result.State), `"agentrayDelegationContextId"`) != wantContexts || strings.Count(string(result.State), `"agentrayDelegationId"`) != 1 || !strings.Contains(string(result.State), "parent-opaque") {
				t.Fatalf("continuation lost contexts/history or termination: calls=%d state=%s", parents, result.State)
			}
			entries, _ = store.Log(ctx, "parent")
			recovered, err := recoverPiState(entries)
			if err != nil {
				t.Fatal(err)
			}
			if pending, err := piAnswerMessages(entries, recovered); err != nil || len(pending) != 0 {
				t.Fatalf("checkpoint did not settle deliveries: %s %v", pending, err)
			}
			if tc.failCheckpoint {
				return // The retry only attaches a receipt; it executes no tool span.
			}
			var spans []telemetry.RecordedSpan
			if err := json.Unmarshal(result.Telemetry, &spans); err != nil {
				t.Fatal(err)
			}
			if len(spans) < 2 || spans[0].Name != "agentray.delegation.resume" || spans[1].Name != "agentray.tool.execute" || spans[1].ParentID == nil || *spans[1].ParentID != spans[0].ID || spans[1].Attributes.Get("tool.name") != subagent.ToolSpawnSubagent {
				t.Fatalf("continuation tool telemetry is missing/misparented: %s", result.Telemetry)
			}
		})
	}
}

func TestNativeAuxiliarySummaryUsesGoAndPreservesSource(t *testing.T) {
	var calls int
	var traces []llm.TraceRecord
	provider := engine.StreamFn(func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
		calls++
		if len(ai.GetCurrentTools(transcript.Messages())) != 0 {
			t.Error("summary advertised tools")
		}
		raw := string(passiveNativeJSON(transcript))
		if !strings.Contains(raw, "source-opaque-signature") || !strings.Contains(raw, "Historical system context (source material only)") {
			t.Errorf("summary lost source: %s", raw)
		}
		if options["apiKey"] != "refreshed" {
			t.Error("summary lost key binding")
		}
		payload := options["onPayload"].(func(context.Context, json.RawMessage, json.RawMessage) (json.RawMessage, error))
		controlled, err := payload(ctx, json.RawMessage(`{"messages":[],"tool_choice":"required"}`), model)
		if err != nil || strings.Contains(string(controlled), `"tool_choice"`) {
			t.Errorf("summary tool controls missing: %s %v", controlled, err)
		}
		return nativeChildResponse(ai.ContentBlock{Type: "text", Text: " summary "}), nil
	})
	runtime := PiRuntimeConfig{NativeStream: provider}
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "stale"}}
	history := json.RawMessage(`[{"role":"system","content":"historical policy","timestamp":1},{"role":"user","content":"question","timestamp":2},{"role":"assistant","content":[{"type":"thinking","thinking":"private","thinkingSignature":"source-opaque-signature"},{"type":"text","text":"answer"}],"stopReason":"stop","timestamp":3}]`)
	result, usage, err := summarizePiHistoryWithUsage(context.Background(), runtime, tier, history, nativeAgentRevision, func(context.Context, string) (string, error) { return "refreshed", nil }, llm.SinkFunc(func(record llm.TraceRecord) { traces = append(traces, record) }))
	if err != nil || result != "summary" || calls != 1 || usage.InputTokens != 1 || len(traces) != 1 {
		t.Fatalf("Go summary failed: %q %+v calls=%d traces=%d %v", result, usage, calls, len(traces), err)
	}
}

func nativeChildThinking(signature string) ai.ContentBlock {
	return ai.ContentBlock{Type: "thinking", Thinking: "private", ThinkingSignature: &signature}
}

func TestNativeProviderRejectsUnportedAPIWithoutWorkerFallback(t *testing.T) {
	_, err := (ai.NativeProvider{}).Stream(context.Background(), json.RawMessage(`{"id":"test","api":"google-generative-ai","provider":"google"}`), ai.NormalizeContext(ai.Context{}), nil)
	if err == nil || err.Error() != `native Go provider for API "google-generative-ai" is not ported` {
		t.Fatalf("unexpected native dispatch: %v", err)
	}
}
