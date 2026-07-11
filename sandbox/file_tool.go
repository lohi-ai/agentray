package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/lohi-ai/agentray/agentcore"
)

const (
	ToolReadFile  = "read_file"
	ToolWriteFile = "write_file"
)

const (
	// maxReadFileBytes is the byte budget for one read window. The window is
	// selected first (offset/limit), then trimmed to this budget — so paging with
	// offset can always reach every line of the file.
	maxReadFileBytes = 64 * 1024
	// maxReadWholeFileBytes bounds how large a file the tool will load at all;
	// beyond this the caller is told to grep for the region instead.
	maxReadWholeFileBytes = 16 * 1024 * 1024
)

type ReadFileTool struct {
	workspace *Workspace
}

func NewReadFileTool(workspace *Workspace) *ReadFileTool {
	return &ReadFileTool{workspace: workspace}
}

func (t *ReadFileTool) Name() string { return ToolReadFile }

func (t *ReadFileTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolReadFile,
		Description: "Read a UTF-8 text file from the agent workspace. Content is returned with " +
			"cat -n style line numbers so you can cite exact lines. Use offset and limit to read a " +
			"window of a large file; a truncated read ends with the exact offset to continue from. " +
			"The path must be relative and cannot escape the workspace.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Workspace-relative file path."},
				"offset": map[string]any{
					"type":        "integer",
					"description": "1-based line number to start reading from. Defaults to 1.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of lines to return. Defaults to the whole file (capped for size).",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (t *ReadFileTool) Parallel() bool { return true }

func (t *ReadFileTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("read_file: invalid arguments: %w", err)
	}
	abs, rel, err := t.workspace.Resolve(in.Path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if !inside(t.workspace.Root(), resolved) {
		return "", fmt.Errorf("read_file: path escapes workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read_file: %s is a directory", rel)
	}
	if info.Size() > maxReadWholeFileBytes {
		return "", fmt.Errorf("read_file: %s is %d bytes, over the %dMB read cap — use grep to locate the region you need",
			rel, info.Size(), maxReadWholeFileBytes/(1024*1024))
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	// Window the requested line range FIRST (1-based offset, optional limit),
	// then trim the selected window to the byte budget — never the file as a
	// whole, so offset-paging can reach every line no matter the file size.
	lines := strings.Split(string(data), "\n")
	// A trailing newline yields a final empty element; drop it so the line count
	// and numbering match cat -n rather than reporting a phantom blank line.
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	start := in.Offset
	if start < 1 {
		start = 1
	}
	if start > total {
		return "", fmt.Errorf("read_file: offset %d is beyond end of file (%d lines)", in.Offset, total)
	}
	end := total
	if in.Limit > 0 && start+in.Limit-1 < end {
		end = start + in.Limit - 1
	}

	// Emit numbered lines cat -n style until the byte budget runs out. Always
	// emit at least one line so a single oversized line can't yield an empty
	// window — it is clamped to the budget instead.
	var content strings.Builder
	last := start - 1
	for i := start; i <= end; i++ {
		line := lines[i-1]
		if content.Len()+len(line) > maxReadFileBytes {
			if i == start {
				fmt.Fprintf(&content, "%6d\t%s\n", i, clampUTF8(line, maxReadFileBytes))
				fmt.Fprintf(&content, "[line %d is %d bytes; showing its first %dKB]\n", i, len(line), maxReadFileBytes/1024)
				last = i
			}
			break
		}
		fmt.Fprintf(&content, "%6d\t%s\n", i, line)
		last = i
	}

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\nbytes: %d\nlines: %d", rel, info.Size(), total)
	if last < total {
		b.WriteString("\ntruncated: true")
	}
	b.WriteString("\ncontent:\n")
	b.WriteString(content.String())
	if last < total {
		fmt.Fprintf(&b, "\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", start, last, total, last+1)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// clampUTF8 cuts s to at most max bytes without splitting a UTF-8 sequence.
func clampUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

type WriteFileTool struct {
	workspace *Workspace
}

func NewWriteFileTool(workspace *Workspace) *WriteFileTool {
	return &WriteFileTool{workspace: workspace}
}

func (t *WriteFileTool) Name() string { return ToolWriteFile }

func (t *WriteFileTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        ToolWriteFile,
		Description: "Write a UTF-8 text file inside the agent workspace. Parent directories are created; paths must be relative and cannot escape the workspace.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Workspace-relative file path."},
				"content": map[string]any{"type": "string", "description": "Complete file content to write."},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (t *WriteFileTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("write_file: invalid arguments: %w", err)
	}
	abs, rel, err := t.workspace.Resolve(in.Path)
	if err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	if !inside(t.workspace.Root(), resolvedDir) {
		return "", fmt.Errorf("write_file: path escapes workspace")
	}
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("write_file: refusing to follow symlink")
	}
	if err := os.WriteFile(abs, []byte(in.Content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("path: %s\nbytes_written: %d", rel, len(in.Content)), nil
}
