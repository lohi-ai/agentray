package agentcore

import "strings"

// A long run's table of contents.
//
// A run of a few dozen steps can be read by scrolling. A run of several
// thousand cannot, and paging it only turns "too much to read" into "too much
// to read, twelve at a time" — the reader still has no idea where in the task
// they are. What is missing is not a smaller page but a STRUCTURE, and the run
// already produced one without being asked to.
//
// Compaction is that structure. Every time the window fills, the loop asks the
// model to summarize everything that happened since the last time it asked, and
// keeps the answer. A run that compacted 353 times therefore contains 353
// model-written accounts of "what I had done by this point", in order, each
// bounding a span of turns. That is a chapter list, and it costs nothing to
// produce because it has already been paid for.
//
// So the Lab's navigation is: chapters (always whole — they are small, and they
// are how you find the part you want) plus one page of steps (large, and only
// useful once you know which part you want). The first chapter of a run that
// never compacted is the whole run, which is exactly right for the short runs
// where none of this matters.

// RunChapter is one span of a run bounded by compaction: the work between one
// context rewrite and the next, and the model's own account of it.
type RunChapter struct {
	// Index is the chapter's 0-based position.
	Index int `json:"index"`
	// FirstStep and LastStep are inclusive indices into the step list, so a
	// client turns a chapter into a page request without a second lookup.
	FirstStep int `json:"first_step"`
	LastStep  int `json:"last_step"`
	// FirstTurn and LastTurn are the run turns the chapter spans.
	FirstTurn int `json:"first_turn"`
	LastTurn  int `json:"last_turn"`
	// Title is a one-line label drawn from the closing summary — the model's own
	// words, not a generated paraphrase.
	Title string `json:"title"`
	// Summary is the compaction summary that CLOSED this chapter: what the agent
	// had accomplished by the end of the span. The final chapter has none (the
	// run ended before the next compaction), which is the honest answer — its
	// outcome is the run's final answer, not a summary.
	Summary string `json:"summary,omitempty"`
	// Steps is how many steps the chapter contains, and ToolCalls how many tool
	// calls were made in it — the two numbers that say whether a chapter is
	// worth opening.
	Steps     int `json:"steps"`
	ToolCalls int `json:"tool_calls"`
	// TokensIn/TokensOut/CostUSD are the chapter's own spend, differenced from
	// the cumulative totals the fold already computed.
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

// RunChapters divides a folded step list into chapters at its compaction steps.
//
// A compaction step CLOSES the chapter it appears in rather than opening the
// next one, because its summary describes the span behind it. The result is
// always at least one chapter for a non-empty run.
func RunChapters(steps []LabStep) []RunChapter {
	if len(steps) == 0 {
		return []RunChapter{}
	}
	out := []RunChapter{}
	start := 0
	var baseIn, baseOut int
	var baseCost float64

	closeChapter := func(end int, summary string) {
		span := steps[start : end+1]
		last := span[len(span)-1]
		calls := 0
		for _, s := range span {
			calls += len(s.ToolCalls)
		}
		out = append(out, RunChapter{
			Index:     len(out),
			FirstStep: start,
			LastStep:  end,
			FirstTurn: span[0].Turn,
			LastTurn:  last.Turn,
			Title:     chapterTitle(summary, len(out)),
			Summary:   summary,
			Steps:     len(span),
			ToolCalls: calls,
			TokensIn:  last.CumTokensIn - baseIn,
			TokensOut: last.CumTokensOut - baseOut,
			CostUSD:   last.CumCostUSD - baseCost,
		})
		baseIn, baseOut, baseCost = last.CumTokensIn, last.CumTokensOut, last.CumCostUSD
		start = end + 1
	}

	for i, s := range steps {
		if s.Kind == LabStepCompaction {
			closeChapter(i, s.Summary)
		}
	}
	// Whatever follows the last compaction is the run's final, unsummarized
	// chapter. A run that never compacted has only this one.
	if start < len(steps) {
		closeChapter(len(steps)-1, "")
	}
	return out
}

// chapterTitle draws a one-line label out of a compaction summary.
//
// The summaries follow the checkpoint format the loop asks for ("## Goal",
// "## Progress", …), so the first line under a heading is a real sentence about
// the work rather than a heading itself. A summary that does not follow the
// format falls back to its first non-empty line, and a chapter with no summary
// (the run's last) is labelled by position.
func chapterTitle(summary string, index int) string {
	for _, line := range strings.Split(summary, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimLeft(line, "-*[]x ")
		if line = strings.TrimSpace(line); line != "" {
			return TruncateBytes(line, 120)
		}
	}
	if index == 0 {
		return "The run"
	}
	return "Final stretch"
}
