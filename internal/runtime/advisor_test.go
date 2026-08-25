package agentruntime

// What the advisor is allowed to READ, and what it is allowed to SAY.
//
// The reviewer is one model call over a whole run, which puts it on the same
// cliff reflect() fell off: input that grows with how long the agent worked
// breaks on precisely the runs worth reviewing. These tests hold the window
// bounded, hold the two things a reviewer cannot do without (what was asked,
// what was most recently done) inside it, and hold the severity mapping to the
// side that cannot buy a turn it did not earn.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/advisor"
)

// longConversation builds a run of the shape a long task produces: the task at
// the top, then hundreds of query/result pairs.
func longConversation(steps int) []agentcore.Message {
	msgs := []agentcore.Message{
		{Role: agentcore.RoleSystem, Content: strings.Repeat("persona and skills. ", 500)},
		{Role: agentcore.RoleUser, Content: "audit every region's open ledger and report the total", Directive: true},
	}
	for i := range steps {
		msgs = append(msgs,
			agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{
				Name:      "run_sql",
				Arguments: fmt.Sprintf(`{"sql":"SELECT sum(amount) FROM ledger WHERE shard = %d"}`, i),
			}}},
			agentcore.Message{Role: agentcore.RoleTool, Name: "run_sql", Content: strings.Repeat("row data ", 200)},
		)
	}
	msgs = append(msgs, agentcore.Message{Role: agentcore.RoleAssistant, Content: "the total is 4.2M"})
	return msgs
}

func TestTranscriptStaysBoundedHoweverLongTheRunWas(t *testing.T) {
	for _, steps := range []int{1, 50, 2000} {
		got := renderAdvisorTranscript(longConversation(steps), advisorTranscriptBudget)
		// The elision marker and the last block can each overshoot slightly;
		// what must not happen is growth with the length of the run.
		if len(got) > advisorTranscriptBudget*2 {
			t.Errorf("%d steps rendered %d bytes, past any sane bound on a %d budget",
				steps, len(got), advisorTranscriptBudget)
		}
	}
}

func TestBoundedTranscriptKeepsTheTaskAndTheRecentWork(t *testing.T) {
	// Both halves are load-bearing and for different reasons: drift is only
	// visible against what was ASKED, and a wrong turn is where the work most
	// recently WENT. A window that keeps only the tail judges the answer
	// against a task it is guessing at.
	msgs := longConversation(2000)
	got := renderAdvisorTranscript(msgs, advisorTranscriptBudget)
	if !strings.Contains(got, "audit every region's open ledger") {
		t.Error("the task fell out of the window — the reviewer cannot see drift without it")
	}
	if !strings.Contains(got, "the total is 4.2M") {
		t.Error("the most recent work fell out of the window")
	}
	if !strings.Contains(got, "steps elided") {
		t.Error("elision must be marked, or the reviewer concludes an unseen step never happened")
	}
}

func TestShortTranscriptIsNotElided(t *testing.T) {
	got := renderAdvisorTranscript(longConversation(2), advisorTranscriptBudget)
	if strings.Contains(got, "steps elided") {
		t.Errorf("a run well under budget was elided:\n%s", got)
	}
}

func TestSystemPromptIsNotShownToTheReviewer(t *testing.T) {
	// The agent's persona and skills are not under review, and they are the
	// largest fixed block in the conversation.
	got := renderAdvisorTranscript(longConversation(1), advisorTranscriptBudget)
	if strings.Contains(got, "persona and skills") {
		t.Error("the agent's own system prompt was rendered into the review")
	}
}

func TestInjectedUserMessagesAreLabelledAsNotFromTheHuman(t *testing.T) {
	// A goal nudge, a budget wrap-up, or a previous advisory is a user-role
	// message the framework wrote. A reviewer that reads one as the human's ask
	// will hold the agent to a requirement nobody made.
	got := renderAdvisorTranscript([]agentcore.Message{
		{Role: agentcore.RoleUser, Content: "the real ask", Directive: true},
		{Role: agentcore.RoleUser, Content: "[System: verify before finishing]"},
	}, advisorTranscriptBudget)
	if !strings.Contains(got, "### user\nthe real ask") {
		t.Errorf("the human's ask was not labelled as theirs:\n%s", got)
	}
	if !strings.Contains(got, "### system-injected") {
		t.Errorf("a framework-written user message was not distinguished:\n%s", got)
	}
}

func TestToolIntentSurvivesIntoTheReview(t *testing.T) {
	// WHICH query ran is most of what a reviewer has to work with. "some tool
	// was called" cannot support a note.
	got := renderAdvisorTranscript([]agentcore.Message{
		{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{
			Name: "run_sql", Arguments: `{"sql":"SELECT sum(amount) FROM orders WHERE refunded = false"}`,
		}}},
	}, advisorTranscriptBudget)
	if !strings.Contains(got, "refunded = false") {
		t.Errorf("the tool arguments did not reach the reviewer:\n%s", got)
	}
}

func TestSeverityDefaultsToTheOneThatCannotBuyATurn(t *testing.T) {
	// "concern?" is not "concern": a near-miss falls to nit rather than being
	// generously read as the level it almost spelled.
	for _, in := range []string{"", "critical", "CONCERN?", "warning", "nit"} {
		if got := advisorSeverity(in); got != advisor.SeverityNit {
			t.Errorf("advisorSeverity(%q) = %q, want nit — an unrecognized level must not re-open the run", in, got)
		}
	}
	for in, want := range map[string]advisor.Severity{
		"concern":   advisor.SeverityConcern,
		"CONCERN":   advisor.SeverityConcern,
		" blocker ": advisor.SeverityBlocker,
	} {
		if got := advisorSeverity(in); got != want {
			t.Errorf("advisorSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOperatorInstructionsReachTheReviewerOnly(t *testing.T) {
	p := advisorSystemPrompt("distrust any revenue figure not grouped by currency")
	if !strings.Contains(p, "distrust any revenue figure") {
		t.Fatal("the operator's review priorities never reached the reviewer's prompt")
	}
	if !strings.Contains(p, "<attention>") {
		t.Error("operator prose must be fenced, not spliced into the rule block")
	}
	// And an empty setting adds nothing at all.
	if strings.Contains(advisorSystemPrompt("   "), "<attention>") {
		t.Error("a blank instructions field still added a block")
	}
}

func TestSecondRoundTellsTheReviewerItIsCheckingResolution(t *testing.T) {
	prompt := advisorUserPrompt(advisor.Review{
		Round: 1,
		Final: "the corrected total is 3.1M",
		Delivered: []advisor.Note{
			{Text: "the revenue figure sums a filtered and an unfiltered query", Severity: advisor.SeverityConcern},
		},
		Messages: []agentcore.Message{{Role: agentcore.RoleUser, Content: "the ask", Directive: true}},
	})
	if !strings.Contains(prompt, "review round 2") {
		t.Error("the reviewer was not told which round this is")
	}
	if !strings.Contains(prompt, "Already raised") || !strings.Contains(prompt, "filtered and an unfiltered query") {
		t.Errorf("the reviewer cannot check resolution without its own prior notes:\n%s", prompt)
	}
	if !strings.Contains(prompt, "the corrected total is 3.1M") {
		t.Error("the answer under review is missing")
	}
	// A first round must NOT carry the resolution framing.
	first := advisorUserPrompt(advisor.Review{Final: "x", Messages: []agentcore.Message{{Role: agentcore.RoleUser, Content: "ask"}}})
	if strings.Contains(first, "review round") {
		t.Error("round-one prompt carried second-round framing")
	}
}

func TestBlockedAndErroredToolsAreVisibleToTheReviewer(t *testing.T) {
	prompt := advisorUserPrompt(advisor.Review{
		Final: "done",
		Tools: []agentcore.ToolTrace{
			{Tool: "run_sql", Allowed: true},
			{Tool: "delete_rows", Allowed: false, Reason: "scope not granted"},
			{Tool: "web_fetch", Allowed: true, Error: "502 from origin"},
		},
	})
	for _, want := range []string{"delete_rows: BLOCKED (scope not granted)", "web_fetch: ERROR 502 from origin"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestEmptyTranscriptDoesNotRenderAsSilence(t *testing.T) {
	// A reviewer handed an empty string would review the answer alone and have
	// no way to know it was missing the run.
	if got := renderAdvisorTranscript(nil, advisorTranscriptBudget); !strings.Contains(got, "no transcript") {
		t.Errorf("empty transcript rendered as %q", got)
	}
}
