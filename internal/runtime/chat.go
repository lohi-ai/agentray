package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/2found/2ai/agentcore"
	goalplugin "github.com/2found/2ai/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// Every conversational turn is owned by the native agent, including greetings
// and follow-ups. Slash commands are the only host-handled turns.
const (
	routeData    = "data"
	routeCommand = "command"
)

// ChatOptions parameterize one chat turn. Conversation routes supply history
// derived from the durable native branch. Display-only history is rejected.
type ChatOptions struct {
	ProjectID string
	// AgentID selects which of the project's agents handles the turn (AgentGarden
	// §3). Empty targets the project's default agent, preserving the single-agent
	// path. It is threaded verbatim into the run.
	AgentID   string
	Message   string
	History   []agentcore.Message
	PiHistory *PiConversationHistory
	InputID   string
	// SessionID is the client-held conversation id. When set, the in-flight run
	// registers under it (via the Runner's LiveRegistry) so a sibling request can
	// steer or follow-up the run. Empty disables live control for the turn.
	SessionID string
	// ConversationID, when set, makes the turn durable in the conversation store
	// (DESIGN-CONVERSATION-STORE.md): the route appends the user message and derives
	// PiHistory server-side before calling Chat, and Chat appends the assistant turn as
	// a message entry when it finishes — so the thread survives on the server and a
	// second machine/user can load and continue it. Empty starts a stateless turn.
	// Distinct from SessionID (live-control key), though a
	// caller typically sets both to the same conversation id.
	ConversationID string
	// ReadOnly withholds the agent's writing tools for this turn (see
	// RunOptions.ReadOnly). The HTTP layer sets it when the selected caller may
	// read but not write (for example a demo viewer or restricted credential).
	ReadOnly bool
	// OnRunID, when set, is called with the run id as soon as the run row opens —
	// before any token — so a streaming caller can surface it to the client (which
	// persists it to reattach to the run after navigating away mid-stream).
	OnRunID func(string)
	// OnPlan, when set, is called with each revision of the agent's live todo list
	// as it is written (the todo plugin's update_plan). The plan is out-of-band by
	// design — it never enters the transcript — so this callback is the only way a
	// watching client sees it change in real time. Every revision is also mirrored
	// into the conversation log, which is what a reload or a second machine reads.
	OnPlan func([]PlanItem)
	// OnGoal, when set, is called once with a /goal turn's completion condition,
	// before the run starts, so the client can pin it above the thread while the
	// agent works toward it.
	OnGoal func(string)
}

// AnswerOptions parameterize answering a parked ask tool call.
type AnswerOptions struct {
	UserID         string
	ProjectID      string
	SessionID      string // client-held conversation id
	CallID         string // tool call id being answered (optional if single pending)
	Answer         string
	ConversationID string
	ReadOnly       bool
	OnPlan         func([]PlanItem)
	OnGoal         func(string)
}

// ChatResult is the outcome of one chat turn, shaped to the chat JSON/SSE
// contract (run_id/final/route/tool_calls/usage/turns + the additive card).
// RunID is empty for host-handled slash commands, which never open a run.
type ChatResult struct {
	RunID string                `json:"run_id"`
	Final string                `json:"final"`
	Route string                `json:"route"`
	Tools []agentcore.ToolTrace `json:"tool_calls"`
	Usage agentcore.Usage       `json:"usage"`
	Turns int                   `json:"turns"`
	Card  *agentcore.ResultCard `json:"card,omitempty"`
	// Stopped marks a turn the user cancelled mid-run. It is not an error: Final
	// carries whatever had streamed, nothing is appended to the conversation, and
	// the client renders a neutral "Stopped" marker rather than a red failure. A
	// second tab learns the same fact from the run row's `stopped` status.
	Stopped bool `json:"stopped,omitempty"`
	// Waiting marks a turn that ended parked on an ask tool call awaiting a
	// human answer. The client renders the question card rather than concluding
	// the turn.
	Waiting bool `json:"waiting,omitempty"`
	// Question carries the parked question's prompt and options (when Waiting is true).
	Question json.RawMessage `json:"question,omitempty"`
}

// chatWork is the delegated turn handed to the data handler.
type chatWork struct {
	ProjectID string
	AgentID   string
	Message   string
	PiHistory *PiConversationHistory
	InputID   string
	SessionID string
	// ConversationID, when set, mirrors each completed tool call into the
	// conversation log (ConvKindToolTrace) so a second machine/user sees the same
	// work timeline (design §7.3). Empty disables mirroring (legacy /chat).
	ConversationID string
	OnRunID        func(string)
	OnPlan         func([]PlanItem)
	// Goal, when non-empty, activates the run-level goal gate for this turn
	// (parsed from a leading "/goal <condition>" line; see parseDirective).
	Goal string
	// ReasoningEffort, when non-empty, is a magic keyword's per-turn override
	// ("ultrathink" → "high"; see parseMagicKeywords). Threaded into the run's
	// provider config; empty leaves the tier default.
	ReasoningEffort string
	// ReadOnly carries ChatOptions.ReadOnly into the run.
	ReadOnly        bool
	ResumeFromRunID string
}

// ChatService runs conversations through one native Runner. The handler seam
// lets command and turn-admission tests avoid opening a database.
type ChatService struct {
	runner *Runner
	handle func(ctx context.Context, req chatWork, sink agentcore.StreamSink) (ChatResult, error)
}

// NewChatService wires the conversational general agent over the storage layer.
// RunnerOptions (e.g. WithSandbox, WithLiveRegistry) are forwarded so a
// chat-triggered run shares the host's isolation substrate and live control. One
// Runner owns the native agent run and its host bindings.
func NewChatService(store *storage.Store, runnerOpts ...RunnerOption) *ChatService {
	s := &ChatService{runner: NewRunner(store, runnerOpts...)}
	s.handle = s.handleData
	return s
}

// Chat runs one turn. When sink is non-nil the turn streams (an opening progress
// beat, then either the typed-out reply or the agent's own tokens/progress/card)
// as it runs; the returned ChatResult is identical with or without a sink.
func (s *ChatService) Chat(ctx context.Context, opts ChatOptions, sink agentcore.StreamSink) (ChatResult, error) {
	emit := func(ev agentcore.StreamEvent) {
		if sink != nil {
			sink(ev)
		}
	}
	emit(agentcore.StreamEvent{Type: agentcore.StreamProgress, Note: "Reading your message…"})

	// Slash commands are decided before anything else — they are the one kind of
	// message whose meaning does not depend on what the model thinks of it.
	// A handled command answers the turn outright (no run, no spend); /goal
	// configures the run that follows; a bare "/goal" with no condition is not a
	// gate at all and falls through as ordinary text.
	d := parseDirective(opts.Message)
	goal, message := "", opts.Message
	switch {
	case d.Name == "" || (d.Name == CmdGoal && d.Arg == ""):
		// not a command
	case d.Name == CmdGoal:
		goal, message = goalTurn(d)
	default:
		return s.runCommand(ctx, opts, d, sink)
	}

	// Magic keywords are the same mechanism one level down: standalone prose
	// words that configure the run rather than address the model. They are
	// stripped from the message the agent sees and recorded in the durable log.
	// The route has already appended the raw user text to the transcript.
	message, fired := parseMagicKeywords(message)
	effort := ""
	for _, kw := range fired {
		if kw.ReasoningEffort != "" {
			effort = kw.ReasoningEffort
		}
	}

	if len(opts.History) > 0 {
		return ChatResult{}, errors.New("Pi chat requires native history; use a native conversation or clear the legacy context")
	}
	if opts.ConversationID != "" && (opts.PiHistory == nil || opts.InputID == "") {
		return ChatResult{}, errors.New("native conversation requires a history anchor and durable input ID")
	}
	// A gated turn announces its condition before the run opens: the client pins
	// it above the thread, and the log carries it so a reload still shows what the
	// agent is working toward. Both are best-effort — a goal that fails to persist
	// is still enforced, because the gate itself lives in the run's own log.
	if goal != "" {
		if opts.OnGoal != nil {
			opts.OnGoal(goal)
		}
		if opts.ConversationID != "" && s.runner != nil && s.runner.Store != nil {
			_, _ = AppendGoalEntry(context.WithoutCancel(ctx), s.runner.Store, opts.ConversationID, opts.AgentID, "", goal)
		}
	}
	// Same durable-record reasoning as the goal entry: the keyword is stripped
	// from what the model sees, so the log is the only place a reader (or a
	// later audit of why a turn ran hot) can see it fired.
	if len(fired) > 0 && opts.ConversationID != "" && s.runner != nil && s.runner.Store != nil {
		words := make([]string, 0, len(fired))
		for _, kw := range fired {
			words = append(words, kw.Word)
		}
		_, _ = AppendKeywordEntry(context.WithoutCancel(ctx), s.runner.Store, opts.ConversationID, opts.AgentID, "", words)
	}
	res, err := s.handle(ctx, chatWork{
		ProjectID: opts.ProjectID, AgentID: opts.AgentID, Message: message,
		SessionID: opts.SessionID, ConversationID: opts.ConversationID,
		PiHistory: opts.PiHistory, InputID: opts.InputID,
		OnRunID: opts.OnRunID, OnPlan: opts.OnPlan, Goal: goal,
		ReasoningEffort: effort,
		ReadOnly:        opts.ReadOnly,
	}, sink)

	res.Route = routeData
	// The gate's sentinel is a protocol between the loop and the plugin, not
	// something to hand a reader. Left in, every gated answer in this product ends
	// with a bare "STATUS: DONE" — and then gets persisted that way, so it is still
	// there on reload and gets replayed to the model as its own prior words.
	res.Final = goalplugin.PublicText(res.Final)
	// A user stop unwinds the loop as an error, but it is not one. Persist nothing:
	// a half-finished answer appended here would be replayed to the model next turn
	// as its own completed thought. The partial text the user is looking at stays
	// on their screen (and in the run row's summary) without becoming history.
	if errors.Is(err, ErrRunStopped) {
		// Everything except the assistant entry still applies: the stopped turn
		// spent real tokens (a rebuilt result would report the most expensive
		// turns in a thread as free), and it may have pushed the conversation
		// past the compaction threshold the next turn has to build history over.
		res.Stopped = true
		s.maybeCompact(ctx, opts)
		return res, nil
	}
	if err != nil {
		s.persistAssistantTurn(ctx, opts, formatAgentError(err.Error()), res.RunID, res.Turns)
		return ChatResult{RunID: res.RunID, Route: routeData, Tools: res.Tools, Turns: res.Turns}, err
	}

	if res.Waiting {
		return res, nil
	}
	s.persistAssistantTurn(ctx, opts, res.Final, res.RunID, res.Turns)
	s.maybeCompact(ctx, opts)
	return res, nil
}

// AnswerQuestion records the user's answer to a parked ask tool call, resumes
// the run from its durable session log, and streams the continuation.
func (s *ChatService) AnswerQuestion(ctx context.Context, opts AnswerOptions, sink agentcore.StreamSink) (ChatResult, error) {
	if s.runner == nil || s.runner.Store == nil || s.runner.SessionStore == nil {
		return ChatResult{}, fmt.Errorf("runner store required for answer")
	}
	waitingRun, err := s.runner.Store.LatestWaitingRunForSession(ctx, opts.UserID, opts.ProjectID, opts.SessionID)
	if err != nil {
		return ChatResult{}, fmt.Errorf("no parked question awaiting answer for session %q: %w", opts.SessionID, err)
	}

	durableSession := waitingRun.DurableSessionID
	if durableSession == "" {
		durableSession = waitingRun.ID
	}
	leaseCtx, release, err := agentcore.AcquireSessionLease(ctx, s.runner.SessionStore, durableSession)
	if err != nil {
		return ChatResult{}, fmt.Errorf("claiming parked session: %w", err)
	}
	defer func() { _ = release() }()
	ctx = leaseCtx

	// The row was selected before ownership acquisition and may have completed
	// while this request waited on another replica. Revalidate under the lease so
	// only the request that still owns a waiting predecessor can continue it.
	status, err := s.runner.Store.AgentRunStatus(ctx, waitingRun.ID)
	if err != nil {
		return ChatResult{}, fmt.Errorf("revalidating parked run: %w", err)
	}
	if status != "waiting" {
		return ChatResult{}, fmt.Errorf("parked run %s is already %s", waitingRun.ID, status)
	}

	sessionLog, err := s.runner.SessionStore.Log(ctx, durableSession)
	if err != nil {
		return ChatResult{}, fmt.Errorf("reading durable session log: %w", err)
	}
	pendingCallID, _, found := agentcore.PendingQuestion(sessionLog)
	if !found {
		// A crash after recording the answer but before starting the continuation
		// leaves the predecessor waiting. A supplied call id makes that retry
		// unambiguous; RecordSessionAnswer below recognizes the exact answer.
		if opts.CallID == "" {
			return ChatResult{}, fmt.Errorf("no unanswered question in durable session %s", durableSession)
		}
		pendingCallID = opts.CallID
	}
	if opts.CallID != "" && opts.CallID != pendingCallID {
		return ChatResult{}, fmt.Errorf("call id mismatch: expected %q, got %q", pendingCallID, opts.CallID)
	}

	convID := opts.ConversationID
	if convID == "" {
		convID = waitingRun.SessionID
	}
	var piHistory *PiConversationHistory
	if convID != "" {
		history, err := buildPiResumeHistory(ctx, s.runner.Store, convID)
		if err != nil {
			return ChatResult{}, err
		}
		state, err := recoverPiState(sessionLog)
		if err != nil {
			return ChatResult{}, err
		}
		if _, err := piConversationSuffix(history.Messages, state); err != nil {
			return ChatResult{}, fmt.Errorf("parked conversation changed: %w", err)
		}
		piHistory = &history
	}
	appended, err := agentcore.RecordSessionAnswer(ctx, s.runner.SessionStore, durableSession, pendingCallID, opts.Answer)
	if err != nil {
		return ChatResult{}, fmt.Errorf("recording answer entry: %w", err)
	}

	if convID != "" && appended {
		_, err = appendMessageAtLeaf(ctx, s.runner.Store, convID, string(agentcore.RoleUser), opts.Answer, waitingRun.AgentID, opts.UserID, "", 0, false, nil, true)
		if err != nil {
			return ChatResult{}, err
		}
	}

	work := chatWork{
		ProjectID:       opts.ProjectID,
		AgentID:         waitingRun.AgentID,
		Message:         "",
		SessionID:       opts.SessionID,
		ConversationID:  convID,
		OnPlan:          opts.OnPlan,
		ReadOnly:        opts.ReadOnly,
		ResumeFromRunID: durableSession,
		PiHistory:       piHistory,
	}
	res, err := s.handle(ctx, work, sink)
	res.Route = routeData

	if err == nil {
		_ = s.runner.Store.SetAgentRunStatus(ctx, waitingRun.ID, "done")
	}
	if err == nil && !res.Waiting {
		chatOpts := ChatOptions{
			ProjectID:      opts.ProjectID,
			AgentID:        waitingRun.AgentID,
			SessionID:      opts.SessionID,
			ConversationID: convID,
		}
		s.persistAssistantTurn(ctx, chatOpts, res.Final, res.RunID, res.Turns)
		s.maybeCompact(ctx, chatOpts)
	}
	return res, err
}

// formatAgentError turns a run-setup failure into the same next-step sentence
// the web client shows, so a reloaded thread still has the recovery copy (not
// the raw "no workspace model key configured" string). Mirrored in
// web/lib/ia.ts — keep the two sentence sets identical.
func formatAgentError(message string) string {
	raw := strings.TrimSpace(message)
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "agent is disabled"):
		return "Growth Lead is paused. Open Team → Set up and turn the agent on."
	case strings.Contains(lower, "no workspace model key"), strings.Contains(lower, "no api key"):
		return "Add an AI key in Settings so I can answer. One key is enough."
	case strings.Contains(raw, "SQLSTATE"), strings.Contains(lower, "syntax error at or near"):
		// A store bug reaching the user verbatim reads as the agent's own
		// confusion ("ERROR: syntax error at or near FILTER" on prod). The
		// detail stays in the run record; the reader gets the honest shape.
		return "Something broke on my side while I was setting up — the team has the detail. Try again in a moment."
	case raw == "":
		return "Something went wrong. Try again."
	default:
		return raw
	}
}

// persistAssistantTurn appends the agent's answer as a durable message entry on the
// conversation (DESIGN-CONVERSATION-STORE.md §5). It runs inside the (possibly
// detached) run goroutine, so the turn is recorded even when the streaming client
// has navigated away. No-op when the turn isn't conversation-scoped or produced no
// text. Best-effort: a persistence failure must not fail the answer the user is
// already seeing, so it is logged-by-omission (the run itself is still durable via
// agent_runs), not surfaced.
func (s *ChatService) persistAssistantTurn(ctx context.Context, opts ChatOptions, final, runID string, turn int) {
	if opts.ConversationID == "" || strings.TrimSpace(final) == "" || s.runner == nil || s.runner.Store == nil {
		return
	}
	// Detach from the request cancellation so a client disconnect at the moment of
	// completion can't abort the write of the answer the run already produced.
	wctx := context.WithoutCancel(ctx)
	_, _ = appendMessageAtLeaf(wctx, s.runner.Store, opts.ConversationID, string(agentcore.RoleAssistant), final, opts.AgentID, "", runID, turn, false, nil, true)
}

// maybeCompact summarizes a complete native prefix using the configured cheap
// tier. Original provider messages remain in the durable branch. Failure does
// not invalidate the answer already delivered to the user.
func (s *ChatService) maybeCompact(ctx context.Context, opts ChatOptions) {
	if opts.ConversationID == "" || s.runner == nil || s.runner.Store == nil {
		return
	}
	wctx := context.WithoutCancel(ctx)
	_, _ = compactPiConversation(wctx, s.runner.Store, opts.ConversationID,
		s.runner.RunTierWindow(wctx, opts.ProjectID), false, s.piSummarizer(opts.ProjectID))
}

// compactionSystem instructs the cheap tier to compress an older slice of the
// conversation into a durable running summary the model can rely on after the
// verbatim turns leave its window. It must preserve decisions, facts, data
// findings, and open threads — not editorialize.
const compactionSystem = `You are compacting the earlier part of an analytics assistant conversation so it can be dropped from the model's live context without losing meaning. ` +
	`Write a faithful running summary that preserves: the user's goals and questions, every concrete data finding or number the assistant reported, decisions made, and any open/unfinished threads. ` +
	`Be concise but lossless on facts. Do not invent anything. Output the summary only, no preamble.`

// handleData runs a delegated analytics turn through the general agent: it
// narrates friendly progress derived from the raw tool trace (deduped so repeated
// calls to the same tool don't spam the thread) and derives a result card from
// the run's tool outputs. The raw trace still streams for the client's developer
// view.
func (s *ChatService) handleData(ctx context.Context, req chatWork, sink agentcore.StreamSink) (ChatResult, error) {
	emit := func(ev agentcore.StreamEvent) {
		if sink != nil {
			sink(ev)
		}
	}
	emit(agentcore.StreamEvent{Type: agentcore.StreamProgress, Note: "Looking into your analytics…"})

	// Capture the run id (emitted early via OnRunID) so mirrored tool-trace entries
	// can carry it, while still forwarding it to the original caller.
	runID := ""
	onRunID := func(id string) {
		runID = id
		if req.OnRunID != nil {
			req.OnRunID(id)
		}
	}

	lastNote := ""
	wrapped := func(ev agentcore.StreamEvent) {
		if ev.Type == agentcore.StreamQuestion {
			emit(ev)
			if req.ConversationID != "" && s.runner != nil && s.runner.Store != nil {
				_, _ = AppendQuestionEntry(context.WithoutCancel(ctx), s.runner.Store, req.ConversationID, req.AgentID, runID, ev.Question, ev.Turn)
			}
			return
		}
		if ev.Type == agentcore.StreamTool {
			emit(ev) // forward the raw trace (debug)
			if ev.Tool != nil {
				// A completed update_plan is the agent's checklist changing. Surface
				// it as a first-class plan revision (live callback + durable entry)
				// rather than leaving it as one more anonymous row in the tool trace,
				// which is all it was before.
				s.mirrorPlan(ctx, req, runID, *ev.Tool, ev.Turn)
				// Mirror the completed call into the conversation log so a joining
				// client renders the same timeline (design §7.3). Best-effort.
				if req.ConversationID != "" && s.runner != nil && s.runner.Store != nil {
					_, _ = AppendToolTraceEntry(context.WithoutCancel(ctx), s.runner.Store, req.ConversationID, req.AgentID, runID, convToolTracePayload{
						CallID: ev.Tool.CallID, Tool: ev.Tool.Tool, Target: ToolTarget(ev.Tool.Args),
						Allowed: ev.Tool.Allowed, Reason: ev.Tool.Reason,
						Error: ev.Tool.Error, ResultMeta: ev.Tool.ResultMeta,
					}, ev.Turn)
				}
				if note := progressNote(ev.Tool.Tool); note != "" && note != lastNote {
					lastNote = note
					emit(agentcore.StreamEvent{Type: agentcore.StreamProgress, Note: note})
				}
			}
			return
		}
		emit(ev) // tokens (and anything else) pass straight through
	}

	opts := RunOptions{
		ProjectID: req.ProjectID, AgentID: req.AgentID, Trigger: "chat", Prompt: req.Message, InputID: req.InputID,
		SessionID: req.SessionID, OnRunID: onRunID, Goal: req.Goal,
		ReasoningEffort: req.ReasoningEffort,
		ReadOnly:        req.ReadOnly,
		ResumeFromRunID: req.ResumeFromRunID,
	}
	if req.PiHistory != nil {
		opts.NativeHistoryRevision = req.PiHistory.Revision
		if req.ResumeFromRunID == "" {
			opts.NativeHistory = req.PiHistory.Messages
		}
	}
	run, res, runErr := s.runner.RunStream(ctx, opts, wrapped)
	if persistErr := s.persistPiTurn(ctx, req, run.ID, res); persistErr != nil {
		runErr = errors.Join(runErr, persistErr)
	}
	if runErr != nil {
		// Final is carried even on the error return: a stopped run's partial answer
		// is the whole point of stopping gracefully, and on a genuine failure it is
		// empty anyway.
		return ChatResult{RunID: run.ID, Final: res.Final, Tools: res.Tools, Turns: res.Turns, Waiting: res.Parked, Question: res.Question}, runErr
	}

	card := cardFromMessages(res.Messages)
	if card != nil {
		emit(agentcore.StreamEvent{Type: agentcore.StreamCard, Card: card})
	}
	return ChatResult{
		RunID: run.ID, Final: res.Final, Tools: res.Tools,
		Usage: res.Usage, Turns: res.Turns, Card: card,
		Waiting: res.Parked, Question: res.Question,
	}, nil
}

// streamText emits text word-by-word as token events so a direct reply types out
// naturally, mirroring how a streamed model turn arrives. No-op when sink is nil
// (the JSON path returns the whole reply at once).
//
// Line breaks are preserved. Small talk is one line and never noticed the
// difference, but a command reply is markdown — a bullet list, a heading — and
// collapsing it to a single run of words renders as one unreadable paragraph
// until the closing `done` event replaces it. Streaming has to arrive as the
// shape it will settle into.
func streamText(text string, sink agentcore.StreamSink) {
	if sink == nil || strings.TrimSpace(text) == "" {
		return
	}
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			sink(agentcore.StreamEvent{Type: agentcore.StreamToken, Token: "\n"})
		}
		for j, word := range strings.Fields(line) {
			frag := word
			if j > 0 {
				frag = " " + word
			}
			sink(agentcore.StreamEvent{Type: agentcore.StreamToken, Token: frag})
		}
	}
}
