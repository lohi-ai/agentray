package dataplane

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbidden[layerSuffix] lists import path fragments that layer may not use.
// Every layer may import internal/shared. The composition root (internal/app,
// cmd/) may import every layer.
var forbidden = map[string][]string{
	"internal/dataplane": {
		"internal/channels", "internal/workloads", "internal/runtime", "internal/app",
	},
	"internal/runtime": {
		"internal/channels", "internal/workloads", "internal/app",
	},
	"internal/workloads": {
		"internal/dataplane", "internal/runtime",
		"internal/channels", "internal/app", "internal/shared",
	},
	"internal/channels": {
		"internal/dataplane", "internal/workloads", "internal/runtime", "internal/app",
	},
	"internal/shared": {
		"internal/channels", "internal/workloads", "internal/runtime",
		"internal/dataplane", "internal/app",
	},
}

func TestLayerImportRules(t *testing.T) {
	root := findModuleRoot(t)
	for layer, bans := range forbidden {
		dir := filepath.Join(root, layer)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			relPath, _ := filepath.Rel(root, path)
			for _, imp := range f.Imports {
				impPath := strings.Trim(imp.Path.Value, `"`)
				for _, ban := range bans {
					if strings.Contains(impPath, ban) {
						t.Errorf("%s imports %s — violates layer rule (no %s)", relPath, impPath, ban)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
