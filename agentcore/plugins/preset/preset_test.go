package preset_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/compaction"
	"github.com/lohi-ai/agentray/agentcore/plugins/finishguard"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/jobs"
	"github.com/lohi-ai/agentray/agentcore/plugins/model"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/policy"
	"github.com/lohi-ai/agentray/agentcore/plugins/preset"
	"github.com/lohi-ai/agentray/agentcore/plugins/repeatguard"
	"github.com/lohi-ai/agentray/agentcore/plugins/sessionquery"
	"github.com/lohi-ai/agentray/agentcore/plugins/spill"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"github.com/lohi-ai/agentray/agentcore/plugins/tools"
)

// echoTool is a minimal host tool for composition tests.
type echoTool struct{ name string }

func (t *echoTool) Name() string { return t.name }

func (t *echoTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{Name: t.name, Description: "echo", Parameters: map[string]any{"type": "object"}}
}

func (t *echoTool) Run(ctx context.Context, args string) (string, error) { return args, nil }

// fullConfig exercises every branch of the Config → plugin mapping, so parity
// is proven over the whole surface rather than the fields that happened to be
// convenient.
func fullConfig() agentcore.Config {
	provider := &agentcore.FauxProvider{}
	retry := agentcore.DefaultRetryPolicy()
	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 7
	env := agentcore.DefaultEnv()
	comp := agentcore.DefaultCompactionSettings()
	comp.KeepRecentTokens = 1234

	return agentcore.Config{
		Provider:   provider,
		Model:      "primary",
		Escalation: []agentcore.ModelRung{{Provider: provider, Model: "fallback"}},
		Retry:      &retry,
		RefreshKey: func(ctx context.Context, p string) (string, error) { return "k", nil },
		MaxTokens:  8192, ReasoningEffort: "high",
		PromptCacheKey: "cache-key", PromptCacheRetention: "long",

		Definition: agentcore.AgentDefinition{ScopeID: "scope", Soul: "soul"},
		Limits:     &limits,
		Env:        &env,

		Tools:      agentcore.NewToolSet(&echoTool{name: "echo"}, &echoTool{name: "other"}),
		Policy:     agentcore.NewAllowList("echo"),
		Goal:       "ship it",
		BudgetGate: func(ctx context.Context, u agentcore.Usage) bool { return false },
		StepGate:   func(ctx context.Context, turn int) error { return nil },

		Session: agentcore.NewMemorySessionStore(), SessionID: "s1", ResumeSession: false,
		SeedDisabledTools:  []string{"broken"},
		Compaction:         &comp,
		CompactionProvider: provider, CompactionModel: "cheap",

		GetSteeringMessages: func(ctx context.Context) []agentcore.Message { return nil },
		GetFollowUpMessages: func(ctx context.Context) []agentcore.Message { return nil },
		PrepareNextTurn: func(ctx context.Context, s agentcore.TurnState) agentcore.TurnState {
			return s
		},

		// Every ejectable capability, in one field. Config knows none of their
		// names — which is why adding a capability never edits agentcore.
		Extensions: []agentcore.ExtensionFactory{
			repeatguard.At(3),
			finishguard.Of(func(context.Context, finishguard.State) string { return "" }),
			spill.To(spill.NewMemorySpillStore()),
			jobs.Local(),
			sessionquery.OwnSession(),
			subagent.SelfOnly(),
			observe.LogInvariant{Report: func(observe.LogInvariantViolation) {}},
		},
	}
}

// TestPluginCompositionMatchesNew is the contract that keeps the two entry
// points honest. agentcore.New applies Config through Registry.ApplyConfig;
// preset.Plugins routes the same Config through twenty separate plugin
// packages. They must produce the same agent.
//
// If someone adds a Config field and wires it in only one place, this fails.
func TestPluginCompositionMatchesNew(t *testing.T) {
	cfg := fullConfig()

	fromConfig, err := agentcore.New(cfg)
	if err != nil {
		t.Fatalf("agentcore.New: %v", err)
	}
	fromPlugins, err := preset.New(cfg)
	if err != nil {
		t.Fatalf("preset.New: %v", err)
	}

	// Seam parity is the property: every Config field must reach the same seam
	// through both entry points. The extensions line is compared separately
	// below, because the two paths legitimately differ there — see the comment
	// on that check.
	want, got := withoutExtensions(fromConfig.Describe()), withoutExtensions(fromPlugins.Describe())
	if want != got {
		t.Fatalf("plugin composition diverged from New(Config).\n--- New(Config) ---\n%s\n--- preset.New ---\n%s", want, got)
	}

	// The one deliberate difference: enforcing a goal is a plugin, so
	// preset.New installs the gate and agentcore.New does not. Config.Goal is
	// the DURABLE RECORD (both paths set that seam, proven above); holding the
	// run to it is agentcore/plugins/goal, which core cannot import.
	//
	// Pinned rather than papered over: if the gate ever stops being a plugin —
	// or preset ever stops wiring it — one of these two fails.
	if exts := extensionsLine(fromPlugins.Describe()); !strings.Contains(exts, "goal") {
		t.Fatalf("preset.New must install the goal gate, got extensions: %s", exts)
	}
	if exts := extensionsLine(fromConfig.Describe()); strings.Contains(exts, "goal") {
		t.Fatalf("agentcore.New must NOT install the goal gate (it cannot import the plugin), got: %s", exts)
	}
	// Guard against a vacuous pass: a Describe that reported nothing would make
	// any two agents "equal".
	for _, marker := range []string{"goal:", "session:", "tools:", "policy:"} {
		if !strings.Contains(want, marker) {
			t.Fatalf("Describe() is too thin to prove parity — missing %q", marker)
		}
	}
}

// TestPresetIsAllRealPlugins guards the claim that agentcore is rebuilt FROM
// the plugin packages: every capability in the default composition must be a
// named plugin, not something the core quietly does anyway.
func TestPresetIsAllRealPlugins(t *testing.T) {
	names := map[string]bool{}
	for _, p := range preset.Plugins(fullConfig()) {
		if p.Name() == "" {
			t.Fatalf("plugin %T has no name", p)
		}
		if names[p.Name()] {
			t.Fatalf("duplicate plugin name %q", p.Name())
		}
		names[p.Name()] = true
	}
	for _, want := range []string{
		// seams: exactly one provider each (the driver seam is defaulted by
		// Build, so no "loop" plugin appears here)
		"model", "definition", "tools", "policy", "hooks", "goal",
		"budget", "session", "memory", "compaction", "steering",
		// extensions: additive capabilities the loop never names
		"finish_guard", "repeat_guard", "spill", "jobs", "session_query",
		"subagent", "log_invariant",
	} {
		if !names[want] {
			t.Fatalf("the default composition is missing the %q plugin", want)
		}
	}
}

// TestFullIsPluginsPlusTheEjectables is the containment contract: Full may only
// ADD to Plugins. Plugins is what the parity test pins to agentcore.New, so a
// capability that quietly rewrote or dropped one of its entries would break the
// proof that the two entry points agree while every test still passed.
func TestFullIsPluginsPlusTheEjectables(t *testing.T) {
	cfg := fullConfig()
	cfg.Extensions = nil // Full adds these as named plugins instead.

	base := preset.Plugins(cfg)
	full := preset.Full(cfg, preset.Options{
		Spill:              spill.NewMemorySpillStore(),
		ReportLogInvariant: func(observe.LogInvariantViolation) {},
	})

	if len(full) <= len(base) {
		t.Fatalf("Full added nothing: %d plugins vs %d", len(full), len(base))
	}
	// Same plugins, same order, for the whole of the base list — Full is a
	// suffix append, not a re-composition.
	for i, p := range base {
		if full[i].Name() != p.Name() {
			t.Fatalf("Full diverged from Plugins at %d: %q != %q", i, full[i].Name(), p.Name())
		}
	}
	names := map[string]bool{}
	for _, p := range full {
		if names[p.Name()] {
			t.Fatalf("duplicate plugin name %q in Full", p.Name())
		}
		names[p.Name()] = true
	}
	for _, want := range []string{"spill", "jobs", "repeat_guard", "session_query", "observe", "log_invariant"} {
		if !names[want] {
			t.Fatalf("Full is missing the %q plugin", want)
		}
	}
}

// TestFullComposesCleanly builds the full agent for real: every one of the five
// must register without a seam conflict and reach the composed agent. A plugin
// that only appears in a list it was appended to is not installed.
func TestFullComposesCleanly(t *testing.T) {
	cfg := fullConfig()
	cfg.Extensions = nil

	agent, err := agentcore.Build(preset.Full(cfg, preset.Options{
		Spill:              spill.NewMemorySpillStore(),
		ReportLogInvariant: func(observe.LogInvariantViolation) {},
	})...)
	if err != nil {
		t.Fatalf("Build(preset.Full): %v", err)
	}
	exts := extensionsLine(agent.Describe())
	for _, want := range []string{"spill", "jobs", "repeat_guard", "session_query", "log_invariant"} {
		if !strings.Contains(exts, want) {
			t.Fatalf("%q did not reach the composed agent: %s", want, exts)
		}
	}
}

// TestFullDegradesWithoutStorage: the zero Options must still build. Spilling
// needs a durable store, and the honest answer to "none supplied" is to leave
// the loop's own truncation in place — not to mint locators into a store that
// dies with the process, which a resumed run would read back as not-found.
func TestFullDegradesWithoutStorage(t *testing.T) {
	cfg := fullConfig()
	cfg.Extensions = nil

	agent, err := agentcore.Build(preset.Full(cfg, preset.Options{})...)
	if err != nil {
		t.Fatalf("Build(preset.Full) with zero Options: %v", err)
	}
	dump := agent.Describe()
	if strings.Contains(extensionsLine(dump), "spill") {
		t.Fatalf("spill installed with no store:\n%s", dump)
	}
	if strings.Contains(extensionsLine(dump), "log_invariant") {
		t.Fatalf("the log invariant installed with nowhere to report:\n%s", dump)
	}
	// The capabilities that need no configuration are still there.
	if !strings.Contains(extensionsLine(dump), "jobs") {
		t.Fatalf("jobs needs no configuration and must still install:\n%s", dump)
	}
}

// TestJobToolsAreBookkeeping pins the interaction between two plugins that do
// not know about each other: polling a background job is administration, so the
// repeat guard must see the job_* tools as transparent rather than nudging the
// legitimate "check the same job until it finishes" chain as a stuck loop, and
// the turn spent polling must not be charged against MaxTurns.
func TestJobToolsAreBookkeeping(t *testing.T) {
	ext, err := jobs.Local().BeginRun(context.Background(), agentcore.RunInfo{Owner: "run_1"})
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	provider, ok := ext.(interface{ Tools() []agentcore.Tool })
	if !ok {
		t.Fatal("the jobs extension no longer contributes tools")
	}
	tools := provider.Tools()
	if len(tools) == 0 {
		t.Fatal("the jobs extension contributed no tools")
	}
	for _, tool := range tools {
		bk, ok := tool.(agentcore.BookkeepingTool)
		if !ok || !bk.Bookkeeping() {
			t.Fatalf("%q is not declared bookkeeping", tool.Name())
		}
	}
}

// TestFullGatesAResumedRun drives the composition end to end on the case that
// motivates having a durable goal at all: a run is gated, it crashes, and the
// process that picks it up does NOT know what it was gated on.
//
// The condition comes back from the log, not from configuration — the resuming
// caller passes no goal — and the gate still refuses an answer without the
// sentinel. That is the whole "resumes gated" claim, and it only holds if the
// durable half (the seam) and the enforcing half (the extension) are both in the
// composition. Running it through preset.Full is what proves the composition a
// deployment actually builds has both.
func TestFullGatesAResumedRun(t *testing.T) {
	ctx := context.Background()
	store := agentcore.NewMemorySessionStore()

	// The crashed run's log: it was gated, it produced no leaf.
	for _, e := range []agentcore.SessionEntry{
		{Kind: agentcore.EntryGoal, Goal: "STATUS: DONE appears"},
		{Kind: agentcore.EntryMessage, Message: &agentcore.Message{Role: agentcore.RoleUser, Content: "count the beans"}},
	} {
		if err := store.Append(ctx, "s-crashed", e); err != nil {
			t.Fatalf("seed log: %v", err)
		}
	}

	// First answer omits the sentinel and must be refused; the second carries it.
	provider := agentcore.NewFauxProvider(
		agentcore.AssistantText("42 beans"),
		agentcore.AssistantText("42 beans. STATUS: DONE"),
	)
	cfg := fullConfig()
	cfg.Extensions = nil
	cfg.Provider = provider
	cfg.Session = store
	cfg.SessionID = "s-crashed"
	cfg.ResumeSession = true
	// The resuming caller does not know the condition. That is the point.
	cfg.Goal = ""

	agent, err := agentcore.Build(preset.Full(cfg, preset.Options{
		Spill:              spill.NewMemorySpillStore(),
		ReportLogInvariant: func(observe.LogInvariantViolation) {},
	})...)
	if err != nil {
		t.Fatalf("Build(preset.Full): %v", err)
	}
	res, err := agent.Prompt(ctx, "continue")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.Recorded) < 2 {
		t.Fatalf("the recovered goal did not gate the run — %d provider calls, want the sentinel-less answer re-opened", len(provider.Recorded))
	}
	if !strings.Contains(res.Final, "STATUS: DONE") {
		t.Fatalf("the run stopped without meeting its recovered goal: %q", res.Final)
	}
}

// TestEjectRemovesEveryTrace is the "plug and eject" claim, checked rather than
// asserted: build the same agent with and without a capability, and the one
// without must carry no sign of it — no tool, no name in the extension list.
//
// This is the test that would fail if someone re-coupled the loop to a plugin
// by name, because the loop would keep doing the work with the plugin gone.
func TestEjectRemovesEveryTrace(t *testing.T) {
	cases := []struct {
		plugin agentcore.Plugin
		tool   string
	}{
		{spill.To(spill.NewMemorySpillStore()), "read_spill"},
		{jobs.Local(), "job_status"},
		{sessionquery.OwnSession(), "session_query"},
		{subagent.SelfOnly(), subagent.ToolSpawnSubagent},
	}
	for _, tc := range cases {
		t.Run(tc.plugin.Name(), func(t *testing.T) {
			cfg := fullConfig()
			cfg.Extensions = nil

			with, err := agentcore.Build(append(preset.Plugins(cfg), tc.plugin)...)
			if err != nil {
				t.Fatalf("Build with %s: %v", tc.plugin.Name(), err)
			}
			without, err := preset.New(cfg)
			if err != nil {
				t.Fatalf("Build without %s: %v", tc.plugin.Name(), err)
			}

			// The tool only exists at RUN time (extensions contribute per run),
			// so prove ejection through what the composition reports.
			withDump, withoutDump := with.Describe(), without.Describe()
			if !strings.Contains(withDump, tc.plugin.Name()) {
				t.Fatalf("%s did not register: %s", tc.plugin.Name(), withDump)
			}
			if strings.Contains(withoutDump, tc.plugin.Name()) {
				t.Fatalf("%s survived its own removal: %s", tc.plugin.Name(), withoutDump)
			}
			if strings.Contains(withoutDump, tc.tool) {
				t.Fatalf("%s is still advertised after ejecting %s", tc.tool, tc.plugin.Name())
			}
		})
	}
}

// TestReplaceSwapsOneSeam is the payoff: changing the control flow is a
// one-line edit to a plugin list, with every other plugin — including
// governance — untouched. The driver seam has no preset entry, so a custom
// driver is a plugin whose Register claims it via SetDriver.
func TestReplaceSwapsOneSeam(t *testing.T) {
	drv := &recordingDriver{answer: "from the custom driver"}
	list := preset.Replace(preset.Plugins(fullConfig()), driverPlugin{drv})

	agent, err := agentcore.Build(list...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "from the custom driver" {
		t.Fatalf("the replacement driver did not run: %q", res.Final)
	}
	// And the rest of the composition survived the swap.
	d := agent.Describe()
	if !describes(d, "goal", "ship it") {
		t.Fatalf("replacing the loop disturbed other plugins:\n%s", d)
	}
}

func TestWithoutDropsAPlugin(t *testing.T) {
	full := preset.Plugins(fullConfig())
	trimmed := preset.Without(full, "goal", "jobs")
	if len(trimmed) != len(full)-2 {
		t.Fatalf("Without removed %d plugins, want 2", len(full)-len(trimmed))
	}
	for _, p := range trimmed {
		if p.Name() == "goal" || p.Name() == "jobs" {
			t.Fatalf("%q survived removal", p.Name())
		}
	}
	// Dropping something already absent is not an error.
	if len(preset.Without(trimmed, "nonexistent")) != len(trimmed) {
		t.Fatal("Without must ignore names that are not present")
	}
}

// TestHandBuiltComposition shows the à-la-carte path: no Config at all, just
// the plugins an agent actually needs.
func TestHandBuiltComposition(t *testing.T) {
	agent, err := agentcore.Build(
		model.Plugin{Provider: &agentcore.FauxProvider{Responses: []agentcore.ChatResponse{
			{Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: "hello"}},
		}}, Model: "m"},
		tools.Of(&echoTool{name: "echo"}),
		policy.AllowList("echo"),
		goal.Until("STATUS appears"),
		repeatguard.Default(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := agent.Describe(); !describes(got, "tools", "echo") {
		t.Fatalf("hand-built composition lost its tool:\n%s", got)
	}
}

// TestPolicyDefaultsToDenyAll: a composition that forgets governance must not
// end up ungoverned.
func TestPolicyDefaultsToDenyAll(t *testing.T) {
	agent, err := agentcore.Build(
		model.Plugin{Provider: &agentcore.FauxProvider{}, Model: "m"},
		tools.Of(&echoTool{name: "echo"}),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(agent.Describe(), "DenyAll") {
		t.Fatalf("an agent with no policy plugin must deny by default:\n%s", agent.Describe())
	}
}

// describes reports whether Describe()'s line for key carries the given value,
// without depending on the column width Describe pads to.
func describes(dump, key, value string) bool {
	for _, ln := range strings.Split(dump, "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v) == value
		}
	}
	return false
}

// driverPlugin claims the driver seam — the two-line plugin a composition
// writes when it wants control flow other than agentcore's default.
type driverPlugin struct{ d agentcore.Driver }

func (driverPlugin) Name() string { return "loop" }

func (p driverPlugin) Register(r *agentcore.Registry) error { return r.SetDriver(p.d) }

// recordingDriver replaces the loop with a canned answer.
type recordingDriver struct{ answer string }

func (d *recordingDriver) Name() string { return "recording" }

func (d *recordingDriver) Drive(ctx context.Context, a *agentcore.Agent, msgs []agentcore.Message, task string, sink agentcore.StreamSink, emit func(agentcore.StreamEvent)) (agentcore.RunResult, error) {
	return agentcore.RunResult{Final: d.answer, Turns: 1}, nil
}

// extensionsLine returns the "extensions:" line of a Describe() dump.
func extensionsLine(desc string) string {
	for _, line := range strings.Split(desc, "\n") {
		if strings.HasPrefix(line, "extensions:") {
			return line
		}
	}
	return ""
}

// withoutExtensions drops the extensions line so the rest of a Describe() dump
// can be compared for seam parity.
func withoutExtensions(desc string) string {
	var keep []string
	for _, line := range strings.Split(desc, "\n") {
		if !strings.HasPrefix(line, "extensions:") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// TestEveryPluginSharesOneInterface is the uniformity check.
//
// The value of "everything is a plugin" is that a reader who has understood one
// plugin has understood them all: every package under agentcore/plugins exports
// a value with the SAME two methods, registers through the SAME Registry, and
// is composed by the SAME Build call. If a capability ever needed a bespoke
// entry point, this is where that shows up — the odd one out cannot be listed
// here.
//
// Extensions are additionally required to be ExtensionFactory, which is what
// makes the loop able to drive them without knowing their names.
func TestEveryPluginSharesOneInterface(t *testing.T) {
	// Every plugin in the default composition, as agentcore.Plugin. That this
	// slice type-checks at all is most of the assertion.
	all := preset.Plugins(fullConfig())
	if len(all) < 15 {
		t.Fatalf("only %d plugins in the default composition — the list is stale", len(all))
	}
	for _, p := range all {
		if p.Name() == "" {
			t.Fatalf("plugin %T has no name", p)
		}
	}

	// The ejectable ones must ALSO satisfy ExtensionFactory: BeginRun is what
	// the loop calls, and a capability that only implemented Register would be
	// composed and then never driven.
	extensions := []agentcore.Plugin{
		goal.Until("x"),
		finishguard.Of(func(context.Context, finishguard.State) string { return "" }),
		repeatguard.At(3),
		spill.To(spill.NewMemorySpillStore()),
		jobs.Local(),
		sessionquery.OwnSession(),
		subagent.SelfOnly(),
		observe.LogInvariant{},
	}
	for _, p := range extensions {
		f, ok := p.(agentcore.ExtensionFactory)
		if !ok {
			t.Fatalf("%q is registered as an extension but does not implement ExtensionFactory", p.Name())
		}
		// BeginRun must be callable on a bare run and must never panic: an
		// extension that cannot serve declines, it does not fail the run.
		ext, err := f.BeginRun(context.Background(), agentcore.RunInfo{Owner: "run_1"})
		if err != nil {
			t.Fatalf("%q failed BeginRun on a minimal run: %v", p.Name(), err)
		}
		if ext != nil && ext.Name() == "" {
			t.Fatalf("%q returned an unnamed extension", p.Name())
		}
	}
}

// TestOptInPluginsShareTheSameInterface covers the capabilities that are NOT in
// the default preset — the ones a composition adds deliberately (a run plan,
// telemetry, cost accounting, retrieval through a caller's own index, an
// explicit no-tools stance).
//
// They need their own test precisely because preset.Plugins never names them:
// without this, the only thing holding todo.Plugin, observe.Hooks and friends
// to the plugin contract is that they happen to compile, and plugins/README.md's
// claim that every capability is the same two-method value would be unchecked
// for exactly the half of the surface that lives outside the default agent.
func TestOptInPluginsShareTheSameInterface(t *testing.T) {
	optIn := []agentcore.Plugin{
		todo.With(todo.NewStore()),
		observe.Hooks{Label: "audit", OnAgentEnd: []agentcore.AgentEndHook{func(context.Context, agentcore.RunResult) {}}},
		observe.Monitor{Sink: observe.SinkFunc(func(observe.TraceRecord) {})},
		policy.DenyAll(),
		sessionquery.Via(stubQuery{}),
	}
	for _, p := range optIn {
		if p.Name() == "" {
			t.Fatalf("plugin %T has no name", p)
		}
	}

	// Composed together they must produce one working agent: same Registry, same
	// Build call, no bespoke entry point anywhere.
	base := []agentcore.Plugin{
		model.Plugin{Provider: &agentcore.FauxProvider{}, Model: "m"},
	}
	reg, err := agentcore.BuildRegistry(append(base, optIn...)...)
	if err != nil {
		t.Fatalf("BuildRegistry with the opt-in plugins: %v", err)
	}
	// Each one leaves the trace its KIND is supposed to leave: todo a tool,
	// monitor a provider decorator, session_query an extension, observe a
	// listener. Read off the REGISTRY dump, which is the only one that reports
	// contributions (a provider decorator is invisible once composed).
	regDump := reg.Describe()
	if !strings.Contains(regDump, todo.ToolName) {
		t.Fatalf("todo did not contribute its tool:\n%s", regDump)
	}
	if !strings.Contains(regDump, "provider_wrappers: 1") {
		t.Fatalf("monitor did not contribute its provider decorator:\n%s", regDump)
	}
	if !strings.Contains(extensionsLine(regDump), "session_query") {
		t.Fatalf("session_query did not install its extension:\n%s", regDump)
	}
	if strings.Contains(regDump, "agent_end=0") {
		t.Fatalf("observe.Hooks did not register its listeners:\n%s", regDump)
	}

	agent, err := reg.Agent()
	if err != nil {
		t.Fatalf("Agent from the opt-in composition: %v", err)
	}
	if dump := agent.Describe(); !describes(dump, "policy", "agentcore.DenyAll") {
		t.Fatalf("policy.DenyAll did not claim the policy seam:\n%s", dump)
	}
}

// stubQuery is a caller-supplied retrieval backend, the thing sessionquery.Via
// exists to accept.
type stubQuery struct{}

func (stubQuery) Search(context.Context, sessionquery.SessionQueryRequest) (sessionquery.SessionQueryResult, error) {
	return sessionquery.SessionQueryResult{}, nil
}

// TestSeamsAreNotExtensions guards the split the other way: a capability the
// loop ALWAYS uses is configured through a seam and must not also be pretending
// to be an ejectable extension. Registering both ways would mean ejecting the
// plugin changed behaviour while the loop still ran the code — the exact
// confusion the seam/extension distinction exists to prevent.
func TestSeamsAreNotExtensions(t *testing.T) {
	seams := []agentcore.Plugin{
		driverPlugin{agentcore.DefaultDriver()},
		policy.AllowList("echo"),
		compaction.Plugin{},
		tools.Of(&echoTool{name: "echo"}),
	}
	for _, p := range seams {
		if _, ok := p.(agentcore.ExtensionFactory); ok {
			t.Fatalf("%q is a seam plugin but also implements ExtensionFactory", p.Name())
		}
	}
}
