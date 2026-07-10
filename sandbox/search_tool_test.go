package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestGrepFindsMatchesWithLocation(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "src/a.go", "package a\nfunc Foo() {}\n")
	mustWrite(t, ws, "src/b.txt", "nothing here\nFoo mention\n")

	grep := NewGrepTool(ws)
	out, err := grep.Run(context.Background(), `{"pattern":"Foo"}`)
	if err != nil {
		t.Fatalf("grep Run: %v", err)
	}
	if !strings.Contains(out, "src/a.go:2:") || !strings.Contains(out, "src/b.txt:2:") {
		t.Fatalf("grep output = %q", out)
	}
}

func TestGrepGlobFilterAndCaseInsensitive(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.go", "TODO fix\n")
	mustWrite(t, ws, "b.md", "todo doc\n")

	grep := NewGrepTool(ws)
	out, err := grep.Run(context.Background(), `{"pattern":"todo","glob":"*.go","case_insensitive":true}`)
	if err != nil {
		t.Fatalf("grep Run: %v", err)
	}
	if !strings.Contains(out, "a.go") || strings.Contains(out, "b.md") {
		t.Fatalf("glob filter failed: %q", out)
	}
}

func TestGrepNoMatch(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.txt", "hello")
	out, err := NewGrepTool(ws).Run(context.Background(), `{"pattern":"zzz"}`)
	if err != nil {
		t.Fatalf("grep Run: %v", err)
	}
	if out != "no matches" {
		t.Fatalf("expected no matches, got %q", out)
	}
}

func TestGlobMatchesDoublestar(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "src/deep/x.go", "")
	mustWrite(t, ws, "src/y.go", "")
	mustWrite(t, ws, "z.txt", "")

	glob := NewGlobTool(ws)
	out, err := glob.Run(context.Background(), `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatalf("glob Run: %v", err)
	}
	if !strings.Contains(out, "src/deep/x.go") || !strings.Contains(out, "src/y.go") {
		t.Fatalf("glob output = %q", out)
	}
	if strings.Contains(out, "z.txt") {
		t.Fatalf("glob matched non-go file: %q", out)
	}
}

func TestGlobSingleStarDoesNotCrossSlash(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.go", "")
	mustWrite(t, ws, "sub/b.go", "")

	out, err := NewGlobTool(ws).Run(context.Background(), `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("glob Run: %v", err)
	}
	if !strings.Contains(out, "a.go") || strings.Contains(out, "sub/b.go") {
		t.Fatalf("single-star crossed slash: %q", out)
	}
}

func TestGrepSkipsDependencyDirs(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "node_modules/dep/x.js", "needle")
	mustWrite(t, ws, "app.js", "needle")
	out, err := NewGrepTool(ws).Run(context.Background(), `{"pattern":"needle"}`)
	if err != nil {
		t.Fatalf("grep Run: %v", err)
	}
	if strings.Contains(out, "node_modules") {
		t.Fatalf("grep walked node_modules: %q", out)
	}
	if !strings.Contains(out, "app.js") {
		t.Fatalf("grep missed app.js: %q", out)
	}
}
