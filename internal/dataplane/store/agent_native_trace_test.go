package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestNativeTraceSQLRoundTripAndConcurrentSequences(t *testing.T) {
	st := openConvTestStore(t)
	user, project := seedConvProject(t, st)
	ctx := context.Background()
	run, err := st.CreateAgentRun(ctx, project, "", "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	const count = 24
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			native := fmt.Sprintf(`{"requestID":%d,"context":{"messages":[{"role":"user","content":[{"type":"image","mimeType":"image/png","data":"abc"}],"extension":{"signature":"opaque > &"}}]},"response":{"role":"assistant","content":[{"type":"text","text":"answer"}]},"spans":[{"id":1,"settled":true}]}`, i+1)
			_, err := st.RecordAgentLLMCall(ctx, AgentLLMCall{RunID: run, SessionKey: fmt.Sprintf("%s/child-%d", run, i), Depth: 1, Provider: "native", Model: "m", MessagesJSON: `[]`, Response: "answer", NativeTraceJSON: native, TokenInput: 7, ToolCallsJSON: `[{"id":"reused","name":"read"}]`})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.AgentLLMCallTrace(ctx, user, project, run)
	if err != nil || len(rows) != count {
		t.Fatalf("trace read: rows=%d %v", len(rows), err)
	}
	for i, row := range rows {
		if row.Seq != i+1 {
			t.Fatalf("concurrent traces reused/out-of-order sequence: %+v", row)
		}
		var native struct {
			RequestID int
			Context   struct{ Messages []map[string]json.RawMessage }
			Spans     []struct{ Settled bool }
		}
		if json.Unmarshal([]byte(row.NativeTraceJSON), &native) != nil || native.RequestID == 0 || len(native.Context.Messages) != 1 || len(native.Context.Messages[0]["extension"]) == 0 || len(native.Spans) != 1 || !native.Spans[0].Settled {
			t.Fatalf("native SQL payload changed: %s", row.NativeTraceJSON)
		}
	}
	metrics, err := st.ListAgentLLMCallMetrics(ctx, user, project, run, 0, 100)
	if err != nil || len(metrics) != count {
		t.Fatalf("metrics read: %v", err)
	}
	for _, row := range metrics {
		if row.NativeTraceJSON != "" || row.MessagesJSON != "" {
			t.Fatal("metrics query included full trace payload")
		}
	}
	if _, err := st.RecordAgentLLMCall(ctx, AgentLLMCall{RunID: run, NativeTraceJSON: `{broken`}); err == nil {
		t.Fatal("invalid native JSON committed")
	}
	seq, err := st.RecordAgentLLMCall(ctx, AgentLLMCall{RunID: run, ToolCallsJSON: `[{"id":"reused","name":"read"}]`})
	if err != nil || seq != count+1 {
		t.Fatalf("failed trace advanced sequence or held lock: %d %v", seq, err)
	}
	if err := st.AttachAgentLLMCallGates(ctx, run, `[{"call_id":"reused","allowed":false,"reason":"legacy receipt"}]`); err != nil {
		t.Fatal(err)
	}
	rows, err = st.AgentLLMCallTrace(ctx, user, project, run)
	if err != nil || len(rows) != count+1 {
		t.Fatalf("gate trace read: rows=%d %v", len(rows), err)
	}
	for _, row := range rows {
		var gates []json.RawMessage
		if err := json.Unmarshal([]byte(row.ToolGatesJSON), &gates); err != nil {
			t.Fatal(err)
		}
		if row.Seq <= count && len(gates) != 0 {
			t.Fatal("run-wide legacy gate overwrote a native invocation")
		}
		if row.Seq == count+1 && len(gates) != 1 {
			t.Fatal("legacy invocation lost its gate overlay")
		}
	}
}
