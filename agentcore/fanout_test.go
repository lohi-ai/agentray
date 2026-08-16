package agentcore_test

// Delegating wide.
//
// Fanning work out to sub-agents in parallel is the one move that makes a long
// run finish sooner rather than just cost less, and the loop supports it: a
// batch of parallel-eligible calls dispatches concurrently, spawn_subagent
// declares itself parallel, and the spawn cap is an atomic counter rather than a
// check-then-act.
//
// What none of that decides is what a wide batch does to the PARENT. A child may
// return up to 48 KB of model-visible answer, and the whole batch lands in the
// parent's window in one turn. Compaction cannot undo it: it runs between turns,
// and it keeps the recent tail verbatim — the tail IS the batch. So the cost of
// delegating wide is charged to the exact thing delegation is supposed to
// protect, and it scales with the width the model chooses.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
)

const fanoutMarker = "FANOUT-SUBTASK"

// fanoutWidth is how many children the parent spawns in one turn. It is well
// inside the derived MaxPerRun for this run's tool budget — the point is not to
// exceed a cap, it is that staying inside every cap is not enough.
const fanoutWidth = 8

// childAnswerBytes is what each child hands back. A real child that read a log,
// searched a corpus, or summarized a shard returns kilobytes, and the subagent
// plugin's own ceiling is 48 KB — this is well under it, so nothing here is
// pathological.
const childAnswerBytes = 6000

// fanoutProvider drives a parent that delegates wide and the children that
// answer it. Routing is by content, because children run concurrently with each
// other and a positional script would be nondeterministic.
type fanoutProvider struct {
	mu sync.Mutex

	parentTurns int
	childCalls  int
	summaries   int

	// requests is every message set the PARENT was shown, in order. The window
	// cost of the fan-out is read off this.
	requests [][]agentcore.Message
}

func (*fanoutProvider) Name() string        { return "fanout" }
func (*fanoutProvider) SupportsTools() bool { return true }

func (p *fanoutProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		return usageFor(req, agentcore.AssistantText(
			"## Goal\nReconcile the corpus\n## Progress\n### Done\n- [x] a batch of shards\n## Next Steps\n1. keep going")), nil
	}

	if isFanoutChild(req.Messages) {
		p.childCalls++
		// Each child reports one distinctive FINDING buried in a realistic amount
		// of supporting detail. The finding is the reason the child was spawned;
		// the detail is what makes delegating worth it in the first place.
		return usageFor(req, agentcore.AssistantText(
			strings.Repeat("shard detail line; reconciled against the clearing file. ", childAnswerBytes/56)+
				"\n"+childFinding(fanoutShard(req.Messages))+
				"\n"+goal.Done)), nil
	}

	p.parentTurns++
	n := p.parentTurns
	p.requests = append(p.requests, req.Messages)

	switch n {
	case 1:
		// One turn, the whole fan-out. This is the shape the capability exists
		// for and the shape a model told to parallelize will produce.
		calls := make([]agentcore.ToolCall, 0, fanoutWidth)
		for i := 0; i < fanoutWidth; i++ {
			calls = append(calls, agentcore.ToolCall{
				ID:   fmt.Sprintf("sp%d", i),
				Name: subagent.ToolSpawnSubagent,
				Arguments: fmt.Sprintf(`{"task":%q}`,
					fmt.Sprintf("%s: reconcile shard %d and report", fanoutMarker, i)),
			})
		}
		return usageFor(req, agentcore.ChatResponse{
			Message: agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: calls},
		}), nil
	default:
		return usageFor(req, agentcore.AssistantText("All shards reconciled.\n"+goal.Done)), nil
	}
}

func (p *fanoutProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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

func isFanoutChild(msgs []agentcore.Message) bool {
	for _, m := range msgs {
		if m.Role == agentcore.RoleUser && strings.Contains(m.Content, fanoutMarker) {
			return true
		}
	}
	return false
}

// fanoutShard reads back which shard this child was given, so its answer can
// carry a finding only that child could have produced.
func fanoutShard(msgs []agentcore.Message) int {
	for _, m := range msgs {
		if m.Role != agentcore.RoleUser {
			continue
		}
		var shard int
		if _, err := fmt.Sscanf(m.Content[strings.Index(m.Content, fanoutMarker):],
			fanoutMarker+": reconcile shard %d", &shard); err == nil {
			return shard
		}
	}
	return -1
}

// childFinding is the one line per child that the whole delegation was for.
func childFinding(shard int) string {
	return fmt.Sprintf("FINDING shard %d: unreconciled balance of %d in the regional clearing file",
		shard, 1000+shard)
}

// TestWideFanOutDoesNotBlowTheParentWindow is the round's headline. Run it with
// -race: the whole point of a wide batch is that it runs concurrently, and the
// parent's accounting, log and plan are all touched around it.
func TestWideFanOutDoesNotBlowTheParentWindow(t *testing.T) {
	prov := &fanoutProvider{}
	store := newE2EStore()

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 20
	limits.MaxToolCalls = 60
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "fanout",
			Tools:      agentcore.NewToolSet(),
			Policy:     agentcore.NewAllowList(subagent.ToolSpawnSubagent),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  "fanout",
		}},
		goal.Until("every shard reconciled"),
		subagent.SelfOnly(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), "Reconcile every shard, in parallel where you can."); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	requests := prov.requests
	children := prov.childCalls
	prov.mu.Unlock()

	if children < fanoutWidth {
		t.Fatalf("only %d of %d children ran: the batch did not actually fan out", children, fanoutWidth)
	}
	if len(requests) < 2 {
		t.Fatalf("the parent never saw the results of its fan-out (%d turns)", len(requests))
	}

	// The turn AFTER the batch is where the cost shows up: every child's answer
	// is now in the window, and it is the window the next provider call is
	// billed for.
	after := transcriptBytes(requests[1])
	t.Logf("fan-out of %d children, %d B each: parent window after the batch = %d B (budget %d B)",
		fanoutWidth, childAnswerBytes, after, limitsWindowBytes)

	if after > 2*limitsWindowBytes {
		t.Fatalf("one wide batch put the parent %d B into a %d B budget (%.1fx). Delegation is "+
			"supposed to keep work OUT of the parent's context; a batch that lands N children's "+
			"answers whole charges the parent for exactly the reading it delegated, and it scales "+
			"with a width the model picks",
			after, limitsWindowBytes, float64(after)/float64(limitsWindowBytes))
	}

	// The other half of the question, and the one that decides whether the
	// fan-out was WORTH anything: the parent has to still have the findings when
	// it writes its answer. Staying inside the budget by summarizing eight
	// children's reports away the turn they arrive is not a win — it spends eight
	// child runs plus a summarization call to arrive at a paraphrase.
	final := requests[len(requests)-1]
	seen, missing := 0, []int{}
	for i := 0; i < fanoutWidth; i++ {
		if strings.Contains(messagesText(final), childFinding(i)) {
			seen++
		} else {
			missing = append(missing, i)
		}
	}
	t.Logf("findings still visible when the parent answered: %d of %d (compaction summaries: %d)",
		seen, fanoutWidth, prov.summaries)
	if seen < fanoutWidth {
		t.Fatalf("the parent lost %d of %d delegated findings (shards %v) before it answered. "+
			"Each child's answer arrives whole, all at once, so the batch overruns the window on "+
			"the spot and compaction summarizes the reports away the turn they land — the run pays "+
			"for %d child runs and a summarization call, and reasons over a paraphrase",
			len(missing), fanoutWidth, missing, fanoutWidth)
	}
}

func messagesText(msgs []agentcore.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}
