package agentruntime

import (
	"strings"

	goalgate "github.com/lohi-ai/agentray/agentcore/plugins/goal"
)

// chatcmd.go — the chat surface's slash commands.
//
// agentcore already carries the capabilities (a goal gate, a live run plan,
// sub-agents, compaction, skills); what was missing was a way for a *person* to
// reach them from the composer and to see the state they produce. That is what a
// command is: a directive typed at the head of a message, parsed HERE — on the
// server — rather than in the client.
//
// Server-side parsing is not an implementation detail, it is the contract. The
// conversation store carries one `message` string per turn (design §5), so a
// command typed on one machine has to mean the same thing when the thread is
// reloaded on another, when it arrives through the API with no browser involved,
// and when the turn is replayed out of the durable log. A client-side
// interpretation would be a second, divergent implementation of the protocol —
// exactly the split the conversation store exists to remove. The client's job is
// the affordance (the `/` menu, the rendering); the meaning lives here.
//
// A command either handles the turn on its own (compact, clear, help, plan,
// agents — no model run, no spend) or configures the run that follows (goal).

// Command names, exactly as typed. Kept as constants because three layers key on
// them: the parser, the dispatch switch in chat.go, and the catalog the client
// renders in its `/` menu.
const (
	CmdGoal    = "goal"
	CmdCompact = "compact"
	CmdClear   = "clear"
	CmdPlan    = "plan"
	CmdAgents  = "agents"
	CmdHelp    = "help"
)

// ChatCommand describes one command for the client's `/` menu. It is served by
// GET /api/agent/commands so the composer's affordances and the server's parser
// can never drift into two different vocabularies.
type ChatCommand struct {
	Name string `json:"name"`
	// Arg is the placeholder shown after the command in the menu ("<condition>"),
	// empty for a command that takes none. It also tells the client whether to
	// leave the caret after the inserted token so the user can keep typing.
	Arg string `json:"arg,omitempty"`
	// Summary is one line of plain language — what the command does, from the
	// user's side, not the mechanism.
	Summary string `json:"summary"`
	// Handled marks a command the server answers itself, with no agent run and no
	// model spend. The client shows these as instant, not as a question.
	Handled bool `json:"handled"`
}

// ChatCommands is the catalog. Order is the order the menu shows.
//
// Deliberately small. Every entry is a capability the runtime already has and a
// person has a reason to reach directly; a command that merely renames something
// the agent does on its own would be one more thing to learn for nothing.
var ChatCommands = []ChatCommand{
	{Name: CmdGoal, Arg: "<condition>", Summary: "Keep working until a condition is met (the agent may not stop before it)"},
	{Name: CmdPlan, Summary: "Show the agent's current plan for this thread", Handled: true},
	{Name: CmdCompact, Summary: "Summarize the older messages now to free up context", Handled: true},
	{Name: CmdClear, Summary: "Start fresh from here — the agent forgets what came before (the transcript stays)", Handled: true},
	{Name: CmdAgents, Summary: "List the teammates this agent can hand work to", Handled: true},
	{Name: CmdHelp, Summary: "List the commands and skills you can use here", Handled: true},
}

// Directive is a parsed leading slash command. Name is empty when the message
// carries none, which is the overwhelmingly common case.
type Directive struct {
	// Name is the command without its slash ("goal"), lowercased.
	Name string
	// Arg is the rest of the command's first line ("all 172 tests pass"),
	// trimmed, with surrounding quotes removed.
	Arg string
	// Rest is the message with the directive line removed — the actual task,
	// when the user typed one under the command.
	Rest string
}

// parseDirective splits a leading "/name [arg]" line off a chat message.
//
// Only the FIRST line is considered, and only when it starts the message: a
// slash deeper in the prose is prose (a path, a fraction, a date), and treating
// it as a command would silently swallow user text. An unknown "/word" is left
// alone for the same reason — the agent sees the message exactly as typed.
func parseDirective(message string) Directive {
	trimmed := strings.TrimSpace(message)
	if !strings.HasPrefix(trimmed, "/") {
		return Directive{Rest: message}
	}
	line, rest, _ := strings.Cut(trimmed, "\n")
	name, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	name = strings.ToLower(strings.TrimPrefix(name, "/"))
	if !isCommand(name) {
		return Directive{Rest: message} // "/goals", "/usr/local", "1/2" — not ours
	}
	return Directive{
		Name: name,
		Arg:  strings.Trim(strings.TrimSpace(arg), `"'`),
		Rest: strings.TrimSpace(rest),
	}
}

// isCommand reports whether name is in the catalog.
func isCommand(name string) bool {
	for _, c := range ChatCommands {
		if c.Name == name {
			return true
		}
	}
	return false
}

// IsHandledCommand reports whether a message is a command the server answers
// itself. The HTTP layer needs this before it decides what a message IS: a second
// message arriving while a run is live is normally an amendment to that run and
// gets injected into it, but "compact this thread" is an instruction to the
// SYSTEM, not something to say to the agent. Steering it would hand the model the
// literal text "/compact" as a mid-run correction and quietly never compact
// anything.
func IsHandledCommand(message string) bool {
	d := parseDirective(message)
	if d.Name == "" {
		return false
	}
	for _, c := range ChatCommands {
		if c.Name == d.Name {
			return c.Handled
		}
	}
	return false
}

// goalTurn resolves a /goal directive into the run's completion condition and the
// prompt that starts it. A directive with no task keeps the condition as the
// prompt, so `/goal all 172 tests pass` alone still starts a gated run.
//
// Callers must have rejected a bare `/goal` (empty Arg) before calling: a gate
// with no condition is nothing to satisfy, and opening a run that claims to be
// gated when it isn't is worse than treating the line as plain text.
func goalTurn(d Directive) (goal, prompt string) {
	if d.Rest == "" {
		return d.Arg, "Work toward this goal until it is satisfied: " + d.Arg
	}
	return d.Arg, d.Rest
}

// stripGoalSentinel removes the goal gate's closing status line from an answer.
//
// The gate asks the agent to end with `STATUS: DONE` (or `STATUS: BLOCKED`) as a
// machine-readable signal that the run may stop. In a terminal that reads as a
// footer; in a product chat it is a leaked protocol token, and — because the
// answer is persisted — it comes back on reload and is replayed to the model as
// its own prior words.
//
// Only the LAST non-empty line is considered, and only when that line is nothing
// BUT the sentinel once markdown decoration is peeled off. The gate itself is
// looser — a `Contains` on the last line — but the gate is deciding whether to
// let a run stop, where a false positive costs one early finish. Here a false
// positive silently deletes a sentence the user was meant to read ("I can't write
// STATUS: DONE yet — two tests fail"), so this side errs the other way: worst
// case an oddly-formatted answer keeps a visible marker.
//
// A blocked run loses only the marker; the blocker is stated in the prose above
// it, which is what the reader needs.
func stripGoalSentinel(final string) string {
	lines := strings.Split(final, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		bare := strings.ToUpper(strings.Trim(line, "*_`# \t"))
		if bare != goalgate.Done && bare != goalgate.Blocked {
			return final
		}
		// Drop the sentinel line and any blank lines that were separating it.
		return strings.TrimRight(strings.Join(lines[:i], "\n"), " \t\n")
	}
	return final
}

// helpText is the /help reply. Written as the answer to "what can I type here",
// not as a reference table — the reader is in a chat box, not a manual.
func helpText(skills []string) string {
	var b strings.Builder
	b.WriteString("Here's what you can type in this box.\n\n**Commands**\n\n")
	for _, c := range ChatCommands {
		b.WriteString("- `/" + c.Name)
		if c.Arg != "" {
			b.WriteString(" " + c.Arg)
		}
		b.WriteString("` — " + c.Summary + "\n")
	}
	if len(skills) > 0 {
		b.WriteString("\n**Skills** — type `/` and pick one to have me follow it for a turn.\n\n")
		for _, s := range skills {
			b.WriteString("- " + s + "\n")
		}
	}
	b.WriteString("\nEverything else is just a question — ask it in your own words.")
	return b.String()
}
