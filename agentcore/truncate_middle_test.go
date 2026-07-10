package agentcore

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateMiddleKeepsHeadAndTail(t *testing.T) {
	s := "HEAD-" + strings.Repeat("x", 4096) + "-TAIL"
	got := truncateMiddle(s, 512)
	if len(got) > 512 {
		t.Fatalf("result exceeds budget: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "HEAD-") {
		t.Fatalf("head lost: %q", got[:16])
	}
	if !strings.HasSuffix(got, "-TAIL") {
		t.Fatalf("tail lost: %q", got[len(got)-16:])
	}
	if !strings.Contains(got, "bytes truncated") {
		t.Fatalf("missing omission marker: %q", got)
	}
}

func TestTruncateMiddleNoopWithinBudget(t *testing.T) {
	s := "short result"
	if got := truncateMiddle(s, 1024); got != s {
		t.Fatalf("modified in-budget string: %q", got)
	}
	if got := truncateMiddle(s, 0); got != s {
		t.Fatalf("maxBytes 0 must disable truncation: %q", got)
	}
}

func TestTruncateMiddleUTF8Safe(t *testing.T) {
	s := strings.Repeat("héllo wörld ", 400)
	for budget := 100; budget <= 400; budget += 37 {
		got := truncateMiddle(s, budget)
		if !utf8.ValidString(got) {
			t.Fatalf("budget %d produced invalid UTF-8", budget)
		}
		if len(got) > budget {
			t.Fatalf("budget %d exceeded: %d bytes", budget, len(got))
		}
	}
}

// TestReasoningEffortThreadedIntoRequests proves the per-agent knob reaches
// every provider call of a run.
func TestReasoningEffortThreadedIntoRequests(t *testing.T) {
	provider := NewFauxProvider(AssistantText("ok"))
	agent, err := New(Config{
		Provider:        provider,
		Model:           "faux-1",
		Tools:           NewToolSet(),
		Policy:          NewAllowList(),
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(t.Context(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.Recorded) == 0 || provider.Recorded[0].ReasoningEffort != "high" {
		t.Fatalf("reasoning effort not threaded: %+v", provider.Recorded)
	}
}
