package agentcore

import (
	"context"
	"sync/atomic"
	"testing"
)

// chunkBarrier is a barrierTool that additionally counts completions, so a
// later sequential tool can prove the whole parallel group finished first.
type chunkBarrier struct {
	barrierTool
	done *atomic.Int32
}

func (c *chunkBarrier) Run(ctx context.Context, args string) (string, error) {
	out, err := c.barrierTool.Run(ctx, args)
	c.done.Add(1)
	return out, err
}

// TestMixedBatchChunksParallelRuns pins the chunked-dispatch contract: in a
// batch [parallel, parallel, sequential], the two parallel calls still run
// concurrently as a group (under the old all-or-nothing rule the sequential
// neighbor forced the whole batch sequential and the barrier pair would time
// out), and the sequential call runs only after the group completes. Trace
// order stays the model's order.
func TestMixedBatchChunksParallelRuns(t *testing.T) {
	arrived := make(chan string, 2)
	release := make(chan struct{})
	var done atomic.Int32
	qa := &chunkBarrier{barrierTool{name: "qa", arrived: arrived, release: release}, &done}
	qb := &chunkBarrier{barrierTool{name: "qb", arrived: arrived, release: release}, &done}
	go func() {
		<-arrived
		<-arrived
		close(release)
	}()

	var wSawDone int32
	w := funcTool{name: "w", run: func(context.Context, string) (string, error) {
		wSawDone = done.Load()
		return "wrote", nil
	}}

	batch := ChatResponse{
		Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "qa", Arguments: "{}"},
			{ID: "c2", Name: "qb", Arguments: "{}"},
			{ID: "c3", Name: "w", Arguments: "{}"},
		}},
		StopReason: "tool_calls",
	}
	agent, err := New(Config{
		Provider: NewFauxProvider(batch, AssistantText("all done")),
		Model:    "test",
		Tools:    NewToolSet(qa, qb, w),
		Policy:   NewAllowList("qa", "qb", "w"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "run all three")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(res.Tools) != 3 {
		t.Fatalf("expected 3 tool traces, got %d", len(res.Tools))
	}
	for i, want := range []string{"qa", "qb", "w"} {
		if res.Tools[i].Tool != want {
			t.Fatalf("trace order not preserved: %+v", res.Tools)
		}
	}
	for _, tr := range res.Tools {
		if tr.Error != "" {
			t.Fatalf("tool %q errored (group serialized?): %s", tr.Tool, tr.Error)
		}
	}
	if wSawDone != 2 {
		t.Fatalf("sequential tool ran before the parallel group finished (saw %d/2 done)", wSawDone)
	}
}

// TestParallelGroupAfterSequentialBarrier pins that a parallel group forms
// anywhere in the batch, not just at the front: [sequential, parallel,
// parallel] runs the trailing pair concurrently after the barrier.
func TestParallelGroupAfterSequentialBarrier(t *testing.T) {
	arrived := make(chan string, 2)
	release := make(chan struct{})
	qa := &barrierTool{name: "qa", arrived: arrived, release: release}
	qb := &barrierTool{name: "qb", arrived: arrived, release: release}
	go func() {
		<-arrived
		<-arrived
		close(release)
	}()

	w := funcTool{name: "w", run: func(context.Context, string) (string, error) {
		return "wrote", nil
	}}

	batch := ChatResponse{
		Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "w", Arguments: "{}"},
			{ID: "c2", Name: "qa", Arguments: "{}"},
			{ID: "c3", Name: "qb", Arguments: "{}"},
		}},
		StopReason: "tool_calls",
	}
	agent, err := New(Config{
		Provider: NewFauxProvider(batch, AssistantText("all done")),
		Model:    "test",
		Tools:    NewToolSet(w, qa, qb),
		Policy:   NewAllowList("qa", "qb", "w"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "write then fan out")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(res.Tools) != 3 {
		t.Fatalf("expected 3 tool traces, got %d", len(res.Tools))
	}
	for _, tr := range res.Tools {
		if tr.Error != "" {
			t.Fatalf("tool %q errored (trailing group serialized?): %s", tr.Tool, tr.Error)
		}
	}
}
