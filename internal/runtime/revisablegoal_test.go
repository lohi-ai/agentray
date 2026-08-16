package agentruntime

import (
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
)

// The revisable goal gate is two decisions that are only correct together, and
// the failure mode when they disagree is silence.
//
// agentcore owns one half: a goal.Plugin with Revisable set contributes
// update_goal from its run extension (plugins/goal's TestTheToolIsOptIn proves
// it, and proves the default withholds it). This file owns the other half — the
// product's default-deny allow-list. AllowList.PermittedTools filters EVERY
// contributed tool through that list, so a composition that installs the
// revisable gate and forgets the name registers update_goal and then hides it
// from the model. Nothing errors. Nothing logs. The run simply never revises its
// goal, and the only way to notice is to wonder why a capability you switched on
// never fires.
//
// The two halves meet at the goal.ToolName constant, which both sides read, so
// they cannot drift on spelling — only on presence. Presence is what is asserted
// here.

// TestARevisableGoalIsPermittedNotJustInstalled is the regression guard for that
// silence: switching the capability on must put the tool in the allow-list too.
func TestARevisableGoalIsPermittedNotJustInstalled(t *testing.T) {
	p := representativeBuildParams()
	p.ReviseGoal = true

	if !contains(permittedToolNames(p), goal.ToolName) {
		t.Fatalf("%s is not in the run's allow-list, so the gate would contribute it and the "+
			"policy would filter it straight back out — the model is never shown a tool the "+
			"composition believes it enabled", goal.ToolName)
	}

	// The plugin half has to survive the swap. preset.Replace drops the entry and
	// re-appends it, which MOVES it — and the goal gate's position is a stated
	// contract, not an accident: it and the finish guard are the composition's
	// only two stop interceptors, the first to re-open a run wins, and verifying
	// an answer that does not meet the goal is wasted work.
	agent, err := Build(p)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	exts := extensionsLineOf(agent.Describe())
	if !strings.Contains(exts, "goal") {
		t.Fatalf("the revisable swap dropped the goal gate entirely: %s", exts)
	}
	if strings.Index(exts, "goal") > strings.Index(exts, "finish_guard") {
		t.Fatalf("replacing the goal plugin moved it behind the finish guard, inverting the "+
			"stop-interceptor contract: %s", exts)
	}
}

// TestTheDefaultRunOffersNoGoalTool pins the default. Handing a model the key to
// its own completion gate is a deliberate act, so a composition that did not ask
// for it must carry no trace — an allow-list entry for a tool nothing installed
// is harmless today and becomes a silently granted capability the moment the
// gate's default changes.
func TestTheDefaultRunOffersNoGoalTool(t *testing.T) {
	p := representativeBuildParams()
	if p.ReviseGoal {
		t.Fatal("the revisable goal gate is now on by default: that is a policy change, not a " +
			"test failure — the model can restate its own finish condition unless a caller opts out")
	}
	if contains(permittedToolNames(p), goal.ToolName) {
		t.Fatalf("%s is permitted in a run that never asked to be revisable", goal.ToolName)
	}
}

// TestARevisableGoalNeedsAGoalToRevise: the gate declines an ungated run, so the
// tool would be offered with nothing behind it — a model that called it would be
// editing a condition no one is enforcing.
func TestARevisableGoalNeedsAGoalToRevise(t *testing.T) {
	p := representativeBuildParams()
	p.ReviseGoal = true
	p.Goal = ""

	if contains(permittedToolNames(p), goal.ToolName) {
		t.Fatalf("%s is permitted on an ungated run: the gate declines at BeginRun, so the tool "+
			"edits a condition nothing enforces", goal.ToolName)
	}
	if _, err := Build(p); err != nil {
		t.Fatalf("Build: %v", err)
	}
}

// TestEveryPluginToolIsAllowListed is the general form, and the reason this file
// is worth more than three assertions about one tool.
//
// Scope-derived analytics tools are registered in full and narrowed by the
// policy — that gap is the design. Plugin-contributed tools are the opposite:
// each is installed only because a BuildParams field asked for it, so being
// installed and not being permitted is never intended, only ever forgotten.
// The next plugin tool added to Build is caught here.
func TestEveryPluginToolIsAllowListed(t *testing.T) {
	p := representativeBuildParams()
	p.ReviseGoal = true
	names := permittedToolNames(p)

	// Every tool a conditionally-installed plugin contributes, against the field
	// that installs it. Todo and Subagents are already set by the representative
	// params; ReviseGoal is set above.
	for field, tool := range map[string]string{
		"Todo":       "update_plan",
		"Subagents":  "spawn_subagent",
		"ReviseGoal": goal.ToolName,
	} {
		if !contains(names, tool) {
			t.Fatalf("BuildParams.%s installs a plugin contributing %q, but the allow-list does "+
				"not name it: the tool is registered and unreachable", field, tool)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
