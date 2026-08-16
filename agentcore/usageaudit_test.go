package agentcore_test

// What the run says it spent, against what it actually spent.
//
// A long run makes three kinds of provider call and only one of them is the
// obvious one: its own turns, the summarization call every compaction makes, and
// a whole child run per delegation. All three are billed. If RunResult.Usage
// omits any of them the run under-reports, and the under-report is not cosmetic
// — the budget gate is handed that same number, so a run whose real spend is
// several times its reported spend runs past a ceiling that was supposed to stop
// it. "Minimal LLM token usage" is not a property you can pursue on a number
// that is wrong.
//
// The provider is the single point every one of those calls goes through, so it
// is the honest place to count from.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

const auditChildMarker = "AUDIT-SUBTASK"

// auditProvider is the ground truth: every call the run makes, of every kind,
// passes through here and is added up.
type auditProvider struct {
	mu sync.Mutex

	workTurns int

	parentTurns int
	childCalls  int
	summaries   int
	spawns      int

	// spent is the sum of the Usage handed back on every call — parent turns,
	// child turns and summarizations alike.
	spent agentcore.Usage

	// byKind splits the same total by which kind of call spent it, so a failure
	// names the leak ("the whole child column is missing") instead of only its
	// size.
	byKind map[string]int
}

func (*auditProvider) Name() string        { return "audit" }
func (*auditProvider) SupportsTools() bool { return true }

func (p *auditProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var resp agentcore.ChatResponse
	kind := "parent"
	switch {
	case len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization"):
		p.summaries++
		kind = "summary"
		resp = usageFor(req, agentcore.AssistantText(
			"## Goal\nAudit\n## Progress\n### Done\n- [x] a batch\n## Next Steps\n1. keep going"))

	case isAuditChild(req.Messages):
		p.childCalls++
		kind = "child"
		if hasToolResult(req.Messages) {
			resp = usageFor(req, agentcore.AssistantText("shard reconciled\n"+goal.Done))
		} else {
			resp = usageFor(req, agentcore.AssistantToolCall(
				fmt.Sprintf("cw%d", p.childCalls), "work", fmt.Sprintf(`{"shard":%d}`, p.childCalls)))
		}

	default:
		p.parentTurns++
		n := p.parentTurns
		switch {
		case n >= p.workTurns:
			resp = usageFor(req, agentcore.AssistantText("all shards audited\n"+goal.Done))
		case n%9 == 0 && p.spawns < 12:
			p.spawns++
			resp = usageFor(req, agentcore.AssistantToolCall(
				fmt.Sprintf("sp%d", p.spawns), subagent.ToolSpawnSubagent,
				fmt.Sprintf(`{"task":%q}`, fmt.Sprintf("%s: reconcile shard %d", auditChildMarker, p.spawns))))
		default:
			resp = usageFor(req, agentcore.AssistantToolCall(
				fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n)))
		}
	}

	p.spent.InputTokens += resp.Usage.InputTokens
	p.spent.OutputTokens += resp.Usage.OutputTokens
	if p.byKind == nil {
		p.byKind = map[string]int{}
	}
	p.byKind[kind] += resp.Usage.InputTokens + resp.Usage.OutputTokens
	return resp, nil
}

func (p *auditProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.ChatDelta, 4)
	go func() {
		defer close(ch)
		// Report usage the way the wire formats do: input tokens are known
		// before a single output token exists, so they go out first, and the
		// final delta restates the running totals. A provider that only
		// reported on Done would be the easier case and would not exercise the
		// merge.
		ch <- agentcore.ChatDelta{Usage: agentcore.Usage{InputTokens: resp.Usage.InputTokens}}
		if resp.Message.Content != "" {
			ch <- agentcore.ChatDelta{ContentDelta: resp.Message.Content}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- agentcore.ChatDelta{ToolCall: &tc}
		}
		ch <- agentcore.ChatDelta{Done: true, StopReason: resp.StopReason, Usage: resp.Usage}
	}()
	return ch, nil
}

func isAuditChild(msgs []agentcore.Message) bool {
	for _, m := range msgs {
		if m.Role == agentcore.RoleUser && strings.Contains(m.Content, auditChildMarker) {
			return true
		}
	}
	return false
}

// TestRunAccountsForEveryProviderCallItMakes is the audit. Every kind of call
// has to be in the total, and the three kinds are counted separately so a
// failure names which one leaked.
func TestRunAccountsForEveryProviderCallItMakes(t *testing.T) {
	const workTurns = 220

	prov := &auditProvider{workTurns: workTurns}
	store := newE2EStore()
	plan := todo.NewStore()
	work := &e2eWorkTool{size: 600}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * workTurns
	limits.MaxToolCalls = 4 * workTurns
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "audit-model",
			Tools:      agentcore.NewToolSet(work),
			Policy:     agentcore.NewAllowList("work", todo.ToolName, subagent.ToolSpawnSubagent),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  "usage-audit",
		}},
		goal.Until("every shard audited"),
		todo.With(plan),
		subagent.SelfOnly(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "Audit every shard in the corpus.")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	spent := prov.spent
	byKind := prov.byKind
	parentTurns, childCalls, summaries, spawns := prov.parentTurns, prov.childCalls, prov.summaries, prov.spawns
	prov.mu.Unlock()

	// The run has to have actually done all three things, or the audit is
	// vacuous — a test that reports perfect accounting of a run that never
	// delegated and never compacted proves nothing about either.
	if summaries < 5 {
		t.Fatalf("only %d compactions: the run never exercised summarization spend", summaries)
	}
	if spawns < 5 || childCalls < 2*spawns {
		t.Fatalf("only %d children (%d child calls): the run never exercised delegated spend",
			spawns, childCalls)
	}

	got := res.Usage.InputTokens + res.Usage.OutputTokens
	want := spent.InputTokens + spent.OutputTokens
	t.Logf("provider saw %d calls (%d parent turns, %d child calls, %d summarizations) = %d tokens "+
		"%v; the run reported %d",
		parentTurns+childCalls+summaries, parentTurns, childCalls, summaries, want, byKind, got)

	if got != want {
		missing := want - got
		t.Fatalf("the run reported %d tokens against %d actually spent — %d unaccounted (%.1f%%). "+
			"The budget gate is handed the reported number, so a run whose real spend is larger "+
			"than what it admits to runs past the ceiling that was meant to stop it. Spend by kind "+
			"of call was %v across %d parent turns, %d child calls and %d summarizations — compare "+
			"the missing amount against those columns to see which one leaked",
			got, want, missing, float64(missing)*100/float64(want),
			byKind, parentTurns, childCalls, summaries)
	}
}
