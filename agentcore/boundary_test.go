package agentcore_test

import (
	"go/build"
	"os"
	"path/filepath"
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

// TestKernelTreeHoldsOnlyPlugins enforces README.md's opening claim — "one flat
// package, no subdirectories except plugins/" — which until now was the only
// structural rule here stated as prose rather than as a test, and consequently
// the only one that had drifted: an authoring/ package sat under agentcore for
// some time, run-time-adjacent by filename and authoring-time by content, with
// nothing to notice.
//
// The rule matters more than tidiness. "The kernel" has to name a tree a reader
// can enumerate, or the boundary tests above are checking one package while the
// directory quietly accumulates others that inherit the kernel's reputation
// without its constraints. A package that imports agentcore belongs beside it
// (authoring/, ai/, sandbox/); a package that extends a running agent belongs in
// plugins/. There is no third position, which is why this list has one entry.
func TestKernelTreeHoldsOnlyPlugins(t *testing.T) {
	const allowed = "plugins"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the agentcore directory: %v", err)
	}
	var sawAllowed bool
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		if e.Name() == allowed {
			sawAllowed = true
			continue
		}
		t.Errorf("agentcore/%s/ is a subdirectory of the kernel.\n"+
			"Only plugins/ may live here. If it extends a running agent it is a plugin; "+
			"otherwise it belongs beside agentcore, not under it.", e.Name())
	}
	// Guard against a vacuous pass: no plugins/ means the check is not reading
	// the real tree, and every future stray would pass silently.
	if !sawAllowed {
		t.Fatal("found no plugins/ directory — the check is not looking at the real tree")
	}
}

// TestPluginsDoNotNameEachOther enforces the second half of plugins/README.md's
// rule, the half Go's cycle check cannot cover:
//
//	Plugins do not name each other. Where one capability needs to know something
//	about another, the question is asked generically (RunInfo.Bookkeeping), never
//	by importing the other package.
//
// Nothing prevents these imports mechanically — plugins are siblings, so any of
// them may import any other with no cycle and no complaint. That makes this the
// cheapest rule in the system to break and the most expensive to unwind: once
// the repeat guard imports the todo plugin to ask whether a call was
// bookkeeping, ejecting todo stops being a composition change and becomes a
// build error, and the property the whole directory exists for is gone.
//
// preset is the one exemption, and it is exempt because it is not a capability:
// it is the aggregator that names the default set so a caller does not have to.
func TestPluginsDoNotNameEachOther(t *testing.T) {
	const aggregator = "preset"

	dirs, err := os.ReadDir("plugins")
	if err != nil {
		t.Fatalf("reading agentcore/plugins: %v", err)
	}
	var checked int
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		pkg, err := build.ImportDir(filepath.Join("plugins", name), 0)
		if err != nil {
			continue // not a Go package (docs-only folder); nothing to check
		}
		checked++
		if name == aggregator {
			continue
		}
		for _, imp := range pkg.Imports {
			if strings.HasPrefix(imp, pluginsPrefix) {
				t.Errorf("plugins/%s imports %s — plugins must not name each other.\n"+
					"Ask the question generically instead (RunInfo.Bookkeeping, an extension "+
					"interface in agentcore/extension.go), so either plugin can be ejected "+
					"without touching the other.", name, imp)
			}
		}
	}
	if checked < 10 {
		t.Fatalf("read only %d plugin packages — the check is not looking at the real tree", checked)
	}
}

// TestEveryPluginDocumentsItself keeps plugins/README.md's opening promise —
// "each folder has a README.md explaining what it does to the agent" — true for
// folders added later than the sentence.
//
// A capability's cost is invisible at its call site: a plugin list says which
// capabilities are installed and nothing about what each one puts in the prompt,
// what it does to the provider's prefix cache, or what it cannot do. The folder
// README is where that is written down, so an undocumented plugin is one a
// composer cannot price.
func TestEveryPluginDocumentsItself(t *testing.T) {
	dirs, err := os.ReadDir("plugins")
	if err != nil {
		t.Fatalf("reading agentcore/plugins: %v", err)
	}
	var checked int
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if _, err := build.ImportDir(filepath.Join("plugins", d.Name()), 0); err != nil {
			continue // not a Go package
		}
		checked++
		readme := filepath.Join("plugins", d.Name(), "README.md")
		if _, err := os.Stat(readme); err != nil {
			t.Errorf("plugins/%s has no README.md.\n"+
				"Say what it does to the agent: what the model sees, what it costs in "+
				"tokens, what it does to the prefix cache, and what it cannot do.", d.Name())
		}
	}
	if checked < 10 {
		t.Fatalf("read only %d plugin packages — the check is not looking at the real tree", checked)
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
