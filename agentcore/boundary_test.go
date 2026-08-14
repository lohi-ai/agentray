package agentcore_test

import (
	"go/build"
	"os"
	"strings"
	"testing"
)

// modulePath is this repo's module path. Anything under it is "our own code",
// which is exactly what the kernel is not allowed to reach for.
const modulePath = "github.com/lohi-ai/agentray"

// pluginsPrefix is the directory the rule is about.
const pluginsPrefix = modulePath + "/agentcore/plugins/"

// TestKernelNamesNoPlugin enforces plugins/README.md's one rule as a build
// artifact rather than a promise:
//
//	The loop names no plugin. Delete every package under agentcore/plugins and
//	agentcore still compiles and runs — it just does less.
//
// Go's own import cycle check covers most of this for free: every plugin today
// imports agentcore, so importing one back is already a build error. What it
// does NOT cover is a plugin package that does not import the kernel — a pure
// data or prompt helper under plugins/ — which the kernel could reach for with
// no cycle and no complaint. That is the case this test exists for, and it is
// the likely shape of the first violation, because it is the one that compiles.
//
// It checks the package's DIRECT imports, which is sufficient rather than
// sloppy: TestKernelIsAModuleLeaf below proves the kernel imports nothing else
// in this module at all, so there is no in-repo package a plugin could be
// smuggled in behind.
func TestKernelNamesNoPlugin(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading the agentcore package: %v", err)
	}
	for _, imp := range pkg.Imports {
		if strings.HasPrefix(imp, pluginsPrefix) {
			t.Errorf("package agentcore imports %s — the kernel must not name a plugin.\n"+
				"Reach it through an interface in extension.go (ToolInterceptor, StepInterceptor, "+
				"ExtensionFactory, …) and let the composition supply it.", imp)
		}
	}
}

// TestKernelIsAModuleLeaf is the stronger property the rule rests on: agentcore
// depends on NOTHING else in this module — not the plugins, not ai, not
// internal/. That is what makes it publishable on its own, and what makes the
// direct-import check above a complete one.
//
// Third-party and stdlib imports are fine; the leaf property is about this
// module's own packages, since those are the ones that could depend back on the
// kernel and close a cycle the plugin split exists to prevent.
func TestKernelIsAModuleLeaf(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading the agentcore package: %v", err)
	}
	for _, imp := range pkg.Imports {
		if imp == modulePath || strings.HasPrefix(imp, modulePath+"/") {
			t.Errorf("package agentcore imports %s — the kernel must stay a leaf of this module.\n"+
				"Whatever it needs belongs behind a seam or an interface the host fills in.", imp)
		}
	}

	// Guard against a vacuous pass: a package that read as having no imports at
	// all would satisfy every check above.
	if len(pkg.Imports) == 0 {
		t.Fatal("read 0 imports for package agentcore — the check is not looking at the real package")
	}
	if len(pkg.GoFiles) < 10 {
		t.Fatalf("read only %d Go files for package agentcore — the check is not looking at the real package", len(pkg.GoFiles))
	}
}

// TestEveryKernelFileJustifiesItself keeps README.md's file map honest.
//
// The map is the answer to the only organizing question a flat package has —
// "why is this file in the kernel and not a plugin?" — and an unlisted file is
// a file nobody had to answer it for. That is how a kernel grows a plugin's
// worth of code back: one unremarkable addition at a time, each individually
// fine, none of them ever compared against the rule.
//
// So the map is not documentation of the package; it is a gate on adding to it.
func TestEveryKernelFileJustifiesItself(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading agentcore/README.md: %v", err)
	}
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading the agentcore package: %v", err)
	}
	if len(pkg.GoFiles) < 10 {
		t.Fatalf("read only %d Go files — the check is not looking at the real package", len(pkg.GoFiles))
	}
	doc := string(readme)
	for _, f := range pkg.GoFiles {
		if !strings.Contains(doc, "`"+f+"`") {
			t.Errorf("%s is in the kernel but not in agentcore/README.md.\n"+
				"Add a row saying why it is core — loop, contract, log, or seam default. "+
				"If it cannot claim one, it belongs in a plugin.", f)
		}
	}
}
