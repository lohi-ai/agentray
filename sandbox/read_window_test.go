package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestReadFileNumbersLines(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.txt", "one\ntwo\nthree\n")
	out, err := NewReadFileTool(ws).Run(context.Background(), `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("read Run: %v", err)
	}
	if !strings.Contains(out, "1\tone") || !strings.Contains(out, "2\ttwo") {
		t.Fatalf("expected numbered lines, got %q", out)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.txt", "l1\nl2\nl3\nl4\nl5\n")
	out, err := NewReadFileTool(ws).Run(context.Background(), `{"path":"a.txt","offset":2,"limit":2}`)
	if err != nil {
		t.Fatalf("read Run: %v", err)
	}
	if !strings.Contains(out, "2\tl2") || !strings.Contains(out, "3\tl3") {
		t.Fatalf("window missing expected lines: %q", out)
	}
	if strings.Contains(out, "\tl1") || strings.Contains(out, "\tl4") {
		t.Fatalf("window leaked out-of-range lines: %q", out)
	}
	if !strings.Contains(out, "truncated: true") {
		t.Fatalf("expected truncated marker: %q", out)
	}
}
