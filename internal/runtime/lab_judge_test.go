package agentruntime

import "testing"

// The Lab test verdict falls back to an LLM rubric judge when the EXPECTED field
// holds criteria rather than a literal answer. parseJudgeLine turns the judge's
// one-line reply into a pass/fail + rationale; it must read the leading verdict
// token robustly and error (so RunTest degrades to the diff) on noise.
func TestParseJudgeLine(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantPass bool
		wantErr  bool
		reason   string
	}{
		{"pass dash", "PASS - mentions the novel and a clear CTA", true, false, "mentions the novel and a clear CTA"},
		{"fail colon", "FAIL: no call to action present", false, false, "no call to action present"},
		{"pass emdash", "PASS — covers all three steps", true, false, "covers all three steps"},
		{"verdict token only", "PASS", true, false, ""},
		{"multiline takes first line", "FAIL - too short\nthe reply is one sentence", false, false, "too short"},
		{"empty errors", "   ", false, true, ""},
		{"noise errors", "I think the answer is pretty good overall", false, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pass, reason, err := parseJudgeLine(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got pass=%v reason=%q", c.in, pass, reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.in, err)
			}
			if pass != c.wantPass {
				t.Errorf("pass = %v, want %v", pass, c.wantPass)
			}
			if reason != c.reason {
				t.Errorf("reason = %q, want %q", reason, c.reason)
			}
		})
	}
}
