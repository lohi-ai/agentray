package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditFileReplacesUniqueMatch(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.txt", "hello world\n")

	edit := NewEditFileTool(ws)
	out, err := edit.Run(context.Background(), `{"path":"a.txt","old_string":"world","new_string":"there"}`)
	if err != nil {
		t.Fatalf("edit Run: %v", err)
	}
	if !strings.Contains(out, "replacements: 1") {
		t.Fatalf("edit output = %q", out)
	}
	got := mustRead(t, ws, "a.txt")
	if got != "hello there\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestEditFileRejectsAmbiguousMatch(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.txt", "x x x")
	edit := NewEditFileTool(ws)
	if _, err := edit.Run(context.Background(), `{"path":"a.txt","old_string":"x","new_string":"y"}`); err == nil {
		t.Fatal("expected ambiguous match to fail")
	}
	// replace_all makes it succeed.
	if _, err := edit.Run(context.Background(), `{"path":"a.txt","old_string":"x","new_string":"y","replace_all":true}`); err != nil {
		t.Fatalf("replace_all Run: %v", err)
	}
	if got := mustRead(t, ws, "a.txt"); got != "y y y" {
		t.Fatalf("content = %q", got)
	}
}

func TestEditFileMissingAndIdentical(t *testing.T) {
	ws := mustWorkspace(t)
	mustWrite(t, ws, "a.txt", "abc")
	edit := NewEditFileTool(ws)
	if _, err := edit.Run(context.Background(), `{"path":"a.txt","old_string":"zzz","new_string":"q"}`); err == nil {
		t.Fatal("expected not-found to fail")
	}
	if _, err := edit.Run(context.Background(), `{"path":"a.txt","old_string":"abc","new_string":"abc"}`); err == nil {
		t.Fatal("expected identical strings to fail")
	}
}

func TestEditFileRejectsEscape(t *testing.T) {
	ws := mustWorkspace(t)
	edit := NewEditFileTool(ws)
	if _, err := edit.Run(context.Background(), `{"path":"../x","old_string":"a","new_string":"b"}`); err == nil {
		t.Fatal("expected escape to fail")
	}
}

// --- shared helpers ---

func mustWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws
}

func mustWrite(t *testing.T, ws *Workspace, rel, content string) {
	t.Helper()
	abs := filepath.Join(ws.Root(), rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func mustRead(t *testing.T, ws *Workspace, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws.Root(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}
