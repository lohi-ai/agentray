package agentcore

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// namedTool is a minimal Tool used to assert the ToolSet registry contract
// (ordering, lookup, overwrite) without standing up a provider/loop.
type namedTool struct {
	name string
	desc string
}

func (t namedTool) Name() string { return t.name }
func (t namedTool) Schema() ToolSchema {
	return ToolSchema{Name: t.name, Description: t.desc}
}
func (namedTool) Run(context.Context, string) (string, error) { return "", nil }

// TestToolSetPreservesRegistrationOrder verifies Names/Schemas reflect insertion
// order — the order the model is shown its tools in.
func TestToolSetPreservesRegistrationOrder(t *testing.T) {
	ts := NewToolSet(namedTool{name: "a"}, namedTool{name: "b"}, namedTool{name: "c"})
	if got := ts.Names(); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("names = %v, want [a b c]", got)
	}
	schemas := ts.Schemas()
	if len(schemas) != 3 || schemas[0].Name != "a" || schemas[2].Name != "c" {
		t.Fatalf("schemas out of order: %+v", schemas)
	}
}

// TestToolSetAddOverwritesInPlace verifies re-adding a name replaces the tool but
// keeps its original position (Add's overwrite branch), so a per-agent override of
// a same-named default does not reshuffle the catalog the model sees.
func TestToolSetAddOverwritesInPlace(t *testing.T) {
	ts := NewToolSet(namedTool{name: "a"}, namedTool{name: "b"})
	ts.Add(namedTool{name: "a", desc: "v2"})

	if got := ts.Names(); strings.Join(got, ",") != "a,b" {
		t.Fatalf("overwrite changed order/count: %v", got)
	}
	got, ok := ts.Get("a")
	if !ok {
		t.Fatal("Get(a) missing after overwrite")
	}
	if got.Schema().Description != "v2" {
		t.Fatalf("overwrite did not replace the tool: %q", got.Schema().Description)
	}
}

// TestToolSetGetMiss verifies a lookup for an unregistered name fails closed.
func TestToolSetGetMiss(t *testing.T) {
	ts := NewToolSet(namedTool{name: "a"})
	if _, ok := ts.Get("missing"); ok {
		t.Fatal("Get(missing) should report not-found")
	}
}

func TestTruncateBytesShortStringUnchanged(t *testing.T) {
	const s = "hello"
	if got := truncateBytes(s, 1024); got != s {
		t.Fatalf("short string altered: %q", got)
	}
	// maxBytes <= 0 disables truncation entirely.
	long := strings.Repeat("x", 100)
	if got := truncateBytes(long, 0); got != long {
		t.Fatalf("maxBytes=0 should disable truncation, got %d bytes", len(got))
	}
}

func TestTruncateBytesAppendsMarkerWhenCut(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got := truncateBytes(long, 64)
	if len(got) > 64 {
		t.Fatalf("result exceeds maxBytes: %d", len(got))
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
}

// TestTruncateBytesDoesNotSplitRune guards the UTF-8 back-off: cutting in the
// middle of a multibyte rune must never yield invalid UTF-8.
func TestTruncateBytesDoesNotSplitRune(t *testing.T) {
	// "世" is 3 bytes each; a byte budget that lands mid-rune exercises RuneStart.
	s := strings.Repeat("世", 100)
	for budget := 20; budget < 40; budget++ {
		got := truncateBytes(s, budget)
		if !utf8.ValidString(got) {
			t.Fatalf("budget %d produced invalid UTF-8: %q", budget, got)
		}
	}
}
