package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lohi-ai/agentray/agentcore"
)

const (
	ToolGrep = "grep"
	ToolGlob = "glob"
)

const (
	maxGrepMatches    = 200
	maxGlobMatches    = 500
	maxGrepFileBytes  = 2 * 1024 * 1024 // skip files larger than this when searching content
	maxGrepLineLength = 400
)

// skipDir lists directories never worth walking for search: VCS metadata and
// dependency caches. Mirrors the practical defaults of ripgrep/Claude Code's
// grep without a full .gitignore parser.
var skipDir = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	".next":        {},
	"dist":         {},
	"vendor":       {},
}

// GrepTool searches file contents in the workspace by regular expression
// (Claude Code's Grep / pi's grep). Pure Go regexp over a guarded walk — no
// shell, no host FS. Returns file:line:match lines, capped for token safety.
type GrepTool struct {
	workspace *Workspace
}

func NewGrepTool(workspace *Workspace) *GrepTool { return &GrepTool{workspace: workspace} }

func (t *GrepTool) Name() string   { return ToolGrep }
func (t *GrepTool) Parallel() bool { return true }

func (t *GrepTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolGrep,
		Description: "Search file contents in the agent workspace by regular expression (Go/RE2 syntax). " +
			"Returns matching lines as path:line:text, capped at " + fmt.Sprint(maxGrepMatches) + " matches. " +
			"Use glob to narrow which files are searched and path to scope to a subdirectory.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "RE2 regular expression to match against each line."},
				"path":    map[string]any{"type": "string", "description": "Optional workspace-relative subdirectory to search. Defaults to the whole workspace."},
				"glob":    map[string]any{"type": "string", "description": "Optional filename filter, e.g. *.go or **/*.ts. Matches the workspace-relative path."},
				"case_insensitive": map[string]any{
					"type":        "boolean",
					"description": "Case-insensitive match. Defaults to false.",
				},
			},
			"required": []string{"pattern"},
		},
	}
}

func (t *GrepTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		CaseInsensitive bool   `json:"case_insensitive"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("grep: invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return "", fmt.Errorf("grep: pattern is empty")
	}
	expr := in.Pattern
	if in.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}

	root, err := t.searchRoot(in.Path)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	var globRe *regexp.Regexp
	if g := strings.TrimSpace(in.Glob); g != "" {
		globRe, err = compileGlob(g)
		if err != nil {
			return "", fmt.Errorf("grep: invalid glob: %w", err)
		}
	}

	var matches []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the search
		}
		if d.IsDir() {
			if _, skip := skipDir[d.Name()]; skip && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // never follow symlinks out of the workspace
		}
		rel := t.rel(path)
		if globRe != nil && !globRe.MatchString(rel) {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Size() > maxGrepFileBytes {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || !utf8.Valid(data) {
			return nil // unreadable or binary
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if len(matches) >= maxGrepMatches {
				truncated = true
				return fs.SkipAll
			}
			matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, i+1, clampLine(line)))
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("grep: %w", walkErr)
	}
	if len(matches) == 0 {
		return "no matches", nil
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n…[truncated at %d matches]", maxGrepMatches)
	}
	return out, nil
}

// GlobTool lists workspace files whose relative path matches a glob pattern
// (Claude Code's Glob), supporting * ? and ** segments. Results are sorted for
// stable output and capped for token safety.
type GlobTool struct {
	workspace *Workspace
}

func NewGlobTool(workspace *Workspace) *GlobTool { return &GlobTool{workspace: workspace} }

func (t *GlobTool) Name() string   { return ToolGlob }
func (t *GlobTool) Parallel() bool { return true }

func (t *GlobTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolGlob,
		Description: "List files in the agent workspace whose relative path matches a glob pattern " +
			"(supports *, ?, and ** for any depth, e.g. **/*.go or src/**/test_*.ts). " +
			"Returns up to " + fmt.Sprint(maxGlobMatches) + " sorted paths.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern matched against the workspace-relative path."},
				"path":    map[string]any{"type": "string", "description": "Optional workspace-relative subdirectory to search within."},
			},
			"required": []string{"pattern"},
		},
	}
}

func (t *GlobTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("glob: invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return "", fmt.Errorf("glob: pattern is empty")
	}
	globRe, err := compileGlob(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("glob: invalid pattern: %w", err)
	}
	root, err := t.searchRoot(in.Path)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}

	var hits []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, skip := skipDir[d.Name()]; skip && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel := t.rel(path)
		if globRe.MatchString(rel) {
			if len(hits) >= maxGlobMatches {
				truncated = true
				return fs.SkipAll
			}
			hits = append(hits, rel)
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("glob: %w", walkErr)
	}
	if len(hits) == 0 {
		return "no files match", nil
	}
	sort.Strings(hits)
	out := strings.Join(hits, "\n")
	if truncated {
		out += fmt.Sprintf("\n…[truncated at %d files]", maxGlobMatches)
	}
	return out, nil
}

// searchRoot resolves an optional relative subdirectory to an absolute path
// inside the workspace, defaulting to the workspace root. Shared by grep/glob.
func (t *GrepTool) searchRoot(sub string) (string, error) { return searchRoot(t.workspace, sub) }
func (t *GlobTool) searchRoot(sub string) (string, error) { return searchRoot(t.workspace, sub) }
func (t *GrepTool) rel(abs string) string                 { return workspaceRel(t.workspace, abs) }
func (t *GlobTool) rel(abs string) string                 { return workspaceRel(t.workspace, abs) }

func searchRoot(ws *Workspace, sub string) (string, error) {
	if strings.TrimSpace(sub) == "" {
		if ws.Root() == "" {
			return "", fmt.Errorf("workspace is not configured")
		}
		return ws.Root(), nil
	}
	abs, _, err := ws.Resolve(sub)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if !inside(ws.Root(), resolved) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return resolved, nil
}

func workspaceRel(ws *Workspace, abs string) string {
	rel, err := filepath.Rel(ws.Root(), abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

func clampLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if len(s) > maxGrepLineLength {
		return s[:maxGrepLineLength] + "…"
	}
	return s
}

// compileGlob converts a shell-style glob (with *, ?, and ** segments) into an
// anchored RE2 regexp matched against a slash-separated relative path. ** spans
// any number of path segments; * does not cross a slash.
func compileGlob(glob string) (*regexp.Regexp, error) {
	glob = filepath.ToSlash(strings.TrimSpace(glob))
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++ // consume second *
				// "**/" or "**" — match across directory separators.
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
