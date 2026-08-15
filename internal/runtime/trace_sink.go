package agentruntime

import (
	"container/list"
	"context"
	"encoding/json"
	"sync"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// storeTraceSink persists every per-LLM-call TraceRecord to agent_llm_calls,
// keyed by the run id the TracingProvider stamped on the record (the trace id
// the Runner set via observe.WithTraceID). It is the DB-backed half of the
// trace fan-out — the queryable source for the monitoring console — and lives
// here, in the consumer, because it is the one place that may import both
// agentcore and storage (storage never imports agentcore).
//
// Records with no trace id (e.g. classifier calls made outside any run) have no
// run to attach to and are dropped — fail-safe, never an orphan row. All writes
// are best-effort: tracing must never break a run, so the storage error is
// swallowed, mirroring FileSink.
//
// # Why the request is stored as a delta
//
// A TraceRecord carries the whole request, because that is what an observer
// should receive: a self-describing record of one call. Persisting it whole is
// a different question, and the answer used to be wrong. Successive requests in
// one run are ~99% the same bytes — each is the previous one plus the turn's
// new messages — so a long run wrote the same conversation back to Postgres
// once per turn. Measured on a 4,200-turn run: 51 MiB of request messages,
// against an 8 MiB durable session log holding strictly more information.
//
// So the sink stores the DIFFERENCE. Each call records how much of the previous
// call's context it kept (KeepPrefix) and the messages that follow. A normal
// turn keeps everything and adds two messages; a compaction keeps only the
// system prompt and writes the (small) rebuilt window. Reconstruction is exact
// — this is compression, not sampling, and no call loses its context.
type storeTraceSink struct {
	store llmCallRecorder
	prev  *contextCache
}

// llmCallRecorder is the sink's whole dependency on storage: append one row,
// get back the sequence number the next delta chains onto. Narrowing it to this
// is what lets the delta encoding be tested for exactness without a database —
// and exactness is the property that matters, because a diff that loses a
// message corrupts every trace silently.
type llmCallRecorder interface {
	RecordAgentLLMCall(ctx context.Context, c storage.AgentLLMCall) (int, error)
}

// NewStoreTraceSink returns a TraceSink that writes LLM-call traces to Postgres.
func NewStoreTraceSink(store *storage.Store) observe.Sink {
	return &storeTraceSink{store: store, prev: newContextCache(maxTracedSessions)}
}

// maxTracedSessions bounds how many in-flight sessions the sink remembers the
// last context for. It is a CACHE, not a ledger: a miss costs one uncompressed
// row (a keyframe) and nothing else, so eviction can never corrupt a trace —
// which is what makes a fixed bound safe here rather than a leak waiting for a
// run that never signals completion. A parent plus its concurrent children are
// a handful of sessions each, so this holds many simultaneous runs.
const maxTracedSessions = 512

// forcedKeyframeInterval caps how long a delta chain grows before the sink
// writes a full context again. Long chains are correct but fragile: a row that
// cannot be read leaves every row after it without the base it extends.
//
// 100 is where the size curve flattens (measured in
// TestTraceDeltaActuallyCompresses: 9.6x at 50, 10.5x at 100, and no further
// gain at 200 or 400 — past a point the keyframes stop being what costs), so it
// is the shortest chain that gives up nothing. Shorter chains are strictly
// better for everything but size, which is why the tie goes to 100.
const forcedKeyframeInterval = 100

// tracedContext is what the sink remembers about a session's previous call.
type tracedContext struct {
	messages []agentcore.Message
	seq      int
	sinceKey int // calls since the last keyframe
}

func (s *storeTraceSink) Record(r observe.TraceRecord) {
	if r.TraceID == "" {
		return // no run correlation → nothing to attach the trace to
	}
	session := r.SessionKey
	if session == "" {
		session = r.TraceID
	}

	base, keep, delta := s.encode(session, r.Messages)
	msgs, err := json.Marshal(delta)
	if err != nil {
		// An unencodable message must not cost the whole trace row: drop the
		// context, keep the metrics, and start a fresh chain.
		msgs, base, keep = []byte("[]"), 0, 0
	}
	calls, _ := json.Marshal(r.ToolCalls)

	seq, err := s.store.RecordAgentLLMCall(context.Background(), storage.AgentLLMCall{
		RunID:         rootRunID(r.TraceID),
		SessionKey:    session,
		Depth:         r.Depth,
		BaseSeq:       base,
		KeepPrefix:    keep,
		Provider:      r.Provider,
		Model:         r.Model,
		MessagesJSON:  string(msgs),
		Tools:         r.Tools,
		Response:      r.Response,
		ToolCallsJSON: string(calls),
		StopReason:    r.StopReason,
		TokenInput:    r.Usage.InputTokens,
		TokenOutput:   r.Usage.OutputTokens,
		CostUSD:       r.Usage.CostUSD,
		LatencyMS:     int(r.LatencyMS),
		Streamed:      r.Streamed,
		Error:         r.Err,
	})
	if err != nil {
		// The row did not land, so the next call must not chain onto it — forget
		// the session and let the next call write a keyframe. Without this a
		// single failed insert would silently break every delta after it.
		s.prev.forget(session)
		return
	}
	s.prev.put(session, tracedContext{
		messages: r.Messages,
		seq:      seq,
		sinceKey: s.nextSinceKey(session, base),
	})
}

// encode diffs this call's context against the previous call of the same
// session, returning the base seq to chain onto (0 = keyframe), how many of the
// base's messages are retained, and the messages that follow.
//
// The diff is a common-prefix diff rather than a plain append, because a
// request is not simply the last one plus new messages: context hooks re-render
// trailing reminders (the run plan, cache anchors) on every turn, so the last
// message of the previous request is usually replaced rather than kept. A
// prefix diff handles that in the same shape it handles compaction, which
// replaces a long head with a short summary.
func (s *storeTraceSink) encode(session string, msgs []agentcore.Message) (baseSeq, keepPrefix int, delta []agentcore.Message) {
	prev, ok := s.prev.get(session)
	if !ok || prev.seq == 0 || prev.sinceKey >= forcedKeyframeInterval {
		return 0, 0, msgs
	}
	keep := commonPrefix(prev.messages, msgs)
	// A diff that keeps almost nothing is not worth chaining: the row would be
	// nearly a keyframe anyway, and an independent row is cheaper to read.
	if keep == 0 {
		return 0, 0, msgs
	}
	return prev.seq, keep, msgs[keep:]
}

// nextSinceKey advances the keyframe counter: a keyframe resets it, a delta
// extends the chain.
func (s *storeTraceSink) nextSinceKey(session string, baseSeq int) int {
	if baseSeq == 0 {
		return 0
	}
	prev, ok := s.prev.get(session)
	if !ok {
		return 1
	}
	return prev.sinceKey + 1
}

// commonPrefix counts the leading messages two contexts agree on.
func commonPrefix(a, b []agentcore.Message) int {
	n := min(len(a), len(b))
	for i := range n {
		if !sameMessage(a[i], b[i]) {
			return i
		}
	}
	return n
}

// sameMessage compares two messages for trace-diff purposes. Tool-call identity
// is part of it: two assistant turns with identical text but different call ids
// are different turns, and treating them as equal would drop a call from the
// reconstructed context.
func sameMessage(a, b agentcore.Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.Name != b.Name || a.ToolCallID != b.ToolCallID {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID ||
			a.ToolCalls[i].Name != b.ToolCalls[i].Name ||
			a.ToolCalls[i].Arguments != b.ToolCalls[i].Arguments {
			return false
		}
	}
	return true
}

// contextCache is a bounded LRU of the last context per session. Concurrency is
// real here: a parent and its children trace from different goroutines through
// the same sink.
type contextCache struct {
	mu    sync.Mutex
	max   int
	order *list.List               // front = most recently used
	items map[string]*list.Element // session -> element holding a cacheEntry
}

type cacheEntry struct {
	session string
	ctx     tracedContext
}

func newContextCache(max int) *contextCache {
	return &contextCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *contextCache) get(session string) (tracedContext, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[session]
	if !ok {
		return tracedContext{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).ctx, true
}

func (c *contextCache) put(session string, tc tracedContext) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[session]; ok {
		el.Value.(*cacheEntry).ctx = tc
		c.order.MoveToFront(el)
		return
	}
	c.items[session] = c.order.PushFront(&cacheEntry{session: session, ctx: tc})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).session)
	}
}

func (c *contextCache) forget(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[session]; ok {
		c.order.Remove(el)
		delete(c.items, session)
	}
}
