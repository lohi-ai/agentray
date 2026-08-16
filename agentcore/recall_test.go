package agentcore_test

// What the run learned early, against what it can still answer with at the end.
//
// Every other long-run test in this package asks whether the machinery survives:
// the window stays bounded, the pin persists, the plan comes back, the tokens
// add up. None of them asks the question the run exists to answer — after a
// hundred compactions, is the final answer still built on what the run actually
// found?
//
// A run that discovers 24 facts in its first 30 turns and then works for another
// 300 has compacted its discovery phase away many times over. Each compaction
// folds the previous checkpoint forward, so a fact survives only if every fold
// between then and the end carried it. One drop is permanent — nothing later
// re-reads the transcript. The failure is silent and it is the worst kind: the
// run finishes, reports success, and answers with a subset of what it knows.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

// findingsTotal is how many facts the run discovers before the long middle
// stretch that compacts them away.
const findingsTotal = 24

var findingRe = regexp.MustCompile(`FINDING-(\d+)=([A-Z0-9]+)`)

func findingCode(k int) string { return fmt.Sprintf("FINDING-%d=SHARD%03dOK", k, k) }

// findingsIn returns the distinct finding numbers readable anywhere in a span,
// which is exactly the set the model could answer from.
func findingsIn(msgs []agentcore.Message) map[string]string {
	found := map[string]string{}
	for _, m := range msgs {
		for _, mt := range findingRe.FindAllStringSubmatch(m.Content, -1) {
			found[mt[1]] = mt[2]
		}
		for _, tc := range m.ToolCalls {
			for _, mt := range findingRe.FindAllStringSubmatch(tc.Arguments, -1) {
				found[mt[1]] = mt[2]
			}
		}
	}
	return found
}

// recallLookupTool is the discovery half of the run: each call returns a bulky
// result with one finding in it, the way a real query or file read does.
type recallLookupTool struct{ pad int }

func (*recallLookupTool) Name() string { return "lookup" }
func (*recallLookupTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        "lookup",
		Description: "Inspect one shard and report what it holds.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"shard": map[string]any{"type": "integer"},
			},
			"required": []string{"shard"},
		},
	}
}

func (t *recallLookupTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Shard int `json:"shard"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	// The finding leads, because a summarizer reading a bounded serialization of
	// this result sees its head. Burying it would test truncation, not recall.
	return findingCode(in.Shard) + "\n" + strings.Repeat("shard detail. ", t.pad), nil
}

// recallProvider plays three roles, and the honest one is the summarizer.
//
// It is written as a COMPETENT model, not a generous one: on the update path it
// carries forward every finding stated in the previous checkpoint plus every
// finding in the new span, and it writes them out in full. That is the best a
// real model could do with what it is handed. Anything the run loses under this
// provider is lost by the machinery — the span it chose to summarize, the
// checkpoint ceiling, the fold — and not by a model that forgot.
type recallProvider struct {
	mu sync.Mutex

	workTurns int

	turns     int
	summaries int

	// answered is what the run could still see when it wrote its final answer.
	answered map[string]string
	// widestSummary is the largest checkpoint the run ever produced, in bytes.
	widestSummary int
}

func (*recallProvider) Name() string        { return "recall" }
func (*recallProvider) SupportsTools() bool { return true }

func (p *recallProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		summary := p.checkpoint(req.Messages)
		if len(summary) > p.widestSummary {
			p.widestSummary = len(summary)
		}
		return usageFor(req, agentcore.AssistantText(summary)), nil
	}

	p.turns++
	n := p.turns
	switch {
	case n <= findingsTotal:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("lk%d", n), "lookup", fmt.Sprintf(`{"shard":%d}`, n))), nil
	case n >= p.workTurns:
		// The answer is written from context and nothing else — the same
		// constraint the real model is under.
		p.answered = findingsIn(req.Messages)
		keys := make([]string, 0, len(p.answered))
		for k, v := range p.answered {
			keys = append(keys, "FINDING-"+k+"="+v)
		}
		sort.Strings(keys)
		return usageFor(req, agentcore.AssistantText(
			"Audit complete. "+strings.Join(keys, " ")+"\n"+goal.Done)), nil
	default:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n))), nil
	}
}

// checkpoint writes the structured summary a competent model would write: the
// findings it can see, all of them, in the section the format reserves for
// completed work.
func (p *recallProvider) checkpoint(msgs []agentcore.Message) string {
	found := findingsIn(msgs)
	nums := make([]int, 0, len(found))
	for k := range found {
		var n int
		fmt.Sscanf(k, "%d", &n)
		nums = append(nums, n)
	}
	sort.Ints(nums)

	var b strings.Builder
	b.WriteString("## Goal\nAudit every shard and report each shard's code.\n")
	b.WriteString("## Progress\n### Done\n")
	for _, n := range nums {
		// Write back the code as READ, never as reconstructed from the shard
		// number. A summarizer that re-derives the fact would silently repair a
		// checkpoint the ceiling had damaged, and a real model cannot do that —
		// it only has what the previous checkpoint told it.
		fmt.Fprintf(&b, "- [x] shard %d inspected, code FINDING-%d=%s\n", n, n, found[fmt.Sprint(n)])
	}
	b.WriteString("## Next Steps\n1. Inspect the remaining shards\n2. Report every code\n")
	return b.String()
}

func (p *recallProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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
		ch <- agentcore.ChatDelta{Done: true, StopReason: resp.StopReason, Usage: resp.Usage}
	}()
	return ch, nil
}

// runRecall drives the audit run and returns its final answer alongside the
// provider's record of what the run could still see when it answered.
// maxSummaryTokens of 0 leaves the checkpoint ceiling at its default.
func runRecall(t *testing.T, maxSummaryTokens int) (agentcore.RunResult, *recallProvider) {
	t.Helper()
	const workTurns = 320

	prov := &recallProvider{workTurns: workTurns}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * workTurns
	limits.MaxToolCalls = 4 * workTurns
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500
	if maxSummaryTokens > 0 {
		cs.MaxSummaryTokens = maxSummaryTokens
	}

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "recall-model",
			Tools:      agentcore.NewToolSet(&recallLookupTool{pad: 60}, &e2eWorkTool{size: 600}),
			Policy:     agentcore.NewAllowList("lookup", "work", todo.ToolName),
			Limits:     &limits,
			Compaction: &cs,
			Session:    newE2EStore(),
			SessionID:  "recall-audit",
		}},
		goal.Until("every shard's code reported"),
		todo.With(todo.NewStore()),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(),
		"Inspect every shard and report each shard's code in your final answer.")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	return res, prov
}

// TestTheAnswerStillKnowsWhatTheRunFoundEarly is the recall audit.
func TestTheAnswerStillKnowsWhatTheRunFoundEarly(t *testing.T) {
	res, prov := runRecall(t, 0)

	prov.mu.Lock()
	answered, summaries, widest := prov.answered, prov.summaries, prov.widestSummary
	prov.mu.Unlock()

	// A recall test that never compacted proves nothing.
	if summaries < 10 {
		t.Fatalf("only %d compactions: the findings never had to survive one", summaries)
	}

	var lost []int
	for k := 1; k <= findingsTotal; k++ {
		if _, ok := answered[fmt.Sprint(k)]; !ok {
			lost = append(lost, k)
		}
	}
	t.Logf("%d compactions, widest checkpoint %d B; %d of %d findings reached the answer",
		summaries, widest, findingsTotal-len(lost), findingsTotal)

	if len(lost) > 0 {
		t.Fatalf("%d of %d findings did not survive to the final answer (%v). Every fold between "+
			"discovery and the end was handed the previous checkpoint and asked to carry it "+
			"forward, and the provider here does carry it — so a finding that is gone was "+
			"dropped by the machinery, not forgotten by a model. The run still reports success: "+
			"it answers with a subset of what it found and nothing anywhere says so. "+
			"%d compactions, widest checkpoint %d B",
			len(lost), findingsTotal, lost, summaries, widest)
	}

	// The answer has to actually contain them, not merely have had them in view.
	for k := 1; k <= findingsTotal; k++ {
		if !strings.Contains(res.Final, findingCode(k)) {
			t.Fatalf("finding %d was in context but missing from the final answer", k)
		}
	}
}

// The same run with a checkpoint ceiling too small to hold every finding.
//
// Losing facts here is correct and expected — a budget is a budget, and the run
// can look a shard up again. What is NOT acceptable is answering with a fact
// that is wrong: the checkpoint is fed to the next fold as the previous summary
// and carried forward, so a code mangled by the ceiling becomes something the
// run believes and reports for the rest of its life, with nothing marking it
// damaged. Under pressure the run must forget, not confabulate.
func TestUnderCheckpointPressureTheRunForgetsRatherThanConfabulates(t *testing.T) {
	res, prov := runRecall(t, 120) // 480 bytes: room for a handful of findings

	prov.mu.Lock()
	answered, summaries := prov.answered, prov.summaries
	prov.mu.Unlock()

	if summaries < 10 {
		t.Fatalf("only %d compactions: the ceiling never had to bite", summaries)
	}
	if len(answered) == findingsTotal {
		t.Fatalf("all %d findings fit under a 480-byte checkpoint — the ceiling did not bite, "+
			"so this test proved nothing about what happens when it does", findingsTotal)
	}

	for k, code := range answered {
		var n int
		if _, err := fmt.Sscanf(k, "%d", &n); err != nil {
			t.Fatalf("the run answered with an unparseable finding number %q", k)
		}
		if want := strings.TrimPrefix(findingCode(n), fmt.Sprintf("FINDING-%d=", n)); code != want {
			t.Fatalf("the run answered finding %d as %q, but its real code is %q. The ceiling cut "+
				"through the fact instead of dropping it, and a half-code reads as a whole one all "+
				"the way to the final answer.\n---\n%s", n, code, want, res.Final)
		}
	}
	t.Logf("%d compactions under a 480-byte checkpoint: %d of %d findings kept, every one of them intact",
		summaries, len(answered), findingsTotal)
}
