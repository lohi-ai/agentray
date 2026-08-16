package agentcore

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The checkpoint ceiling, and what it is allowed to destroy.
//
// A checkpoint that overshoots its budget has to lose something. The question is
// what, and the answer has to be "whole facts", because the alternative is not a
// smaller checkpoint — it is a wrong one. This document is handed to the NEXT
// fold as the previous summary with instructions to carry it forward, so
// whatever survives the cut is what the run believes for the rest of its life.

// shardCheckpoint is a realistic over-budget checkpoint: a Done list of
// identifier-bearing facts between a Goal and a Next Steps section.
func shardCheckpoint(n int) string {
	var b strings.Builder
	b.WriteString("## Goal\nAudit every shard and report each shard's code.\n## Progress\n### Done\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "- [x] shard %d inspected, code FINDING-%d=SHARD%03dOK\n", i, i, i)
	}
	b.WriteString("## Next Steps\n1. Inspect the remaining shards\n2. Report every code\n")
	return b.String()
}

var shardCodeRe = regexp.MustCompile(`FINDING-(\d+)=(\S*)`)

// The regression. A byte-exact cut through the Done list produced
// "code FINDING-4=SHARD00" for a code that ends 002OK — a fact that is wrong
// while reading as complete, which the next fold then carries forward as truth.
func TestClampedCheckpointNeverCutsAFactInHalf(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)

	for _, m := range shardCodeRe.FindAllStringSubmatch(got, -1) {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
			t.Fatalf("unparseable finding number %q", m[1])
		}
		want := fmt.Sprintf("SHARD%03dOK", n)
		if m[2] != want {
			t.Fatalf("finding %d survived the clamp as %q, want %q. A truncated identifier is "+
				"worse than a dropped one: this checkpoint is handed to the next fold as the "+
				"previous summary, so the run carries the mutilated value forward and reports "+
				"it as a finding, with nothing anywhere marking it damaged.\n---\n%s",
				n, m[2], want, got)
		}
	}
}

// What is lost has to be countable, because the next fold reads this document
// and a stated count is something it can act on ("20 items were dropped");
// a bare ellipsis is not.
func TestClampedCheckpointSaysHowMuchItDropped(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)
	if !regexp.MustCompile(`\[\d+ earlier lines dropped`).MatchString(got) {
		t.Fatalf("the clamp dropped content without saying how much:\n---\n%s", got)
	}
}

// The middle goes, not the end: Next Steps and Critical Context sit last in the
// format and are the sections a resuming agent actually reads.
func TestClampedCheckpointKeepsBothEnds(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)
	if !strings.HasPrefix(got, "## Goal") {
		t.Fatalf("the goal did not survive the clamp:\n---\n%s", got)
	}
	if !strings.Contains(got, "## Next Steps") || !strings.Contains(got, "2. Report every code") {
		t.Fatalf("the clamp ate Next Steps, the one section a resuming agent reads:\n---\n%s", got)
	}
}

// Whole-line cutting must not become an excuse to overshoot: an over-budget
// checkpoint is fed back into every window that follows it.
func TestClampedCheckpointRespectsTheCeiling(t *testing.T) {
	for _, tokens := range []int{40, 120, 200, 400} {
		got := clampSummary(shardCheckpoint(60), tokens)
		if max := tokens * bytesPerTokenEstimate; len(got) > max {
			t.Fatalf("clamp at %d tokens returned %d bytes, over the %d-byte ceiling",
				tokens, len(got), max)
		}
	}
}

// A checkpoint that fits is returned untouched — no marker, no reflow.
func TestClampSummaryLeavesAFittingCheckpointAlone(t *testing.T) {
	in := shardCheckpoint(3)
	got := clampSummary(in, 2048)
	if got != strings.TrimSpace(in) {
		t.Fatalf("a checkpoint inside its budget was rewritten:\n---\n%s", got)
	}
}

// Degenerate input still has to be bounded. One enormous line has no structure
// to respect, and an unbounded checkpoint would become the permanent floor of
// every window after it — so a byte cut is correct here, and the point of the
// test is that the ceiling still holds.
func TestClampSummaryBoundsAStructurelessReply(t *testing.T) {
	got := clampSummary(strings.Repeat("word ", 4000), 100)
	if len(got) > 100*bytesPerTokenEstimate {
		t.Fatalf("a single-line reply escaped the ceiling: %d bytes", len(got))
	}
}

// Clamping is applied once per fold, and the fold's own output is clamped again
// next time. Markers must not nest into a document made mostly of markers.
func TestClampingAnAlreadyClampedCheckpointDoesNotNestMarkers(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)
	for i := 0; i < 5; i++ {
		got = clampSummary(got, 120)
	}
	if n := strings.Count(got, "earlier lines dropped"); n > 1 {
		t.Fatalf("%d drop markers accumulated across folds:\n---\n%s", n, got)
	}
}
