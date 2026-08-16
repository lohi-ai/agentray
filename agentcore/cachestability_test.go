package agentcore_test

// What a long run actually re-bills.
//
// Prompt caching is the single biggest lever on the token cost of a long run:
// the transcript-so-far is re-sent on every turn, so if the provider can read it
// from cache instead of re-ingesting it, the run pays for the new tail only. The
// loop opts in with Config.PromptCacheKey and places a breakpoint with
// markCacheAnchors.
//
// A cache only pays if the PREFIX is stable. Two things have to hold, and
// neither is checked anywhere: the request's leading messages must be identical
// from turn to turn, and the breakpoint has to sit on a message that is itself
// stable — anchoring on a message the loop rewrites every turn caches a prefix
// that is guaranteed to miss.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

// cacheProbeProvider records the exact request view of every parent turn — after
// context hooks and after anchor placement, which is what the provider is
// actually billed on.
type cacheProbeProvider struct {
	mu        sync.Mutex
	turns     int
	summaries int
	finishAt  int
	requests  [][]agentcore.Message
}

func (*cacheProbeProvider) Name() string        { return "cache-probe" }
func (*cacheProbeProvider) SupportsTools() bool { return true }

func (p *cacheProbeProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		return usageFor(req, agentcore.AssistantText(
			"## Goal\nAudit the corpus\n## Progress\n### Done\n- [x] a batch\n## Next Steps\n1. keep going")), nil
	}

	p.turns++
	n := p.turns
	// Snapshot: the loop reuses its backing array between turns.
	p.requests = append(p.requests, append([]agentcore.Message(nil), req.Messages...))

	switch {
	case n >= p.finishAt:
		return usageFor(req, agentcore.AssistantText("done\n"+goal.Done)), nil
	case n%7 == 0:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("p%d", n), todo.ToolName, cachePlanArgs(n))), nil
	default:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n))), nil
	}
}

func (p *cacheProbeProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.ChatDelta, 4)
	go func() {
		defer close(ch)
		if resp.Message.Content != "" {
			ch <- agentcore.ChatDelta{ContentDelta: resp.Message.Content}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- agentcore.ChatDelta{ToolCall: &tc}
		}
		ch <- agentcore.ChatDelta{Done: true}
	}()
	return ch, nil
}

func cachePlanArgs(n int) string {
	return fmt.Sprintf(`{"items":[{"content":"audit batch %d","status":"in_progress"},`+
		`{"content":"file the reports","status":"pending"}]}`, n/7)
}

// sameMessage compares two request messages the way a provider's cache does:
// on what is sent, not on Go identity.
func sameMessage(a, b agentcore.Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.Name != b.Name || a.ToolCallID != b.ToolCallID {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i] != b.ToolCalls[i] {
			return false
		}
	}
	return true
}

// commonPrefix returns how many leading messages two requests share.
func commonPrefix(a, b []agentcore.Message) int {
	n := 0
	for n < len(a) && n < len(b) && sameMessage(a[n], b[n]) {
		n++
	}
	return n
}

// TestTheCachedPrefixIsStableAcrossTurns is the round's measurement. A run that
// re-bills its whole window every turn costs several times what it should, and
// nothing else in the suite would notice.
func TestTheCachedPrefixIsStableAcrossTurns(t *testing.T) {
	const turns = 300

	prov := &cacheProbeProvider{finishAt: turns}
	store := newE2EStore()
	plan := todo.NewStore()
	work := &e2eWorkTool{size: 600}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * turns
	limits.MaxToolCalls = 4 * turns
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:        prov,
			Model:           "cache-model",
			Tools:           agentcore.NewToolSet(work),
			Policy:          agentcore.NewAllowList("work", todo.ToolName),
			Limits:          &limits,
			Compaction:      &cs,
			Session:         store,
			SessionID:       "cache-stability",
			PromptCacheKey:  "agent-scope-1",
			ReasoningEffort: "",
		}},
		goal.Until("the corpus is audited"),
		todo.With(plan),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "Audit every shard in the corpus."); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	reqs := prov.requests
	summaries := prov.summaries
	prov.mu.Unlock()

	if len(reqs) < 50 {
		t.Fatalf("only %d turns recorded; not a long run", len(reqs))
	}

	// The question a cache actually asks. A breakpoint tells the provider to
	// store the prefix up to and including that message. That entry is worth
	// something only if it is still a prefix of the NEXT request — otherwise the
	// run pays the cache-write premium every turn and never reads one back.
	//
	// This is the measurement that matters, and it is not the same as "the
	// requests share a long prefix": they can share almost everything and still
	// miss, if the anchor is placed past the point where they diverge.
	readable, written := 0, 0
	for i := 0; i < len(reqs)-1; i++ {
		cur, next := reqs[i], reqs[i+1]
		idx := -1
		for j := range cur {
			if cur[j].CacheAnchor {
				idx = j
			}
		}
		if idx < 0 {
			t.Fatalf("turn %d carries no cache anchor although PromptCacheKey is set: the run opted "+
				"into caching and then never told the provider where the stable prefix ends", i+1)
		}
		written++
		if commonPrefix(cur[:idx+1], next) == idx+1 {
			readable++
		}
	}
	t.Logf("cache entries written %d, still a prefix of the next request %d", written, readable)

	// The headline: how much of each request could actually be served from the
	// previous turn's cache.
	totalBytes, cachedBytes, resets := 0, 0, 0
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1], reqs[i]
		n := commonPrefix(prev, cur)
		total := transcriptBytes(cur)
		shared := transcriptBytes(cur[:n])
		totalBytes += total
		cachedBytes += shared
		// A reset is a turn that shares almost nothing with the one before it —
		// compaction rewrites the window, and that is expected. What matters is
		// that it is RARE.
		if total > 0 && shared*4 < total {
			resets++
		}
	}
	pct := cachedBytes * 100 / max(totalBytes, 1)
	t.Logf("%d turns, %d compactions: %d%% of request bytes were a prefix of the previous request "+
		"(%d full resets)", len(reqs), summaries, pct, resets)

	// Compactions legitimately rewrite the window, so a handful of unreadable
	// entries is expected. Anything beyond that means the anchor is systematically
	// misplaced.
	if readable < written-2*summaries-1 {
		t.Fatalf("only %d of %d cache entries were still a prefix of the next request (%d "+
			"compactions explain at most %d). The anchor goes on the FINAL message of the request, "+
			"but context hooks append a regenerated reminder there — it is not part of the "+
			"append-only history, so the cached prefix ends inside content guaranteed to differ "+
			"next turn. The run pays the cache-WRITE premium every turn and never reads one back",
			readable, written, summaries, 2*summaries)
	}
	if pct < 60 {
		t.Fatalf("only %d%% of each request is a prefix of the one before it, so a long run re-bills "+
			"most of its window every turn (%d resets over %d turns)", pct, resets, len(reqs))
	}
}
