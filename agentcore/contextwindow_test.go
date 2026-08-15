package agentcore

import (
	"context"
	"strings"
	"testing"
)

// The bug this file exists for: before the window capped it, the loop compacted
// at a fixed 200k regardless of the model. Point a run at anything smaller and
// compaction never fires — the transcript grows past what the model can hold and
// the provider rejects it, which no retry or escalation can rescue because the
// transcript is simply too big.
func TestBudgetIsCappedByTheModelWindow(t *testing.T) {
	// A 32k model with the default (300k) configured ceiling must compact well
	// inside 32k, not at 300k.
	got := effectiveBudget(defaultContextTokenBudget, 32_000)
	if got >= 32_000 {
		t.Fatalf("budget %d does not fit a 32000-token window", got)
	}
	if !shouldCompact(msgsOfTokens(33_000), got) {
		t.Fatal("a transcript larger than the model window did not trigger compaction")
	}
}

// The other direction: the configured ceiling is a ceiling. A 1M-token model
// must not license a 1M-token transcript when the operator asked for less —
// that is the cost control.
func TestConfiguredBudgetStillCapsALargeWindow(t *testing.T) {
	if got := effectiveBudget(120_000, 1_000_000); got != 120_000 {
		t.Fatalf("effectiveBudget(120000, 1000000) = %d, want the configured 120000", got)
	}
}

// An unknown window (0) is "nobody could tell us", not "a window of zero". It
// must leave the configured budget alone rather than collapsing it, or every
// self-hosted endpoint would compact on the first turn.
func TestUnknownWindowLeavesTheConfiguredBudgetAlone(t *testing.T) {
	if got := effectiveBudget(150_000, 0); got != 150_000 {
		t.Fatalf("effectiveBudget(150000, 0) = %d, want 150000", got)
	}
	if got := effectiveBudget(0, 0); got != defaultContextTokenBudget {
		t.Fatalf("effectiveBudget(0, 0) = %d, want the default %d", got, defaultContextTokenBudget)
	}
}

// A context window holds the answer as well as the prompt. Budgeting the whole
// window means the loop only compacts once the input alone has filled it, with
// no room left to reply in — so the cap must reserve output headroom.
func TestBudgetReservesRoomToAnswerIn(t *testing.T) {
	const window = 200_000
	got := effectiveBudget(defaultContextTokenBudget, window)
	if got > window-outputHeadroomTokens {
		t.Fatalf("budget %d leaves less than %d tokens to answer in", got, outputHeadroomTokens)
	}
}

// A window smaller than the headroom must not produce a zero or negative
// budget: shouldCompact reads 0 as "use the default 300k", so collapsing to
// zero would disable compaction on the *smallest* window — the exact inversion
// of what the cap is for.
func TestATinyWindowStillProducesAWorkingBudget(t *testing.T) {
	for _, window := range []int{1_000, 8_192, 16_385, 32_000} {
		got := effectiveBudget(defaultContextTokenBudget, window)
		if got <= 0 {
			t.Fatalf("window %d produced budget %d, which disables compaction", window, got)
		}
		if got >= window {
			t.Fatalf("window %d produced budget %d, which does not fit", window, got)
		}
	}
}

// The ladder is the reason the window lives on the rung. A run that escalates
// from a large-window model to a small-window one must re-derive its budget, or
// it carries the first rung's headroom onto a model that cannot hold it.
func TestEscalationRederivesTheBudgetForTheNewRung(t *testing.T) {
	big := effectiveBudget(defaultContextTokenBudget, 1_000_000)
	small := effectiveBudget(defaultContextTokenBudget, 32_000)
	if big <= small {
		t.Fatalf("a 1M-token rung (%d) must budget more than a 32k one (%d)", big, small)
	}
	// The transcript that was comfortable on the big rung must trigger
	// compaction on the small one.
	msgs := msgsOfTokens(40_000)
	if shouldCompact(msgs, big) {
		t.Fatal("a 40k transcript should be fine on a 1M-token model")
	}
	if !shouldCompact(msgs, small) {
		t.Fatal("a 40k transcript must compact on a 32k model")
	}
}

// End to end through a real run, and the pair below is the point: the SAME
// transcript must compact on a small-window model and not on an undeclared one.
// Without the wiring (Config → registry → agent → ladder → budget) every unit
// test above can pass while the loop still reads the raw configured ceiling, so
// only a run proves it.
func TestARunWithASmallWindowActuallyCompacts(t *testing.T) {
	if n := windowRunCompactions(t, 32_000); n == 0 {
		t.Fatal("a run on a 32k model never compacted; the window did not reach the loop")
	}
}

func TestTheSameRunWithNoDeclaredWindowDoesNotCompact(t *testing.T) {
	if n := windowRunCompactions(t, 0); n != 0 {
		t.Fatalf("a run with no declared window compacted %d times; the budget is not the configured one", n)
	}
}

// windowRunCompactions drives an identical bulky run at a given declared window
// and reports how many compactions the durable log recorded. Holding everything
// but the window fixed is what makes the pair above a controlled comparison
// rather than two independent assertions.
func windowRunCompactions(t *testing.T, window int) int {
	t.Helper()

	work := bulkTool{size: 60_000}
	call := func(id string) ChatResponse {
		return ChatResponse{Message: Message{
			Role:      RoleAssistant,
			ToolCalls: []ToolCall{{ID: id, Name: "work", Arguments: "{}"}},
		}}
	}
	fp := &FauxProvider{Responses: []ChatResponse{
		call("c1"), call("c2"), call("c3"),
		{Message: Message{Role: RoleAssistant, Content: "done"}},
	}}

	store := NewMemorySessionStore()
	agent, err := New(Config{
		Provider:      fp,
		Model:         "m",
		ContextWindow: window,
		Tools:         NewToolSet(work),
		Policy:        NewAllowList("work"),
		Session:       store,
		SessionID:     "s1",
		Limits: &Limits{
			MaxTurns: 8, MaxToolCalls: 8,
			MaxToolResultLen: 64 * 1024,
			MaxContextTokens: defaultContextTokenBudget,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	log, err := store.Log(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	n := 0
	for _, e := range log {
		if e.Kind == EntryCompaction && e.Final {
			n++
		}
	}
	return n
}

// bulkTool returns a payload large enough that a few calls cross a small
// window, so compaction is exercised for real rather than merely configured.
type bulkTool struct{ size int }

func (bulkTool) Name() string { return "work" }
func (bulkTool) Schema() ToolSchema {
	return ToolSchema{Name: "work", Description: "does work", Parameters: map[string]any{"type": "object"}}
}
func (b bulkTool) Run(context.Context, string) (string, error) {
	return strings.Repeat("x", b.size), nil
}

// msgsOfTokens builds a transcript whose byte estimate is roughly n tokens, for
// driving shouldCompact without a provider.
func msgsOfTokens(n int) []Message {
	return []Message{{Role: RoleUser, Content: strings.Repeat("x", n*4)}}
}
