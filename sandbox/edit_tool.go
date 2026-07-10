package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

const ToolEditFile = "edit_file"

// EditFileTool performs surgical in-place edits: it replaces an exact substring
// in a workspace file rather than rewriting the whole thing (Claude Code's Edit /
// pi's edit). This keeps large files cheap to change and makes intent reviewable —
// the model states the precise text it is swapping. It shares Workspace and the
// same symlink/escape guards as write_file so it can never touch the host FS.
type EditFileTool struct {
	workspace *Workspace
}

func NewEditFileTool(workspace *Workspace) *EditFileTool {
	return &EditFileTool{workspace: workspace}
}

func (t *EditFileTool) Name() string { return ToolEditFile }

func (t *EditFileTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolEditFile,
		Description: "Replace an exact string in a UTF-8 text file inside the agent workspace. " +
			"old_string must appear exactly once unless replace_all is true; otherwise the edit is " +
			"refused as ambiguous. Use this for surgical changes instead of rewriting the whole file " +
			"with write_file. Paths must be relative and cannot escape the workspace.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "Workspace-relative file path."},
				"old_string": map[string]any{"type": "string", "description": "Exact text to replace. Include enough surrounding context to be unique."},
				"new_string": map[string]any{"type": "string", "description": "Replacement text. Must differ from old_string."},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every occurrence instead of requiring a unique match. Defaults to false.",
				},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
}

func (t *EditFileTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("edit_file: invalid arguments: %w", err)
	}
	if in.OldString == in.NewString {
		return "", fmt.Errorf("edit_file: old_string and new_string are identical")
	}
	if in.OldString == "" {
		return "", fmt.Errorf("edit_file: old_string is empty")
	}

	abs, rel, err := t.workspace.Resolve(in.Path)
	if err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}
	if !inside(t.workspace.Root(), resolved) {
		return "", fmt.Errorf("edit_file: path escapes workspace")
	}
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("edit_file: refusing to follow symlink")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}
	content := string(data)

	count := strings.Count(content, in.OldString)
	if count == 0 {
		return "", fmt.Errorf("edit_file: old_string not found in %s", rel)
	}
	if count > 1 && !in.ReplaceAll {
		return "", fmt.Errorf("edit_file: old_string appears %d times in %s; add context to make it unique or set replace_all", count, rel)
	}

	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
	}

	if err := os.WriteFile(resolved, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}
	return fmt.Sprintf("path: %s\nreplacements: %d\nbytes: %d", rel, count, len(updated)), nil
}
