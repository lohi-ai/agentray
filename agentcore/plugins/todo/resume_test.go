package todo_test

// The plan across a crash.
//
// A long run's three pieces of durable intent are the user's requirement (pinned
// through compaction), the goal (EntryGoal, recovered by the loop), and the
// plan. Two of the three come back after a crash. This is about the third.
//
// It matters most in exactly the case it fails in: a run long enough to have
// been compacted has had its original task summarized away, so the checklist is
// the agent's remaining record of what it decided to do — and a resumed run that
// starts with an empty one re-plans from a summary, which is how a recovered run
// quietly does different work than the one it is recovering.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

type planLogStore struct {
	mu  sync.Mutex
	log map[string][]agentcore.SessionEntry
}

func newPlanLogStore() *planLogStore {
	return &planLogStore{log: map[string][]agentcore.SessionEntry{}}
}

func (s *planLogStore) Append(_ context.Context, id string, e agentcore.SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log[id] = append(s.log[id], e)
	return nil
}

func (s *planLogStore) Log(_ context.Context, id string) ([]agentcore.SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentcore.SessionEntry(nil), s.log[id]...), nil
}

const (
	planStepA = "Enumerate the shards in the ledger corpus"
	planStepB = "Reconcile the CLEARING-7742 discrepancy"
	planStepC = "File one report per region"
)

const resumePlanArgs = `{"items":[` +
	`{"content":"` + planStepA + `","status":"completed"},` +
	`{"content":"` + planStepB + `","status":"in_progress"},` +
	`{"content":"` + planStepC + `","status":"pending"}]}`

// TestPlanComesBackOnResume is the requirement "the run retains its todo list"
// taken literally: across a process boundary, not just across compaction.
func TestPlanComesBackOnResume(t *testing.T) {
	sess := newPlanLogStore()
	const sessionID = "plan-resume"

	// First run: the agent writes its plan and then the process dies mid-task.
	// The death has to be real — a run that ENDS writes a leaf, and a leaf is
	// what says "this plan is history". Nothing is recovered from a finished run,
	// which is the next test.
	first := todo.NewStore()
	limits := agentcore.DefaultLimits()
	agent, err := agentcore.Build(planCfg{cfg: agentcore.Config{
		Provider:  &crashAfterPlan{},
		Model:     "test",
		Tools:     agentcore.NewToolSet(),
		Policy:    agentcore.NewAllowList(todo.ToolName),
		Limits:    &limits,
		Session:   sess,
		SessionID: sessionID,
	}}, todo.With(first))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "audit the corpus"); err == nil {
		t.Fatal("the first run was supposed to die mid-task; a clean finish tests nothing")
	}
	if got := first.Render(); !strings.Contains(got, planStepB) {
		t.Fatalf("the first run did not hold the plan it wrote:\n%s", got)
	}

	// Second run: a new process, a new store, resuming the same session. This is
	// the only thing under test — what the recovered run knows about its own plan.
	second := todo.NewStore()
	limits2 := agentcore.DefaultLimits()
	resumed, err := agentcore.Build(planCfg{cfg: agentcore.Config{
		Provider:      agentcore.NewFauxProvider(agentcore.AssistantText("carrying on")),
		Model:         "test",
		Tools:         agentcore.NewToolSet(),
		Policy:        agentcore.NewAllowList(todo.ToolName),
		Limits:        &limits2,
		Session:       sess,
		SessionID:     sessionID,
		ResumeSession: true,
	}}, todo.With(second))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := resumed.Prompt(context.Background(), "carry on"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got := second.Render()
	if got == "" {
		t.Fatal("the resumed run came back with no plan at all: the checklist is the one piece of a " +
			"long run's intent that a crash erases, and it is the piece the model navigates by " +
			"once its original task has been summarized away")
	}
	if !strings.Contains(got, planStepB) {
		t.Fatalf("the resumed run does not know which step it was on:\n%s", got)
	}
	for _, want := range []string{planStepA, planStepC} {
		if !strings.Contains(got, want) {
			t.Fatalf("the recovered plan is missing %q:\n%s", want, got)
		}
	}

	// The status must survive too, not just the text: a plan that comes back with
	// every step pending sends the agent to redo finished work.
	for _, it := range second.List() {
		if it.Content == planStepA && it.Status != todo.StatusCompleted {
			t.Fatalf("the completed step came back as %q, so the run would redo it", it.Status)
		}
		if it.Content == planStepB && it.Status != todo.StatusInProgress {
			t.Fatalf("the active step came back as %q, so the run lost its place", it.Status)
		}
	}
}

// A finished run's checklist is history, not context. Chaining a new task onto
// the same session must start clean — inheriting the previous run's list would
// put steps the new task never had in front of the model, and the completed ones
// would read as work already done on it. This is the same rule the goal gate
// uses, and it is the reason the recovery above has to be triggered by a crash.
func TestFinishedRunsPlanIsNotInherited(t *testing.T) {
	sess := newPlanLogStore()
	const sessionID = "plan-finished"

	first := todo.NewStore()
	limits := agentcore.DefaultLimits()
	agent, err := agentcore.Build(planCfg{cfg: agentcore.Config{
		Provider: agentcore.NewFauxProvider(
			agentcore.AssistantToolCall("p1", todo.ToolName, resumePlanArgs),
			agentcore.AssistantText("audit filed"),
		),
		Model:     "test",
		Tools:     agentcore.NewToolSet(),
		Policy:    agentcore.NewAllowList(todo.ToolName),
		Limits:    &limits,
		Session:   sess,
		SessionID: sessionID,
	}}, todo.With(first))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "audit the corpus"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	next := todo.NewStore()
	limits2 := agentcore.DefaultLimits()
	chained, err := agentcore.Build(planCfg{cfg: agentcore.Config{
		Provider:      agentcore.NewFauxProvider(agentcore.AssistantText("on it")),
		Model:         "test",
		Tools:         agentcore.NewToolSet(),
		Policy:        agentcore.NewAllowList(todo.ToolName),
		Limits:        &limits2,
		Session:       sess,
		SessionID:     sessionID,
		ResumeSession: true,
	}}, todo.With(next))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := chained.Prompt(context.Background(), "now do something else entirely"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := next.Render(); got != "" {
		t.Fatalf("a new task inherited the finished run's checklist:\n%s", got)
	}
}

// crashAfterPlan lets the plan reach the log and then kills the run, which is
// the only way to leave the log in the shape a crash leaves it: the work is
// recorded, but no leaf says the run ended.
type crashAfterPlan struct {
	mu    sync.Mutex
	calls int
}

func (*crashAfterPlan) Name() string        { return "crash-after-plan" }
func (*crashAfterPlan) SupportsTools() bool { return true }

func (p *crashAfterPlan) Chat(_ context.Context, _ agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return agentcore.AssistantToolCall("p1", todo.ToolName, resumePlanArgs), nil
	}
	return agentcore.ChatResponse{}, errors.New("provider died mid-task")
}

func (p *crashAfterPlan) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.ChatDelta, 4)
	go func() {
		defer close(ch)
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- agentcore.ChatDelta{ToolCall: &tc}
		}
		ch <- agentcore.ChatDelta{Done: true}
	}()
	return ch, nil
}

type planCfg struct{ cfg agentcore.Config }

func (planCfg) Name() string { return "config" }

func (c planCfg) Register(r *agentcore.Registry) error { return r.ApplyConfig(c.cfg) }
