package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
)

// chatcmdrun.go — executing the slash commands parsed in chatcmd.go, plus the
// one piece of run state a person could not see before: the live plan.
//
// Every handled command answers the turn on its own. That is the point: a person
// asking to compact a thread, clear its context, or read back the plan should not
// pay for a model call, wait on a tool loop, or risk the agent deciding to do
// something else instead. The reply is streamed like any other so the chat
// surface needs no special case for it, and it is persisted like any other so a
// reload shows what happened.

// runCommand handles a directive the server answers itself. It streams the reply,
// persists it, and returns a routeCommand result — no run row, no model spend
// (except /compact's one cheap-tier summary call, which is the work being asked
// for).
func (s *ChatService) runCommand(ctx context.Context, opts ChatOptions, d Directive, sink agentcore.StreamSink) (ChatResult, error) {
	var reply string
	switch d.Name {
	case CmdCompact:
		// The only command that isn't instant: summarizing the thread is a real
		// model call, ~20s on a long chat. Without a beat the stream sits silent
		// after "Reading your message…" and reads as a hang.
		if sink != nil {
			sink(agentcore.StreamEvent{Type: agentcore.StreamProgress, Note: "Summarizing the earlier messages…"})
		}
		reply = s.cmdCompact(ctx, opts)
	case CmdClear:
		reply = s.cmdClear(ctx, opts)
	case CmdPlan:
		reply = s.cmdPlan(ctx, opts)
	case CmdAgents:
		reply = s.cmdAgents(ctx, opts)
	case CmdHelp:
		reply = s.cmdHelp(ctx, opts)
	default:
		// Unreachable via parseDirective (which only recognizes catalog names), but
		// a command added to the catalog and not to this switch must not silently
		// do nothing.
		reply = "I don't know the command `/" + d.Name + "` yet."
	}
	streamText(reply, sink)
	// Persisted as a command turn: visible in the transcript, invisible to the
	// model. The reply is the server talking about the thread, not the agent
	// answering a question, and replaying it as history is how a run ends up
	// echoing "Cleared. I've forgotten everything above this line" as its answer.
	if opts.ConversationID != "" && s.runner != nil && s.runner.Store != nil {
		_, _ = AppendCommandEntry(context.WithoutCancel(ctx), s.runner.Store, opts.ConversationID,
			string(agentcore.RoleAssistant), reply, opts.AgentID, "")
	}
	return ChatResult{Route: routeCommand, Final: reply}, nil
}

// cmdCompact folds the older half of the thread into a running summary now,
// rather than waiting for the automatic trigger. The reply says which of the two
// things happened, because "nothing to compact" and "compacted" look identical
// from the outside otherwise.
func (s *ChatService) cmdCompact(ctx context.Context, opts ChatOptions) string {
	if opts.ConversationID == "" || s.runner == nil || s.runner.Store == nil {
		return "This chat isn't saved on the server yet, so there's nothing to compact. Send a message first."
	}
	wctx := context.WithoutCancel(ctx)
	done, err := CompactConversationNow(wctx, s.runner.Store, opts.ConversationID,
		s.runner.RunTierWindow(wctx, opts.ProjectID), s.summarizer(opts.ProjectID))
	switch {
	case err != nil:
		return "I couldn't compact this chat just now. Try again in a moment."
	case !done:
		// Covers both no-op cases — a thread too short to have anything above the
		// newest turn, and one compacted a moment ago with nothing new since.
		return "Nothing new to compact — I'm already holding this chat as tightly as I can."
	}
	return "Compacted. I've summarized the earlier part of this chat and kept the most recent turn in full — I still remember what we decided, with room to keep going."
}

// cmdClear drops everything above this point out of the agent's context without
// deleting it. That distinction is the whole feature, so the reply states it.
func (s *ChatService) cmdClear(ctx context.Context, opts ChatOptions) string {
	if opts.ConversationID == "" || s.runner == nil || s.runner.Store == nil {
		return "This chat isn't saved on the server yet, so there's nothing to clear."
	}
	if _, err := AppendClearEntry(context.WithoutCancel(ctx), s.runner.Store, opts.ConversationID, opts.AgentID); err != nil {
		return "I couldn't clear the context just now. Try again in a moment."
	}
	return "Cleared. I've forgotten everything above this line — the messages are still here for you to read, but I'm starting fresh from your next one."
}

// cmdPlan reads the agent's last plan back out of the log. The run's own plan
// store is per-run and gone once the run ends, so between turns the log is the
// only place the plan still exists.
func (s *ChatService) cmdPlan(ctx context.Context, opts ChatOptions) string {
	if opts.ConversationID == "" || s.runner == nil || s.runner.Store == nil {
		return "No plan yet — I write one when a task has enough steps to be worth tracking."
	}
	items, ok, err := LatestPlan(ctx, s.runner.Store, opts.ConversationID)
	if err != nil || !ok || len(items) == 0 {
		return "No plan yet — I write one when a task has enough steps to be worth tracking."
	}
	left := 0
	for _, it := range items {
		if it.Status != todo.StatusCompleted {
			left++
		}
	}
	head := fmt.Sprintf("Here's my plan — %d of %d steps still open.\n\n", left, len(items))
	return head + RenderPlan(items)
}

// cmdAgents lists the teammates this agent may hand work to. It answers the
// question the spawn_subagent tool raises the first time a user sees it in the
// work log: who else is in here.
func (s *ChatService) cmdAgents(ctx context.Context, opts ChatOptions) string {
	if s.runner == nil || s.runner.Store == nil {
		return "I can't reach the team roster right now."
	}
	scopeID, err := s.runner.Store.AgentScopeForRun(ctx, opts.ProjectID, opts.AgentID)
	if err != nil {
		return "I can't reach the team roster right now."
	}
	targets, err := s.runner.Store.AgentDelegatesForRun(ctx, scopeID)
	if err != nil {
		return "I can't reach the team roster right now."
	}
	var b strings.Builder
	if len(targets) == 0 {
		b.WriteString("No teammates are granted to me yet — I work on my own here, and I can still split a big job across temporary sub-agents when it helps.\n\n")
		b.WriteString("You can grant me a teammate in Team → Teammates.")
		return b.String()
	}
	b.WriteString("I can hand work to these teammates. Each runs under its own persona, tools, and permissions — not mine.\n\n")
	for _, t := range targets {
		b.WriteString("- **" + t.Name + "**")
		if desc := strings.TrimSpace(t.Description); desc != "" {
			b.WriteString(" — " + desc)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nJust describe the job — I'll pick who it goes to, and you'll see the hand-off in the work log.")
	return b.String()
}

// cmdHelp lists the commands and the agent's usable skills. The skills come from
// the same source the system prompt is built from, so the menu can't advertise
// one the runtime wouldn't load.
func (s *ChatService) cmdHelp(ctx context.Context, opts ChatOptions) string {
	names := []string{}
	if s.runner != nil && s.runner.Store != nil {
		if scopeID, err := s.runner.Store.AgentScopeForRun(ctx, opts.ProjectID, opts.AgentID); err == nil {
			if skills, err := s.runner.loadSkills(ctx, scopeID); err == nil {
				for _, sk := range skills {
					if sk.Enabled {
						names = append(names, sk.Name)
					}
				}
				sort.Strings(names)
			}
		}
	}
	return helpText(names)
}

// mirrorPlan turns a completed update_plan call into a plan revision: the live
// callback the streaming client renders, and a durable entry a reload or a second
// machine reads. Only a call that actually landed counts — a rejected one (two
// items in progress, empty content) left the agent's plan unchanged, and showing
// its arguments would put a plan on screen that the agent does not have.
func (s *ChatService) mirrorPlan(ctx context.Context, req chatWork, runID string, t agentcore.ToolTrace, turn int) {
	if t.Tool != todo.ToolName || !t.Allowed || t.Error != "" {
		return
	}
	var args struct {
		Items []PlanItem `json:"items"`
	}
	if json.Unmarshal([]byte(t.Args), &args) != nil || len(args.Items) == 0 {
		return
	}
	if req.OnPlan != nil {
		req.OnPlan(args.Items)
	}
	if req.ConversationID != "" && s.runner != nil && s.runner.Store != nil {
		_, _ = AppendPlanEntry(context.WithoutCancel(ctx), s.runner.Store, req.ConversationID, req.AgentID, runID, args.Items, turn)
	}
}
