package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func TestPiTracePreservesNativeDataAndProjectsAccounting(t *testing.T) {
	raw := json.RawMessage(`{"requestID":1,"startedAtMs":1234,"durationMs":12,"upstreamCommit":"revision","model":{"provider":"native","id":"model"},"context":{"systemPrompt":"system","messages":[{"role":"user","content":"request","extension":{"opaque":true}}],"tools":[{"name":"read"}]},"response":{"role":"assistant","content":[{"type":"thinking","thinking":"private","thinkingSignature":"signature"},{"type":"text","text":"answer"}],"stopReason":"stop","usage":{"input":8,"output":2,"cacheRead":3,"cacheWrite":0,"cost":{"total":0}}},"spans":[{"id":2,"parentId":1,"name":"agentray.ai.request","settled":true}]}`)
	var records []observe.TraceRecord
	cfg := bindPiTrace(agentcore.PiConfig{}, observe.SinkFunc(func(r observe.TraceRecord) { records = append(records, r) }), false, "fallback")
	ctx := agentcore.WithDelegationDepth(agentcore.WithRunSession(observe.WithTraceID(context.Background(), "new-run"), "old-run/child"), 2)
	cfg.OnTrace(ctx, raw)
	if len(records) != 1 {
		t.Fatal("trace dropped")
	}
	r := records[0]
	if r.TraceID != "new-run" || r.SessionKey != "old-run/child" || r.Depth != 2 || r.Timestamp.UnixMilli() != 1234 || r.LatencyMS != 12 || r.Model != "model" || r.Provider != "native" || r.Response != "answer" || r.Usage.InputTokens != 8 || r.Usage.CacheReadTokens != 3 || !r.Usage.CostUnpriced {
		t.Fatalf("incorrect trace projection: %+v", r)
	}
	if len(r.Messages) != 2 || r.Messages[0].Role != agentcore.RoleSystem || strings.Contains(r.Response, "private") || !samePiJSON(r.NativeTrace, raw) {
		t.Fatalf("lost opaque native fields or exposed thinking as response: %+v", r)
	}
	st := newRecordingStore()
	sink := newTestSink(st)
	sink.Record(r)
	if len(st.rows) != 1 || !samePiJSON(decodePiTrace(st.rows[0].NativeTraceJSON, r.SessionKey, st.rows[0].Seq, map[int]piTraceContext{}), raw) {
		t.Fatal("store sink lost authoritative native trace")
	}
	steps := agentcore.FoldSteps(recordsFromCalls(st.rows))
	if len(steps) != 1 || !samePiJSON(steps[0].NativeTrace, raw) || steps[0].SessionKey != r.SessionKey || steps[0].Depth != 2 {
		t.Fatal("trace API discarded native data or attribution")
	}
	if _, err := agentcore.NewReplayProvider(recordsFromCalls(st.rows)...).Chat(context.Background(), agentcore.ChatRequest{}); err == nil {
		t.Fatal("legacy replay accepted a native display projection")
	}
}

func TestPiTraceRecordsProviderFailureWithoutInventingResponse(t *testing.T) {
	var got observe.TraceRecord
	cfg := bindPiTrace(agentcore.PiConfig{}, observe.SinkFunc(func(r observe.TraceRecord) { got = r }), true, "run")
	cfg.OnTrace(context.Background(), json.RawMessage(`{"startedAtMs":1,"durationMs":2,"model":{"provider":"p","id":"m"},"context":{"messages":[]},"error":{"name":"Error","message":"provider failed"},"spans":[]}`))
	if got.TraceID != "run" || got.SessionKey != "run" || got.Err != "provider failed" || got.Response != "" || got.Usage.InputTokens != 0 {
		t.Fatalf("failure trace fabricated a response: %+v", got)
	}
}

func TestPiTraceCompressionPreservesOpaqueChangesAndRunBoundaries(t *testing.T) {
	st := newRecordingStore()
	sink := newTestSink(st)
	originals := []json.RawMessage{}
	for i := 0; i < 160; i++ {
		messages := []json.RawMessage{}
		for j := 0; j <= i; j++ {
			signature := "opaque"
			if i >= 80 && j == 1 {
				signature = "changed-signature"
			}
			msg, _ := json.Marshal(map[string]any{"role": "user", "content": strings.Repeat("text", 40), "timestamp": j, "extension": map[string]any{"signature": signature}})
			messages = append(messages, msg)
		}
		raw, _ := json.Marshal(map[string]any{"context": map[string]any{"messages": messages, "systemPrompt": "system"}, "response": map[string]any{"role": "assistant", "content": []any{}}, "spans": []any{map[string]any{"id": i + 1, "settled": true}}})
		originals = append(originals, raw)
		cfg := bindPiTrace(agentcore.PiConfig{}, sink, true, "run-one")
		cfg.OnTrace(agentcore.WithRunSession(context.Background(), "shared-session"), raw)
	}
	records := recordsFromCalls(st.rows)
	storedBytes, originalBytes := 0, 0
	for i, record := range records {
		if !samePiJSON(record.NativeTrace, originals[i]) {
			t.Fatalf("native trace %d lost opaque fields: %s", i, record.NativeTrace)
		}
		storedBytes += len(st.rows[i].NativeTraceJSON)
		originalBytes += len(originals[i])
	}
	if storedBytes*8 >= originalBytes {
		t.Fatalf("native contexts were not compressed: stored=%d original=%d", storedBytes, originalBytes)
	}
	// A resumed session under a new run owns a fresh SQL sequence space.
	cfg := bindPiTrace(agentcore.PiConfig{}, sink, true, "run-two")
	cfg.OnTrace(agentcore.WithRunSession(context.Background(), "shared-session"), originals[len(originals)-1])
	last := st.rows[len(st.rows)-1]
	var delta piTraceDelta
	_ = json.Unmarshal([]byte(last.NativeTraceJSON), &delta)
	if last.BaseSeq != 0 || delta.BaseSeq != 0 || !samePiJSON(recordsFromCalls([]storage.AgentLLMCall{last})[0].NativeTrace, originals[len(originals)-1]) {
		t.Fatal("native trace used another run's delta base")
	}
}

func TestPiTraceMissingBaseDoesNotInventNativeContext(t *testing.T) {
	raw := json.RawMessage(`{"context":{"messages":[{"role":"user","content":"one"},{"role":"user","content":"two"}]},"response":{"role":"assistant","content":[]}}`)
	_, _, messages, _ := piTraceMessages(raw)
	encoded, _ := encodePiTrace(raw, messages[:1], 7)
	for _, bases := range []map[int]piTraceContext{{}, {7: {session: "other", messages: messages[:1]}}} {
		value := decodePiTrace(encoded, "session", 8, bases)
		if !strings.Contains(string(value), "pi-trace-unavailable") || strings.Contains(string(value), `"base_seq":0`) {
			t.Fatalf("invented a missing native prefix: %s", value)
		}
	}
}

func TestPiTraceUsesAttemptPricingKnowledge(t *testing.T) {
	for _, known := range []bool{false, true} {
		var got observe.TraceRecord
		cfg := bindPiTrace(agentcore.PiConfig{}, observe.SinkFunc(func(r observe.TraceRecord) { got = r }), !known, "run")
		raw, _ := json.Marshal(map[string]any{
			"model":    map[string]any{"id": "fallback", "provider": "openai"},
			"attempt":  map[string]any{"rung": 1, "providerId": "row-b", "generation": 0, "pricingKnown": known},
			"response": json.RawMessage(`{"role":"assistant","content":[],"stopReason":"stop","usage":{"input":2,"output":1,"cacheRead":0,"cacheWrite":0,"cost":{"total":0}}}`),
		})
		cfg.OnTrace(context.Background(), raw)
		if got.Usage.CostUnpriced == known || got.Usage.InputTokens != 2 || !samePiJSON(got.NativeTrace, raw) {
			t.Fatal("attempt pricing inherited primary flag", got.Usage)
		}
	}
}
