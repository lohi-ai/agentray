package agentruntime

import "testing"

// TestToolTargetRendersScalarArgs verifies the work-log label is the call's
// scalar arguments in a stable order — the only thing that separates two
// concurrent calls to the same tool on screen.
func TestToolTargetRendersScalarArgs(t *testing.T) {
	// Keys are deliberately out of alphabetical order in the JSON: the label must
	// not depend on Go's map iteration order, or the same call renders
	// differently between the tool_start frame and the mirrored trace entry.
	// Ordered by key (breakdown, days, event), not by position in the JSON.
	const want = "breakdown, 30, signup_completed"
	got := ToolTarget(`{"event":"signup_completed","days":30,"breakdown":true,"note":""}`)
	if got != want {
		t.Fatalf("ToolTarget = %q, want %q", got, want)
	}
	// Stable across calls.
	if again := ToolTarget(`{"event":"signup_completed","days":30,"breakdown":true,"note":""}`); again != got {
		t.Fatalf("ToolTarget is not stable: %q then %q", got, again)
	}
}

// TestToolTargetSkipsNoise verifies empty strings, false flags, and nested
// objects contribute nothing — the label is a glance, not a dump.
func TestToolTargetSkipsNoise(t *testing.T) {
	if got := ToolTarget(`{"a":"","b":false,"c":{"deep":"value"},"d":[1,2],"e":null}`); got != "" {
		t.Fatalf("ToolTarget = %q, want empty", got)
	}
}

// TestToolTargetInvalidJSONIsEmpty verifies a non-object argument string yields
// no label rather than leaking the raw text into the row.
func TestToolTargetInvalidJSONIsEmpty(t *testing.T) {
	for _, args := range []string{"", "not json", `"a string"`, `[1,2,3]`} {
		if got := ToolTarget(args); got != "" {
			t.Fatalf("ToolTarget(%q) = %q, want empty", args, got)
		}
	}
}

// TestToolTargetIsCapped verifies a large argument blob never rides the wire in
// full.
func TestToolTargetIsCapped(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	got := ToolTarget(`{"q":"` + string(long) + `"}`)
	if len([]rune(got)) != toolTargetMax+1 { // +1 for the ellipsis
		t.Fatalf("ToolTarget length = %d, want %d", len([]rune(got)), toolTargetMax+1)
	}
	if got[len(got)-3:] != "…" {
		t.Fatalf("ToolTarget = %q, want a trailing ellipsis", got)
	}
}
