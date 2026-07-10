package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace guards file tools to one host directory. It accepts only relative
// paths, cleans them, follows the root symlink once, and rejects traversal before
// any filesystem operation happens.
type Workspace struct {
	root string
}

func NewWorkspace(root string) (*Workspace, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("workspace root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return nil, fmt.Errorf("workspace root: %w", err)
			}
			resolved = abs
		} else {
			return nil, fmt.Errorf("workspace root: %w", err)
		}
	}
	return &Workspace{root: filepath.Clean(resolved)}, nil
}

func (w *Workspace) Root() string {
	if w == nil {
		return ""
	}
	return w.root
}

func (w *Workspace) Resolve(rel string) (string, string, error) {
	if w == nil || w.root == "" {
		return "", "", fmt.Errorf("workspace is not configured")
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", "", fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("path must be relative")
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("path escapes workspace")
	}
	abs := filepath.Join(w.root, clean)
	if !inside(w.root, abs) {
		return "", "", fmt.Errorf("path escapes workspace")
	}
	return abs, clean, nil
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
