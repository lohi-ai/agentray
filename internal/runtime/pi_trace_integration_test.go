package agentruntime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func TestPiRunnerNativeTraceRecordsEveryParentAndChildCall(t *testing.T) {
	ctx := observe.WithTraceID(piSessionContext(t), "root-run")
	var parents, children atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.Contains(string(piSessionJSON(body.Messages)), "PARENT-ONLY") {
			if parents.Add(1) == 1 {
				piChildSSE(w, "spawn_subagent", `{"task":"child task"}`, "")
			} else {
				piChildSSE(w, "", "", "parent finished")
			}
		} else {
			children.Add(1)
			piChildSSE(w, "", "", "child finished")
		}
	}))
	defer server.Close()
	st := newRecordingStore()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tracer = newTestSink(st)
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-trace", BaseURL: server.URL + "/v1", APIKey: "trace-secret-key"}}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY"}, tier, nil)
	if err != nil || result.Final != "parent finished" {
		t.Fatalf("tracing changed native result: %+v %v", result, err)
	}
	st.mu.Lock()
	rows := append([]storage.AgentLLMCall{}, st.rows...)
	st.mu.Unlock()
	if len(rows) != 3 || parents.Load() != 2 || children.Load() != 1 {
		t.Fatalf("lost or duplicated native calls: rows=%d parent=%d child=%d", len(rows), parents.Load(), children.Load())
	}
	seenChildren, totalInput := 0, 0
	nativeContexts := map[int]piTraceContext{}
	for _, row := range rows {
		if row.RunID != "root-run" || row.TokenInput != 7 || row.TokenOutput != 3 || row.NativeTraceJSON == "" || !row.CostUnpriced {
			t.Fatalf("bad per-call row: %+v", row)
		}
		totalInput += row.TokenInput
		if row.Depth == 1 {
			seenChildren++
			if row.SessionKey == p.SessionID || strings.Contains(row.NativeTraceJSON, "PARENT-ONLY") {
				t.Fatal("child trace inherited parent attribution/context")
			}
		}
		if strings.Contains(row.NativeTraceJSON, "trace-secret-key") {
			t.Fatal("native trace recorded provider credential")
		}
		var native struct {
			RequestID      int
			UpstreamCommit string
			Spans          []struct {
				Settled bool
				Name    string
			}
		}
		if err := json.Unmarshal(decodePiTrace(row.NativeTraceJSON, row.SessionKey, row.Seq, nativeContexts), &native); err != nil {
			t.Fatal(err)
		}
		if native.RequestID == 0 || native.UpstreamCommit != result.NativeRevision || len(native.Spans) == 0 {
			t.Fatalf("missing original Pi spans: %s", row.NativeTraceJSON)
		}
		for _, span := range native.Spans {
			if !span.Settled {
				t.Fatal("trace exported an unfinished span")
			}
		}
	}
	if seenChildren != 1 || totalInput != result.Usage.InputTokens {
		t.Fatalf("trace usage differs from parent+child bill: traces=%d result=%+v", totalInput, result.Usage)
	}
}

func TestPiTraceSinkPanicDoesNotChangeNativeResult(t *testing.T) {
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	var calls atomic.Int32
	p.Tracer = observe.SinkFunc(func(observe.TraceRecord) { calls.Add(1); panic("observer failed") })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { piChildSSE(w, "", "", "still done") }))
	defer server.Close()
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "native-trace", BaseURL: server.URL + "/v1", APIKey: "test"}}
	result, err := runner.runModelLoop(piSessionContext(t), p, RunOptions{Prompt: "test"}, tier, nil)
	if err != nil || result.Final != "still done" || calls.Load() != 1 {
		t.Fatalf("observer altered execution: %+v %v calls=%d", result, err, calls.Load())
	}
}
