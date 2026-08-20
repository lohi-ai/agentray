package agentcore

import (
	"strings"
	"testing"
)

// TestSerializeToolArgsFatValueDoesNotStarveSiblings is the defect this helper
// closes: with only the per-call cap, truncateMiddle kept the head and tail of
// the raw blob, so an argument sitting BETWEEN two fat values — its name
// included — never reached the summarizer, which was then asked to describe a
// call it could not read. Per-value capping first keeps every key.
func TestSerializeToolArgsFatValueDoesNotStarveSiblings(t *testing.T) {
	raw := `{"content":"` + bigText(40_000) + `","mode":"overwrite","patch":"` + bigText(40_000) + `"}`

	out := serializeToolArgs(raw)

	// The sandwiched argument is the one the old whole-blob truncation lost.
	if !strings.Contains(out, "mode") || !strings.Contains(out, "overwrite") {
		t.Fatalf("sandwiched argument was starved out of the serialization: %q", out)
	}
	for _, key := range []string{"content", "mode", "patch"} {
		if !strings.Contains(out, key+"=") {
			t.Fatalf("argument %q missing from the serialization: %q", key, out)
		}
	}
}

// TestSerializeToolArgsRespectsPerCallCap pins that the outer bound still
// holds: many fat values must not add up past maxSerializedToolArgs, which is
// what keeps the summarization request itself small.
func TestSerializeToolArgsRespectsPerCallCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{`)
	for i, key := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + key + `":"` + bigText(5_000) + `"`)
	}
	b.WriteString(`}`)

	out := serializeToolArgs(b.String())

	if len(out) > maxSerializedToolArgs {
		t.Fatalf("serialized args exceeded the per-call cap: %d bytes", len(out))
	}
}

// TestSerializeToolArgsPerValueCap pins the inner bound independently of the
// outer one: a single fat value is cut to maxSerializedToolArgValue, not left
// to spend the whole call budget.
func TestSerializeToolArgsPerValueCap(t *testing.T) {
	out := serializeToolArgs(`{"q":"` + bigText(50_000) + `"}`)

	if len(out) > maxSerializedToolArgValue+64 {
		t.Fatalf("single value was not capped per-value: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected a truncation marker: %q", out)
	}
}

// TestSerializeToolArgsDeterministicKeyOrder guards a cache property, not a
// style preference: the summarization request rides the provider's prefix
// cache, and Go's randomized map iteration would churn it on every compaction.
func TestSerializeToolArgsDeterministicKeyOrder(t *testing.T) {
	raw := `{"zeta":1,"alpha":2,"mu":3,"beta":4}`

	first := serializeToolArgs(raw)
	if want := `alpha=2, beta=4, mu=3, zeta=1`; first != want {
		t.Fatalf("keys not rendered in sorted order:\n got %q\nwant %q", first, want)
	}
	for i := 0; i < 50; i++ {
		if got := serializeToolArgs(raw); got != first {
			t.Fatalf("rendering not deterministic on run %d: %q != %q", i, got, first)
		}
	}
}

// TestSerializeToolArgsNonObjectFallsThrough keeps the documented escape hatch:
// arguments that are not a JSON object behave byte-for-byte as before the
// per-value cap existed.
func TestSerializeToolArgsNonObjectFallsThrough(t *testing.T) {
	cases := []string{
		`"just a string"`,
		`[1,2,3]`,
		`42`,
		`null`,
		``,
		`{"unterminated": `,
		`not json at all`,
	}
	for _, raw := range cases {
		if got, want := serializeToolArgs(raw), truncateMiddle(raw, maxSerializedToolArgs); got != want {
			t.Fatalf("non-object %q: got %q, want the old whole-blob truncation %q", raw, got, want)
		}
	}
}

// TestSerializeToolArgsMalformedOversizedFallsThrough covers the fallback on a
// blob big enough for the old path to actually truncate, so the fall-through is
// pinned against truncateMiddle's output and not just against identity.
func TestSerializeToolArgsMalformedOversizedFallsThrough(t *testing.T) {
	raw := `{"path":"x.ts","content":"` + bigText(40_000) // never closed

	got := serializeToolArgs(raw)

	if want := truncateMiddle(raw, maxSerializedToolArgs); got != want {
		t.Fatalf("malformed JSON did not fall through unchanged:\n got %q\nwant %q", got, want)
	}
	if len(got) > maxSerializedToolArgs {
		t.Fatalf("fallback exceeded the per-call cap: %d bytes", len(got))
	}
}
