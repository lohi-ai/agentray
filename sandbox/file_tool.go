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

const maxReadFileBytes = 64 * 1024

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
			"window of a large file. The path must be relative and cannot escape the workspace.",
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
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	truncated := false
	if len(data) > maxReadFileBytes {
		data = data[:maxReadFileBytes]
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
		truncated = true
	}

	// Window the requested line range (1-based offset, optional limit) and number
	// each line cat -n style so the model can reference exact lines for edits.
	lines := strings.Split(string(data), "\n")
	// A trailing newline yields a final empty element; drop it so the line count
	// and numbering match cat -n rather than reporting a phantom blank line.
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := in.Offset
	if start < 1 {
		start = 1
	}
	if start > len(lines) {
		start = len(lines) + 1
	}
	end := len(lines)
	if in.Limit > 0 && start+in.Limit-1 < end {
		end = start + in.Limit - 1
		truncated = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\nbytes: %d\nlines: %d", rel, info.Size(), len(lines))
	if truncated {
		fmt.Fprintf(&b, "\ntruncated: true")
	}
	b.WriteString("\ncontent:\n")
	for i := start; i <= end && i <= len(lines); i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, lines[i-1])
	}
	return strings.TrimRight(b.String(), "\n"), nil
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
