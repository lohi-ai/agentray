package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// The trace is stored as a diff, so the only thing that really matters about it
// is that the diff is EXACT. Compression that silently drops a message is worse
// than no compression: the run detail still renders, the step still has a
// context, and the context is simply not the one the model was shown — which is
// the exact failure the log invariant exists to catch elsewhere, reintroduced in
// the layer people use to investigate it.
//
// These tests therefore assert the round trip first (every call reconstructs
// byte for byte) and the size win second. Both are needed: a keyframe-always
// encoder passes the first, and a lossy encoder passes the second.

// recordingStore stands in for Postgres: it assigns per-run sequence numbers the
// way RecordAgentLLMCall's INSERT does and keeps the rows.
type recordingStore struct {
	mu   sync.Mutex
	rows []storage.AgentLLMCall
	next map[string]int
	fail map[int]bool // 1-based call ordinals that must fail the insert
	n    int
}

func newRecordingStore() *recordingStore {
	return &recordingStore{next: map[string]int{}, fail: map[int]bool{}}
}

func (s *recordingStore) RecordAgentLLMCall(_ context.Context, c storage.AgentLLMCall) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.fail[s.n] {
		return 0, fmt.Errorf("insert failed")
	}
	s.next[c.RunID]++
	c.Seq = s.next[c.RunID]
	s.rows = append(s.rows, c)
	return c.Seq, nil
}

func (s *recordingStore) storedBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		n += len(r.MessagesJSON)
	}
	return n
}

// newTestSink builds a sink over the fake store.
func newTestSink(st llmCallRecorder) *storeTraceSink {
	return &storeTraceSink{store: st, prev: newContextCache(maxTracedSessions)}
}

// --- a realistic request sequence ---------------------------------------------

func msg(role agentcore.Role, content string) agentcore.Message {
	return agentcore.Message{Role: role, Content: content}
}

// runShape generates the request sequence of a long run: a system prompt, turns
// that append an assistant message and a tool result, a trailing plan reminder
// re-rendered every turn, and a compaction every `compactEvery` turns that
// replaces the older span with a summary.
//
// The trailing reminder is the detail that makes this worth generating rather
// than hand-writing: it means a request is NOT the previous one plus new
// messages, so a naive append-only diff would be wrong on every single turn and
// still look right in a hand-written two-turn fixture.
func runShape(turns, compactEvery int) [][]agentcore.Message {
	system := msg(agentcore.RoleSystem, "You are a careful agent.")
	body := []agentcore.Message{}
	out := make([][]agentcore.Message, 0, turns)

	for i := 1; i <= turns; i++ {
		if compactEvery > 0 && i%compactEvery == 0 {
			// Compaction: the head is replaced by a summary, a short tail survives.
			keep := body
			if len(keep) > 16 {
				keep = keep[len(keep)-16:]
			}
			body = append([]agentcore.Message{
				msg(agentcore.RoleSystem, fmt.Sprintf("[context summary of earlier conversation]\nturn %d checkpoint", i)),
			}, keep...)
		}
		body = append(body,
			agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{
				{ID: fmt.Sprintf("c%d", i), Name: "work", Arguments: fmt.Sprintf(`{"n":%d}`, i)},
			}},
			agentcore.Message{Role: agentcore.RoleTool, ToolCallID: fmt.Sprintf("c%d", i), Name: "work",
				Content: fmt.Sprintf("result %d %s", i, strings.Repeat("x", 400))},
		)
		// The full request: prompt + history + a freshly rendered trailing plan.
		req := make([]agentcore.Message, 0, len(body)+2)
		req = append(req, system)
		req = append(req, body...)
		req = append(req, msg(agentcore.RoleSystem, fmt.Sprintf("[run plan]\nstep %d of the plan is in progress", i)))
		out = append(out, req)
	}
	return out
}

// feed writes one request sequence through the sink as a session's calls.
func feed(sink *storeTraceSink, traceID, session string, reqs [][]agentcore.Message) {
	for _, r := range reqs {
		sink.Record(observe.TraceRecord{
			TraceID:    traceID,
			SessionKey: session,
			Provider:   "p",
			Model:      "m",
			Messages:   r,
		})
	}
}

// reconstructed folds the stored rows back into per-call contexts using the
// SAME reconstruction the Lab uses, so the test cannot pass with a decoder that
// only exists in the test.
func reconstructed(rows []storage.AgentLLMCall) [][]agentcore.Message {
	records := recordsFromCalls(rows)
	out := make([][]agentcore.Message, len(records))
	for i, r := range records {
		out[i] = r.Messages
	}
	return out
}

func assertRoundTrip(t *testing.T, want [][]agentcore.Message, rows []storage.AgentLLMCall) {
	t.Helper()
	got := reconstructed(rows)
	if len(got) != len(want) {
		t.Fatalf("reconstructed %d calls, wrote %d", len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("call %d: reconstructed %d messages, sent %d", i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			if !sameMessage(got[i][j], want[i][j]) {
				t.Fatalf("call %d message %d differs:\n got %+v\nwant %+v", i, j, got[i][j], want[i][j])
			}
		}
	}
}

// TestTraceDeltaRoundTripsExactly is the correctness claim: over a run with
// compaction and a re-rendered trailing reminder, every stored call
// reconstructs to the exact request that was sent.
func TestTraceDeltaRoundTripsExactly(t *testing.T) {
	st := newRecordingStore()
	reqs := runShape(200, 17)
	feed(newTestSink(st), "run-1", "run-1", reqs)
	assertRoundTrip(t, reqs, st.rows)
}

// TestTraceDeltaActuallyCompresses is the other half. Without it, an encoder
// that gave up and wrote a keyframe every time would pass every correctness
// test in this file while leaving the 51 MiB problem exactly where it was.
func TestTraceDeltaActuallyCompresses(t *testing.T) {
	st := newRecordingStore()
	reqs := runShape(200, 17)
	feed(newTestSink(st), "run-1", "run-1", reqs)

	// Both sides measured as the JSON that actually lands in messages_json —
	// comparing content bytes against encoded bytes would flatter or penalise
	// the encoder for reasons that have nothing to do with the encoding.
	full := 0
	for _, r := range reqs {
		encoded, err := jsonMarshal(r)
		if err != nil {
			t.Fatal(err)
		}
		full += len(encoded)
	}
	stored := st.storedBytes()
	ratio := float64(full) / float64(stored)
	t.Logf("whole requests %d KiB -> stored %d KiB (%.1fx)", full/1024, stored/1024, ratio)
	if ratio < 8 {
		t.Fatalf("delta encoding saved almost nothing: %.1fx (stored %d of %d bytes)", ratio, stored, full)
	}
}

// TestTraceDeltaSurvivesAFailedInsert covers the gap case. A row that never
// landed cannot be chained onto, and a sink that forgot to notice would produce
// a chain pointing at a base that does not exist — every later call in the run
// reconstructing against nothing.
func TestTraceDeltaSurvivesAFailedInsert(t *testing.T) {
	st := newRecordingStore()
	st.fail[5] = true // the fifth call's insert fails
	reqs := runShape(40, 0)
	feed(newTestSink(st), "run-1", "run-1", reqs)

	if len(st.rows) != len(reqs)-1 {
		t.Fatalf("expected one dropped row, got %d of %d", len(st.rows), len(reqs))
	}
	// Everything that DID land must still reconstruct: the calls before the gap
	// against their own chain, the calls after it against a fresh keyframe.
	want := append(append([][]agentcore.Message{}, reqs[:4]...), reqs[5:]...)
	assertRoundTrip(t, want, st.rows)
}

// TestTraceDeltaChainsAreBounded pins the forced keyframe. A chain that grew
// without limit would make one unreadable row strand every row after it, and
// force any reader to walk to the start of the run.
func TestTraceDeltaChainsAreBounded(t *testing.T) {
	st := newRecordingStore()
	feed(newTestSink(st), "run-1", "run-1", runShape(300, 0))

	chain := 0
	longest := 0
	for _, r := range st.rows {
		if r.BaseSeq == 0 {
			chain = 0
			continue
		}
		chain++
		longest = max(longest, chain)
	}
	if longest > forcedKeyframeInterval {
		t.Fatalf("delta chain ran %d rows past a keyframe, cap is %d", longest, forcedKeyframeInterval)
	}
	if longest == 0 {
		t.Fatal("no chains at all — the encoder is writing keyframes only")
	}
}

// TestTraceSeparatesSubagentSessions is the attribution fix. A parent and its
// children share a provider and a ctx, so their calls arrive interleaved
// through one sink; each must be recorded against its own session, and — the
// part that actually breaks without it — each must diff against its OWN
// previous context rather than whichever agent happened to call last.
func TestTraceSeparatesSubagentSessions(t *testing.T) {
	st := newRecordingStore()
	sink := newTestSink(st)

	parent := runShape(30, 0)
	child := runShape(30, 0)
	// Interleave them the way a parent that delegates every other turn does.
	for i := range parent {
		feed(sink, "run-1", "run-1", parent[i:i+1])
		sink.Record(observe.TraceRecord{
			TraceID: "run-1", SessionKey: "run-1/call-" + fmt.Sprint(i), Depth: 1,
			Provider: "p", Model: "m", Messages: child[i],
		})
	}

	var parentRows, childRows []storage.AgentLLMCall
	for _, r := range st.rows {
		if r.SessionKey == "run-1" {
			parentRows = append(parentRows, r)
		} else {
			childRows = append(childRows, r)
			if r.Depth != 1 {
				t.Fatalf("child call recorded at depth %d, want 1", r.Depth)
			}
		}
	}
	if len(parentRows) != len(parent) || len(childRows) != len(child) {
		t.Fatalf("attribution lost calls: %d parent, %d child", len(parentRows), len(childRows))
	}
	// Each agent's calls still reconstruct despite the other's landing between
	// every pair of them — which is the thing that breaks if the sink diffs
	// against "the last call" instead of "the last call of this session".
	assertRoundTrip(t, parent, parentRows)
	for i, row := range childRows {
		assertRoundTrip(t, child[i:i+1], childRows[i:i+1])
		if row.BaseSeq != 0 {
			t.Fatalf("child %d chained onto seq %d, but each child's first call is its own keyframe", i, row.BaseSeq)
		}
	}
}

// TestTraceContextCacheIsBounded guards the one piece of unbounded-looking
// state. The sink remembers a context per live session; if that map only ever
// grew, a process serving many runs would hold every run's last window forever.
// Eviction must be safe, not just present — an evicted session falls back to a
// keyframe, so the trace stays exact.
func TestTraceContextCacheIsBounded(t *testing.T) {
	st := newRecordingStore()
	sink := &storeTraceSink{store: st, prev: newContextCache(4)}

	reqs := runShape(3, 0)
	for i := range 20 {
		feed(sink, "run-1", fmt.Sprintf("session-%d", i), reqs)
	}
	if n := len(sink.prev.items); n > 4 {
		t.Fatalf("context cache holds %d sessions, cap is 4", n)
	}
	// Every session's calls still reconstruct, evictions included.
	for i := range 20 {
		var rows []storage.AgentLLMCall
		for _, r := range st.rows {
			if r.SessionKey == fmt.Sprintf("session-%d", i) {
				rows = append(rows, r)
			}
		}
		assertRoundTrip(t, reqs, rows)
	}
}

// TestLegacyTraceRowsStillReconstruct covers the rows already in the database:
// written before the delta encoding existed, they carry the whole request with
// base_seq 0 and keep_prefix 0, which the reconstruction must read as "this IS
// the context". They are not backfilled, so this is the only thing standing
// between an upgrade and every historical run losing its trace.
func TestLegacyTraceRowsStillReconstruct(t *testing.T) {
	reqs := runShape(3, 0)
	rows := []storage.AgentLLMCall{}
	for i, r := range reqs {
		msgs, err := jsonMarshal(r)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, storage.AgentLLMCall{
			Seq: 0, BaseSeq: 0, KeepPrefix: 0, // exactly what an old row scans as
			MessagesJSON: msgs, Response: fmt.Sprintf("answer %d", i),
		})
	}
	assertRoundTrip(t, reqs, rows)
}

// jsonMarshal is a tiny helper so the legacy-row test builds its payloads the
// same way the old sink did.
func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
