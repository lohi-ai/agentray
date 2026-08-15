package agentcore

import (
	"fmt"
	"strings"
	"testing"
)

// stepsWithCompactionsAt builds a folded step list of `turns` turns with a
// compaction step closing the span at each of the given turns, and cumulative
// accounting that advances one unit per step — enough to check that a chapter's
// own spend is differenced, not copied.
func stepsWithCompactionsAt(turns int, compactAt ...int) []LabStep {
	mark := map[int]bool{}
	for _, t := range compactAt {
		mark[t] = true
	}
	var steps []LabStep
	cumIn, cumOut, cumCost := 0, 0, 0.0
	add := func(turn int, kind LabStepKind, summary string, calls int) {
		cumIn += 10
		cumOut += 2
		cumCost += 0.5
		tc := make([]LabToolCall, calls)
		steps = append(steps, LabStep{
			Index: len(steps), Turn: turn, Kind: kind, Summary: summary,
			ToolCalls: tc, CumTokensIn: cumIn, CumTokensOut: cumOut, CumCostUSD: cumCost,
		})
	}
	for turn := 1; turn <= turns; turn++ {
		if mark[turn] {
			add(turn, LabStepCompaction,
				fmt.Sprintf("## Goal\nFinish the audit\n## Progress\n### Done\n- [x] shards through turn %d reconciled", turn), 0)
		}
		add(turn, LabStepTurn, "", 1)
	}
	return steps
}

// TestRunChaptersDivideAtCompactions is the core claim: a long run comes back
// as a handful of navigable spans instead of thousands of undifferentiated
// steps, and each span says which steps it covers so a client can turn a
// chapter into a page request without a second lookup.
func TestRunChaptersDivideAtCompactions(t *testing.T) {
	steps := stepsWithCompactionsAt(20, 6, 13)
	chapters := RunChapters(steps)

	if len(chapters) != 3 {
		t.Fatalf("two compactions must yield three chapters, got %d", len(chapters))
	}
	// The spans must tile the step list exactly: no step in two chapters, none
	// in none. A reader who opened every chapter must have seen the whole run.
	if chapters[0].FirstStep != 0 {
		t.Fatalf("first chapter starts at step %d", chapters[0].FirstStep)
	}
	for i := 1; i < len(chapters); i++ {
		if chapters[i].FirstStep != chapters[i-1].LastStep+1 {
			t.Fatalf("chapters %d and %d do not tile: %d then %d",
				i-1, i, chapters[i-1].LastStep, chapters[i].FirstStep)
		}
	}
	if last := chapters[len(chapters)-1].LastStep; last != len(steps)-1 {
		t.Fatalf("chapters end at step %d, run has %d steps", last, len(steps))
	}
	total := 0
	for _, c := range chapters {
		total += c.Steps
	}
	if total != len(steps) {
		t.Fatalf("chapters cover %d steps, run has %d", total, len(steps))
	}
}

// TestRunChaptersCloseOnTheirOwnSummary pins the direction. A compaction
// summarizes what came BEFORE it, so it has to close its chapter rather than
// open the next one — a list that labelled each span with the summary written
// at its start would describe every chapter by the work preceding it.
func TestRunChaptersCloseOnTheirOwnSummary(t *testing.T) {
	steps := stepsWithCompactionsAt(20, 6, 13)
	chapters := RunChapters(steps)

	if got := chapters[0].Summary; got == "" {
		t.Fatal("the first chapter must carry the summary that closed it")
	}
	for i, want := range []int{6, 13} {
		if !strings.Contains(chapters[i].Summary, fmt.Sprintf("turn %d", want)) {
			t.Fatalf("chapter %d closed with the wrong summary: %q", i, chapters[i].Summary)
		}
		// The compaction step is the last step OF the chapter it summarizes, not
		// the first step of the one after. Getting this off by one puts each
		// chapter's closing account one step outside the span it describes.
		if last := steps[chapters[i].LastStep]; last.Kind != LabStepCompaction {
			t.Fatalf("chapter %d ends on a %v step; a summarized chapter ends on its compaction",
				i, last.Kind)
		}
		if last := steps[chapters[i].LastStep]; last.Summary != chapters[i].Summary {
			t.Fatalf("chapter %d carries a summary from step %d, not from the step that closed it",
				i, chapters[i].LastStep)
		}
	}
	// The last chapter is the stretch after the final compaction. It has no
	// summary because none was ever written for it, and claiming otherwise
	// would attribute an earlier chapter's account to later work.
	if s := chapters[2].Summary; s != "" {
		t.Fatalf("the final chapter must not borrow a summary: %q", s)
	}
}

// TestRunChaptersAccountPerChapter checks the numbers are differenced out of
// the cumulative totals rather than reported as running totals — otherwise
// every chapter would appear to cost more than the last no matter what
// happened in it.
func TestRunChaptersAccountPerChapter(t *testing.T) {
	steps := stepsWithCompactionsAt(20, 6, 13)
	chapters := RunChapters(steps)

	sumIn, sumCalls := 0, 0
	for _, c := range chapters {
		if c.TokensIn <= 0 {
			t.Fatalf("chapter %d reports %d input tokens", c.Index, c.TokensIn)
		}
		sumIn += c.TokensIn
		sumCalls += c.ToolCalls
	}
	last := steps[len(steps)-1]
	if sumIn != last.CumTokensIn {
		t.Fatalf("chapter tokens sum to %d, run total is %d", sumIn, last.CumTokensIn)
	}
	if sumCalls != 20 {
		t.Fatalf("chapter tool calls sum to %d, run made 20", sumCalls)
	}
}

// TestRunChaptersOnARunThatNeverCompacted covers the short-run case, which is
// most runs: one chapter spanning everything, so a client renders the same way
// without special-casing.
func TestRunChaptersOnARunThatNeverCompacted(t *testing.T) {
	chapters := RunChapters(stepsWithCompactionsAt(5))
	if len(chapters) != 1 {
		t.Fatalf("want one chapter, got %d", len(chapters))
	}
	if chapters[0].FirstStep != 0 || chapters[0].LastStep != 4 || chapters[0].Steps != 5 {
		t.Fatalf("the single chapter must span the run: %+v", chapters[0])
	}
	if chapters[0].Title == "" {
		t.Fatal("even an unsummarized chapter needs a label")
	}
}

func TestRunChaptersOnAnEmptyRun(t *testing.T) {
	if got := RunChapters(nil); len(got) != 0 {
		t.Fatalf("an empty run has no chapters, got %d", len(got))
	}
}

// TestChapterTitleReadsTheSummary checks the label comes from the model's own
// words. The checkpoint format is mostly headings and list markers, so a naive
// "first line" would title every chapter "## Goal".
func TestChapterTitleReadsTheSummary(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		want    string
	}{
		{"skips headings", "## Goal\nReconcile the ledger\n## Progress", "Reconcile the ledger"},
		{"strips list markers", "## Progress\n### Done\n- [x] shards 1-40 audited", "shards 1-40 audited"},
		{"falls back to the first real line", "just a plain summary", "just a plain summary"},
		{"labels an unsummarized chapter", "", "The run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chapterTitle(tc.summary, 0); got != tc.want {
				t.Fatalf("title = %q, want %q", got, tc.want)
			}
		})
	}
}
