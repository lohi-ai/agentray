package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/advisor"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// advisor.go — the reviewer half of agentcore/plugins/advisor: one model call
// that reads a finished run and decides whether the answer is fit to hand over.
//
// It is deliberately shaped like reflect.go — a one-shot provider call at its
// own tier, no tools, JSON out — and for the same reason: a pass that reviews
// the run must not be able to change it. The difference is when. Reflection
// runs after the run is over and writes memory; the advisor runs at the moment
// the run tries to end, which is the last point at which "that figure does not
// follow from the query you ran" is still actionable.
//
// The reviewer gets no tools in this first pass. omp's advisor is a full agent
// with read/grep/glob; the agentray analog would be a nested run holding this
// project's read tools, with its own scope gating, credentials and budget — a
// second governed agent, not a config change. It buys less here than it does
// there: an analytics agent's evidence is the SQL it ran and the rows it got
// back, and both are already in the transcript the reviewer reads. Granting
// read tools is the documented follow-on, not a gap in this.

// advisorMaxTokens caps the reviewer's output. Notes are terse by contract; a
// reviewer writing an essay has already failed the "concrete, terse,
// actionable" instruction, and the emission guard would clamp it anyway.
const advisorMaxTokens = 1024

// advisorTranscriptBudget bounds what the reviewer is shown, in bytes of
// rendered transcript. A long run can carry hundreds of KB of tool output, and
// the reviewer's job does not need all of it: the task at the top and the
// recent work at the bottom are where a wrong turn is visible. The middle is
// elided with a marker, so the reviewer knows it is looking at a window rather
// than assuming a step it cannot see never happened.
const advisorTranscriptBudget = 60_000

// advisorInput is everything one agent's reviewer needs.
type advisorInput struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	// Instructions are the operator's review priorities (storage.AgentAdvisor).
	// They reach the REVIEWER only — never the agent under review.
	Instructions string
}

// advisorNotes is the reviewer's structured answer. A run that went fine
// returns an empty list, and that is the expected outcome.
type advisorNotes struct {
	Notes []struct {
		Text     string `json:"text"`
		Severity string `json:"severity"`
	} `json:"notes"`
}

// advisorReviewer builds the plugin's Reviewer over a provider call.
//
// Every failure path returns an error, which the plugin treats as "accept the
// finish". That is the contract that makes the advisor safe to turn on: a
// provider outage, a timeout, or a model that answers in prose instead of JSON
// must leave the run's answer exactly as it would have been with the advisor
// switched off.
func (r *Runner) advisorReviewer(in advisorInput) advisor.Reviewer {
	return func(ctx context.Context, rev advisor.Review) ([]advisor.Note, error) {
		provider, err := buildTracedProvider(in.Provider, in.BaseURL, in.APIKey, r.Tracer)
		if err != nil {
			return nil, err
		}
		resp, err := provider.Chat(ctx, agentcore.ChatRequest{
			Model:     in.Model,
			MaxTokens: advisorMaxTokens,
			Messages: []agentcore.Message{
				{Role: agentcore.RoleSystem, Content: advisorSystemPrompt(in.Instructions)},
				{Role: agentcore.RoleUser, Content: advisorUserPrompt(rev)},
			},
		})
		if err != nil {
			return nil, err
		}
		var out advisorNotes
		if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
			return nil, fmt.Errorf("advisor: unparseable review: %w", err)
		}
		notes := make([]advisor.Note, 0, len(out.Notes))
		for _, n := range out.Notes {
			if strings.TrimSpace(n.Text) == "" {
				continue
			}
			notes = append(notes, advisor.Note{Text: n.Text, Severity: advisorSeverity(n.Severity)})
		}
		return notes, nil
	}
}

// advisorSeverity maps the model's string to a severity, defaulting to nit.
// The default matters: an unrecognized value must fall to the severity that
// does NOT re-open the run, so a reviewer that invents a level cannot buy
// itself a turn with it.
func advisorSeverity(s string) advisor.Severity {
	switch advisor.Severity(strings.ToLower(strings.TrimSpace(s))) {
	case advisor.SeverityBlocker:
		return advisor.SeverityBlocker
	case advisor.SeverityConcern:
		return advisor.SeverityConcern
	default:
		return advisor.SeverityNit
	}
}

// advisorSystemPrompt is the reviewer's contract, with the operator's review
// priorities appended.
//
// Most of it is prohibitions, and that is not padding. A reviewer model's
// default failure is not missing bugs — it is finding something to say. Left
// unconstrained it advises the agent to "confirm scope with the user", to
// "consider backwards compatibility", to "add error handling", on every run,
// forever; the operator turns the feature off inside a day. The rules that
// earn their tokens are the ones that make silence the default and force a
// note to name a concrete, transcript-evident risk.
//
// Adapted from oh-my-pi's advisor system prompt.
func advisorSystemPrompt(instructions string) string {
	var b strings.Builder
	b.WriteString(advisorBasePrompt)
	if s := strings.TrimSpace(instructions); s != "" {
		// Fenced so operator prose cannot be read as a new rule block. It is
		// review priorities, not an amendment to the contract above.
		b.WriteString("\n\nThis project also asks you to pay attention to:\n<attention>\n")
		b.WriteString(s)
		b.WriteString("\n</attention>")
	}
	return b.String()
}

const advisorBasePrompt = `You are the advisor for an analytics agent. The agent has finished a piece of
work and is about to hand its answer to the user. You read what it actually did
and decide whether that answer is fit to hand over.

You do not act. You have no tools. You cannot edit, re-run, or query anything —
everything you know comes from the transcript below.

Answer with JSON only:
{"notes":[{"text":"<concrete, terse, actionable>","severity":"nit|concern|blocker"}]}

SILENCE IS THE DEFAULT. A run that went fine gets {"notes":[]}. You are not
scored on finding something. Returning nothing is the correct, common answer.

Severity decides what happens:
- "nit"     — recorded for the operator; the answer ships as-is. Cleanup, a
              simplification, a low-risk edge case.
- "concern" — the agent is sent back to weigh it before answering. Use for a
              material risk: a figure that does not follow from the query that
              was run, a wrong code path, a constraint in the task that the
              answer ignores, a claim with no evidence behind it in this run.
- "blocker" — the agent is sent back and the work is not fit to hand over.
              Use ONLY for: a claim of completion over scope that was sampled
              or dropped; an answer contradicting an explicit instruction in
              the task; a stub or mock standing in for the real result; a
              number contradicted by the tool output in this same transcript.

NEVER:
- Never restate something the agent already saw. A failed query, an error it
  already read, a tool result already in the transcript: it knows.
- Never repeat advice already listed as previously given. If you still object
  after the agent answered it, you must say something NEW or escalate the
  severity — a verbatim repeat is discarded unread.
- Never tell the agent to confirm scope, ask the user for clarification,
  summarize the request, or narrate its plan. Intent belongs to the agent.
- Never raise backwards compatibility, deprecation shims, or migration paths
  unless the task explicitly asked for them.
- Never object to the size or ambition of the work. A large answer is not a
  problem. Object only when an explicit instruction was breached — and cite it.
- Never assert a value you cannot see. Truncated or elided content is UNKNOWN;
  say what is observably true and name what should be checked.
- Never raise generic unease, style preference, or "consider also…". If you
  cannot name the concrete risk and where in the transcript it is visible, the
  answer is silence.

Cite only what is in the transcript. At most 3 notes; if you have more, keep
the ones that would change what the user is told.`

// advisorUserPrompt renders the run for review: the task, a bounded window of
// the transcript, the tool trace, the answer under review, and — on a second
// round — the notes the agent has already been given.
func advisorUserPrompt(rev advisor.Review) string {
	var b strings.Builder

	if rev.Round > 0 {
		fmt.Fprintf(&b, `This is review round %d. You already gave the agent the notes listed under
"Already raised" below, and the answer under review is what it produced in
response. Judge whether it dealt with them. Repeating one of those notes
verbatim is discarded — say something new, escalate, or return {"notes":[]}.

`, rev.Round+1)
	}

	b.WriteString("## Transcript\n\n")
	b.WriteString(renderAdvisorTranscript(rev.Messages, advisorTranscriptBudget))

	if len(rev.Tools) > 0 {
		fmt.Fprintf(&b, "\n\n## Tool trace (%d calls)\n\n", len(rev.Tools))
		for _, t := range rev.Tools {
			switch {
			case !t.Allowed:
				fmt.Fprintf(&b, "- %s: BLOCKED (%s)\n", t.Tool, t.Reason)
			case t.Error != "":
				fmt.Fprintf(&b, "- %s: ERROR %s\n", t.Tool, clip(t.Error, 200))
			default:
				fmt.Fprintf(&b, "- %s: ok\n", t.Tool)
			}
		}
	}

	if len(rev.Delivered) > 0 {
		b.WriteString("\n\n## Already raised (do not repeat verbatim)\n\n")
		for _, n := range rev.Delivered {
			fmt.Fprintf(&b, "- [%s] %s\n", n.Severity, n.Text)
		}
	}

	fmt.Fprintf(&b, "\n\n## The answer under review (turn %d)\n\n%s\n", rev.Turns, clip(rev.Final, 20_000))
	b.WriteString("\nReturn JSON only.")
	return b.String()
}

// renderAdvisorTranscript renders the conversation for the reviewer, keeping
// the head and the tail when it does not fit.
//
// Head and tail rather than a plain truncation because they answer different
// questions and the reviewer needs both: the head is what was ASKED (drift is
// only visible against it) and the tail is what was most recently DONE (that is
// where a wrong turn is). Dropping the head to keep more tail is the version of
// this that fails silently — the reviewer then judges the work against a task
// it is guessing at.
func renderAdvisorTranscript(msgs []agentcore.Message, budget int) string {
	blocks := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if s := renderAdvisorMessage(m); s != "" {
			blocks = append(blocks, s)
		}
	}
	if len(blocks) == 0 {
		return "(no transcript captured)"
	}

	total := 0
	for _, s := range blocks {
		total += len(s) + 2
	}
	if total <= budget {
		return strings.Join(blocks, "\n\n")
	}

	// A third to the head, the rest to the tail: the task is short, the work is
	// not.
	headBudget, tailBudget := budget/3, budget-budget/3
	head, used := 0, 0
	for head < len(blocks) && used+len(blocks[head]) <= headBudget {
		used += len(blocks[head]) + 2
		head++
	}
	tail, used := len(blocks), 0
	for tail > head && used+len(blocks[tail-1]) <= tailBudget {
		used += len(blocks[tail-1]) + 2
		tail--
	}
	if head == 0 && tail == len(blocks) {
		// One block larger than the whole budget: show its head rather than
		// eliding the entire transcript.
		return clip(blocks[0], budget) + "\n\n[… truncated]"
	}
	out := append([]string{}, blocks[:head]...)
	// Only claim an elision when there was one. A "0 steps elided" marker is a
	// lie in the reviewer's prompt, and the marker exists precisely to be
	// truthful about what the window left out.
	if tail > head {
		out = append(out, fmt.Sprintf("[… %d steps elided — this is a window on the run, not the whole of it …]", tail-head))
	}
	out = append(out, blocks[tail:]...)
	return strings.Join(out, "\n\n")
}

// clip truncates to at most max BYTES without splitting a multi-byte rune.
//
// The shared truncate() cuts on a byte index, which for this prompt is a real
// problem rather than a tidiness one: the transcripts are Vietnamese, so a cut
// lands mid-rune routinely, and the replacement characters that survive into
// the request are noise the reviewer has to read past on every long run.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// renderAdvisorMessage renders one message with its tool intent, which is the
// part a reviewer needs most: WHICH query was run matters more than that some
// query was.
func renderAdvisorMessage(m agentcore.Message) string {
	switch m.Role {
	case agentcore.RoleSystem:
		// The agent's own persona and skills are not under review, and they are
		// the largest fixed block in the conversation.
		return ""
	case agentcore.RoleUser:
		label := "user"
		if !m.Directive {
			// A framework-synthesized user message — a goal nudge, a budget
			// wrap-up, a previous advisory. Marked so the reviewer does not read
			// it as something the human asked for.
			label = "system-injected"
		}
		return "### " + label + "\n" + clip(m.Content, 8000)
	case agentcore.RoleAssistant:
		var b strings.Builder
		b.WriteString("### agent\n")
		if s := strings.TrimSpace(m.Content); s != "" {
			b.WriteString(clip(s, 8000))
			b.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "→ calls %s(%s)\n", tc.Name, clip(tc.Arguments, 2000))
		}
		return strings.TrimRight(b.String(), "\n")
	case agentcore.RoleTool:
		name := m.Name
		if name == "" {
			name = "tool"
		}
		return fmt.Sprintf("### %s result\n%s", name, clip(m.Content, 4000))
	default:
		return ""
	}
}

// advisorNoteRecorder persists what the advisor said about a run.
//
// Every note is recorded, not just the ones that re-opened the run: a nit
// never reaches the model, so this is the only place it exists. The Delivered
// flag preserves the distinction the operator actually needs — "the reviewer
// mentioned it" versus "the agent was made to answer for it".
//
// Best-effort. A review that cannot be written down must not fail the run it
// reviewed, and must not block the loop that is trying to finish.
func (r *Runner) advisorNoteRecorder(runID string) func(context.Context, []advisor.Note, bool) {
	return func(ctx context.Context, notes []advisor.Note, delivered bool) {
		if r.Store == nil || len(notes) == 0 {
			return
		}
		rows := make([]storage.AgentAdvisorNote, 0, len(notes))
		for _, n := range notes {
			rows = append(rows, storage.AgentAdvisorNote{
				Text:      n.Text,
				Severity:  string(n.Severity),
				Delivered: delivered,
			})
		}
		_ = r.Store.RecordAgentRunAdvisorNotes(ctx, runID, rows)
	}
}
