package agentruntime

// Real-provider tests for the advisor — the half that no faux provider can
// answer.
//
// The unit tests prove the plumbing: a note re-opens the run, a repeat is
// dropped, an error accepts the finish. What they cannot prove is the only
// question that decides whether this feature is worth having switched on:
// does a real model, shown a real run, say the right thing — and, far more
// importantly, does it stay QUIET when there is nothing to say?
//
// That second case is the non-happy path here, and it is the one that fails in
// the wild. A reviewer's default failure is not missing a bug; it is finding
// something to say on every run until the operator turns it off. So there are
// two live cases below: a run with a defect the transcript itself contradicts,
// and a clean run that must draw silence.
//
// Gated on the same env vars as the other real-provider tests; skips without
// them.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/advisor"
	"github.com/lohi-ai/agentray/ai"
)

// liveAdvisorReview runs one review through the exact prompt/parse path
// advisorReviewer() uses, and returns the notes it produced.
func liveAdvisorReview(t *testing.T, instructions string, rev advisor.Review) []advisor.Note {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("set AGENTRAY_TEST_OPENAI_BASE_URL, AGENTRAY_TEST_OPENAI_API_KEY, AGENTRAY_TEST_OPENAI_MODEL to run real-provider tests")
	}
	provider := ai.NewOpenAIProvider(key, base, ai.DefaultCompat())

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := provider.Chat(ctx, agentcore.ChatRequest{
		Model:     model,
		MaxTokens: advisorMaxTokens,
		Messages: []agentcore.Message{
			{Role: agentcore.RoleSystem, Content: advisorSystemPrompt(instructions)},
			{Role: agentcore.RoleUser, Content: advisorUserPrompt(rev)},
		},
	})
	if err != nil {
		t.Fatalf("advisor Chat: %v", err)
	}
	t.Logf("raw review: %s", resp.Message.Content)

	var out advisorNotes
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
		t.Fatalf("review did not parse through the shipped path: %v\nraw: %s", err, resp.Message.Content)
	}
	notes := make([]advisor.Note, 0, len(out.Notes))
	for _, n := range out.Notes {
		if strings.TrimSpace(n.Text) == "" {
			continue
		}
		notes = append(notes, advisor.Note{Text: n.Text, Severity: advisorSeverity(n.Severity)})
	}
	return notes
}

// TestReal_Advisor_CatchesAnAnswerTheTranscriptContradicts is the happy path:
// the agent ran a query that returned one number and reported a different one.
// This is the class the advisor exists for — the evidence guard cannot see it,
// because a read tool WAS executed.
func TestReal_Advisor_CatchesAnAnswerTheTranscriptContradicts(t *testing.T) {
	notes := liveAdvisorReview(t, "", advisor.Review{
		Turns: 3,
		Final: "Doanh thu tháng 7 là 4.2 tỷ VND, tăng 18% so với tháng 6.",
		Tools: []agentcore.ToolTrace{{Tool: "run_sql", Allowed: true}},
		Messages: []agentcore.Message{
			{Role: agentcore.RoleUser, Directive: true, Content: "Doanh thu tháng 7 là bao nhiêu?"},
			{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{
				Name:      "run_sql",
				Arguments: `{"sql":"SELECT sum(amount) AS revenue FROM orders WHERE month = '2026-07'"}`,
			}}},
			{Role: agentcore.RoleTool, Name: "run_sql", Content: `[{"revenue": 3120000000}]`},
		},
	})

	if len(notes) == 0 {
		t.Fatal("the reviewer stayed silent on an answer its own transcript contradicts (3.12B reported as 4.2B)")
	}
	// It must interrupt, not merely mention it: an answer with the wrong number
	// in it is not a nit.
	interrupting := false
	for _, n := range notes {
		if n.Severity.Interrupting() {
			interrupting = true
		}
		t.Logf("note [%s]: %s", n.Severity, n.Text)
	}
	if !interrupting {
		t.Errorf("a contradicted figure drew only nits, so the answer would have shipped: %+v", notes)
	}
	// And it must be about the number, not generic unease.
	joined := strings.ToLower(strings.Join(noteTexts(notes), " "))
	if !strings.Contains(joined, "3.12") && !strings.Contains(joined, "3,12") &&
		!strings.Contains(joined, "3120000000") && !strings.Contains(joined, "4.2") {
		t.Errorf("the note never names the figure it is about — that is generic unease, not advice: %+v", notes)
	}
}

// TestReal_Advisor_StaysSilentOnACleanRun is the non-happy path, and the one
// that decides whether an operator leaves the feature on.
//
// The run below is fine: the question was answered from a query that returned
// exactly that number. There is nothing to say. A reviewer that manufactures a
// concern here — "consider adding error handling", "confirm the date range with
// the user" — makes every run cost an extra turn for nothing, which is the
// documented way this feature dies.
func TestReal_Advisor_StaysSilentOnACleanRun(t *testing.T) {
	notes := liveAdvisorReview(t, "", advisor.Review{
		Turns: 2,
		Final: "Tháng 7 có 12,480 người dùng hoạt động, tăng 6% so với tháng 6 (11,774).",
		Tools: []agentcore.ToolTrace{{Tool: "run_sql", Allowed: true}},
		Messages: []agentcore.Message{
			{Role: agentcore.RoleUser, Directive: true, Content: "Tháng 7 có bao nhiêu người dùng hoạt động, so với tháng 6?"},
			{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{
				Name:      "run_sql",
				Arguments: `{"sql":"SELECT date_trunc('month', ts) AS m, count(distinct user_id) AS mau FROM events WHERE ts >= '2026-06-01' AND ts < '2026-08-01' GROUP BY 1 ORDER BY 1"}`,
			}}},
			{Role: agentcore.RoleTool, Name: "run_sql", Content: `[{"m":"2026-06-01","mau":11774},{"m":"2026-07-01","mau":12480}]`},
		},
	})

	// Any note here would re-open the run for nothing. A nit is tolerated (it
	// ships the answer anyway); an interrupting note on a clean run is the
	// failure.
	for _, n := range notes {
		t.Logf("note [%s]: %s", n.Severity, n.Text)
		if n.Severity.Interrupting() {
			t.Errorf("the reviewer re-opened a clean run — this is the failure mode that gets the feature switched off: [%s] %s", n.Severity, n.Text)
		}
	}
	if len(notes) == 0 {
		t.Log("silent on a clean run, which is the correct answer")
	}
}

// TestReal_Advisor_HonorsTheOperatorsReviewPriorities proves the WATCHDOG.md
// analog actually steers the reviewer: the same clean-looking run draws a note
// only because the operator said this project cares about it.
func TestReal_Advisor_HonorsTheOperatorsReviewPriorities(t *testing.T) {
	rev := advisor.Review{
		Turns: 2,
		Final: "Tổng doanh thu là 8,450,000.",
		Tools: []agentcore.ToolTrace{{Tool: "run_sql", Allowed: true}},
		Messages: []agentcore.Message{
			{Role: agentcore.RoleUser, Directive: true, Content: "Tổng doanh thu tháng này?"},
			{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{
				Name:      "run_sql",
				Arguments: `{"sql":"SELECT sum(amount) FROM orders WHERE month = '2026-08'"}`,
			}}},
			{Role: agentcore.RoleTool, Name: "run_sql", Content: `[{"sum": 8450000}]`},
		},
	}
	notes := liveAdvisorReview(t,
		"This project bills in several currencies. Any revenue total that is not grouped by currency is wrong — say so.",
		rev)

	if len(notes) == 0 {
		t.Fatal("the operator's review priorities did not reach the reviewer's judgement")
	}
	joined := strings.ToLower(strings.Join(noteTexts(notes), " "))
	if !strings.Contains(joined, "currenc") && !strings.Contains(joined, "tiền tệ") {
		t.Errorf("the note is not about what the operator asked to watch for: %+v", notes)
	}
	for _, n := range notes {
		t.Logf("note [%s]: %s", n.Severity, n.Text)
	}
}

func noteTexts(notes []advisor.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.Text)
	}
	return out
}
