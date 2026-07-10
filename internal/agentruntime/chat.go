package agentruntime

import (
	"context"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// This file is the conversational entry of the general agent. It used to be a
// binding to a separate, domain-agnostic agentorch front desk; that package has
// been retired into the agent itself (see ARCHITECT-AGENT-TEAM.md). A chat turn
// is now owned end-to-end here: a cheap front-desk classifier answers small talk
// directly, and a real product question runs the general agent (the Growth
// Analyst), narrating friendly progress and deriving a result card. The
// orchestrator will return later as the *team lead* general agent when we build
// agent teams; until then there is one agent abstraction, conversational by
// default.

// route names the front-desk classifier emits. smalltalk is answered directly
// with the classifier's one-shot reply; data runs the general agent.
const (
	routeData      = "data"
	routeSmallTalk = "smalltalk"
)

// ChatOptions parameterize one chat turn. History is the prior conversation
// (oldest first), client-held and threaded per turn — there is no server-side
// conversation store.
type ChatOptions struct {
	ProjectID string
	// AgentID selects which of the project's agents handles the turn (AgentGarden
	// §3). Empty targets the project's default agent, preserving the single-agent
	// path. It is threaded verbatim into the run.
	AgentID string
	Message string
	History []agentcore.Message
	// SessionID is the client-held conversation id. When set, the in-flight run
	// registers under it (via the Runner's LiveRegistry) so a sibling request can
	// steer or follow-up the run. Empty disables live control for the turn.
	SessionID string
	// ConversationID, when set, makes the turn durable in the conversation store
	// (DESIGN-CONVERSATION-STORE.md): the route appends the user message and derives
	// History server-side before calling Chat, and Chat appends the assistant turn as
	// a message entry when it finishes — so the thread survives on the server and a
	// second machine/user can load and continue it. Empty keeps the legacy
	// client-held-history path. Distinct from SessionID (live-control key), though a
	// caller typically sets both to the same conversation id.
	ConversationID string
	// OnRunID, when set, is called with the run id as soon as the run row opens —
	// before any token — so a streaming caller can surface it to the client (which
	// persists it to reattach to the run after navigating away mid-stream).
	OnRunID func(string)
}

// ChatResult is the outcome of one chat turn, shaped to the chat JSON/SSE
// contract (run_id/final/route/tool_calls/usage/turns + the additive card).
// RunID is empty for a direct small-talk reply, which never opens a run.
type ChatResult struct {
	RunID string                `json:"run_id"`
	Final string                `json:"final"`
	Route string                `json:"route"`
	Tools []agentcore.ToolTrace `json:"tool_calls"`
	Usage agentcore.Usage       `json:"usage"`
	Turns int                   `json:"turns"`
	Card  *agentcore.ResultCard `json:"card,omitempty"`
}

// chatDecision is the front-desk classifier's verdict for one turn. A non-empty
// Reply is streamed directly (small talk / persona); otherwise Route selects how
// the turn is handled.
type chatDecision struct {
	Route string
	Reply string
	Usage agentcore.Usage
}

// chatWork is the delegated turn handed to the data handler.
type chatWork struct {
	ProjectID string
	AgentID   string
	Message   string
	History   []agentcore.Message
	SessionID string
	// ConversationID, when set, mirrors each completed tool call into the
	// conversation log (ConvKindToolTrace) so a second machine/user sees the same
	// work timeline (design §7.3). Empty disables mirroring (legacy /chat).
	ConversationID string
	OnRunID        func(string)
}

// ChatService owns one conversational turn of the general agent. It holds a
// single Runner (shared by the classifier's cheap-tier call and the agent run)
// and two seams — classify + handle — defaulted to the runner-backed
// implementations and overridable in tests so the routing logic is exercised
// without a database.
type ChatService struct {
	runner   *Runner
	classify func(ctx context.Context, projectID string, history []agentcore.Message, message string) (chatDecision, error)
	handle   func(ctx context.Context, req chatWork, sink agentcore.StreamSink) (ChatResult, error)
}

// NewChatService wires the conversational general agent over the storage layer.
// RunnerOptions (e.g. WithSandbox, WithLiveRegistry) are forwarded so a
// chat-triggered run shares the host's isolation substrate and live control. One
// Runner backs both the cheap classifier and the agent run.
func NewChatService(store *storage.Store, runnerOpts ...RunnerOption) *ChatService {
	s := &ChatService{runner: NewRunner(store, runnerOpts...)}
	s.classify = s.classifyTurn
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

	dec, err := s.classify(ctx, opts.ProjectID, opts.History, opts.Message)
	if err != nil {
		return ChatResult{}, err
	}

	// Direct reply (small talk / persona): stream it word-by-word, no run.
	if strings.TrimSpace(dec.Reply) != "" {
		streamText(dec.Reply, sink)
		s.persistAssistantTurn(ctx, opts, dec.Reply, "", 0)
		return ChatResult{Route: dec.Route, Final: dec.Reply, Usage: dec.Usage}, nil
	}

	// Anything else runs the general agent. Today the only non-direct route is
	// data; an unexpected route is a fail-closed error rather than a silent run.
	if dec.Route != routeData {
		return ChatResult{Route: dec.Route}, fmt.Errorf("agentruntime: no handler for route %q", dec.Route)
	}
	res, err := s.handle(ctx, chatWork{
		ProjectID: opts.ProjectID, AgentID: opts.AgentID, Message: opts.Message,
		History: opts.History, SessionID: opts.SessionID, ConversationID: opts.ConversationID,
		OnRunID: opts.OnRunID,
	}, sink)
	res.Route = dec.Route
	if err != nil {
		return ChatResult{RunID: res.RunID, Route: dec.Route, Tools: res.Tools, Turns: res.Turns}, err
	}

	// Fold the classification cost into the reported usage.
	res.Usage.InputTokens += dec.Usage.InputTokens
	res.Usage.OutputTokens += dec.Usage.OutputTokens
	res.Usage.CostUSD += dec.Usage.CostUSD
	s.persistAssistantTurn(ctx, opts, res.Final, res.RunID, res.Turns)
	s.maybeCompact(ctx, opts)
	return res, nil
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
	_, _ = AppendMessageEntry(wctx, s.runner.Store, opts.ConversationID, string(agentcore.RoleAssistant), final, opts.AgentID, "", runID, turn)
}

// maybeCompact appends a compaction entry when the conversation's live context
// estimate crosses the threshold (DESIGN-CONVERSATION-STORE.md §6), so the next
// turn's BuildHistory replays a summary plus the recent tail instead of the full
// transcript. The summarizer runs on the project's cheap tier — the same provider
// the front-desk classifier uses. Best-effort and detached: a failure to compact
// never fails the answer the user already has. No-op when not conversation-scoped.
func (s *ChatService) maybeCompact(ctx context.Context, opts ChatOptions) {
	if opts.ConversationID == "" || s.runner == nil || s.runner.Store == nil {
		return
	}
	wctx := context.WithoutCancel(ctx)
	summarize := func(sctx context.Context, transcript, previousSummary string) (string, error) {
		prov, model, err := s.runner.CheapProvider(sctx, opts.ProjectID)
		if err != nil {
			return "", err
		}
		// On a second+ compaction, fold the new transcript into the prior running
		// summary instead of summarizing the slice alone — only the latest summary
		// survives into BuildHistory, so a from-scratch summary would lose earlier
		// facts. (pi's iterative update-summary.)
		userContent := transcript
		if strings.TrimSpace(previousSummary) != "" {
			userContent = "## Running summary so far\n" + strings.TrimSpace(previousSummary) +
				"\n\n## New conversation since that summary\n" + transcript +
				"\n\nUpdate the running summary above so it preserves everything it already captured and folds in the new conversation. Output only the updated running summary."
		}
		resp, err := prov.Chat(sctx, agentcore.ChatRequest{
			Model: model, Temperature: 0.2, MaxTokens: 1024,
			Messages: []agentcore.Message{
				{Role: agentcore.RoleSystem, Content: compactionSystem},
				{Role: agentcore.RoleUser, Content: userContent},
			},
		})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(resp.Message.Content), nil
	}
	_, _ = MaybeCompactConversation(wctx, s.runner.Store, opts.ConversationID, summarize)
}

// compactionSystem instructs the cheap tier to compress an older slice of the
// conversation into a durable running summary the model can rely on after the
// verbatim turns leave its window. It must preserve decisions, facts, data
// findings, and open threads — not editorialize.
const compactionSystem = `You are compacting the earlier part of an analytics assistant conversation so it can be dropped from the model's live context without losing meaning. ` +
	`Write a faithful running summary that preserves: the user's goals and questions, every concrete data finding or number the assistant reported, decisions made, and any open/unfinished threads. ` +
	`Be concise but lossless on facts. Do not invent anything. Output the summary only, no preamble.`

// classifySystem instructs the cheap tier to act as the agent's front desk:
// decide whether the message needs the user's product data, and for small talk
// write the reply itself in one shot (saving a second round trip).
const classifySystem = `You are the friendly front desk of an analytics assistant for a non-technical product person. ` +
	`Decide whether the user's latest message needs their product DATA (metrics, funnels, retention, traffic, events, charts, "how many", "which", "why did X change") ` +
	`or is SMALL TALK / greeting / thanks / meta ("hi", "what can you do", "who are you", "thanks").

Reply ONLY with a compact JSON object, no prose, no code fences:
{"route":"data"} when the message needs their analytics data, or
{"route":"smalltalk","reply":"<a warm, brief, human reply>"} for small talk or meta.

For smalltalk replies: sound like a helpful human teammate, one or two sentences, no jargon, and gently invite a real question about their product when it fits. Never invent data.`

// classifyTurn routes a turn on the project's cheap (lite) tier. It returns a
// ready reply only for small talk; on any provider/parse failure it
// conservatively routes to data — better to do the analytics work than to wrongly
// brush off a real question. A CheapProvider error (disabled agent / no key) is a
// hard error so the caller degrades to a setup prompt rather than a dead end.
func (s *ChatService) classifyTurn(ctx context.Context, projectID string, history []agentcore.Message, message string) (chatDecision, error) {
	prov, model, err := s.runner.CheapProvider(ctx, projectID)
	if err != nil {
		return chatDecision{}, err
	}

	msgs := []agentcore.Message{{Role: agentcore.RoleSystem, Content: classifySystem}}
	msgs = append(msgs, recentHistory(history, 6)...)
	msgs = append(msgs, agentcore.Message{Role: agentcore.RoleUser, Content: message})

	resp, err := prov.Chat(ctx, agentcore.ChatRequest{
		Model: model, Messages: msgs, Temperature: 0.2, MaxTokens: 400,
	})
	if err != nil {
		return chatDecision{Route: routeData}, nil // conservative fallback
	}

	route, reply := decodeDecision(resp.Message.Content)
	if route == routeSmallTalk && reply != "" {
		return chatDecision{Route: routeSmallTalk, Reply: reply, Usage: resp.Usage}, nil
	}
	return chatDecision{Route: routeData, Usage: resp.Usage}, nil
}

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
		if ev.Type == agentcore.StreamTool {
			emit(ev) // forward the raw trace (debug)
			if ev.Tool != nil {
				// Mirror the completed call into the conversation log so a joining
				// client renders the same timeline (design §7.3). Best-effort.
				if req.ConversationID != "" && s.runner != nil && s.runner.Store != nil {
					_, _ = AppendToolTraceEntry(context.WithoutCancel(ctx), s.runner.Store, req.ConversationID, req.AgentID, runID, convToolTracePayload{
						Tool: ev.Tool.Tool, Allowed: ev.Tool.Allowed, Reason: ev.Tool.Reason,
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

	run, res, runErr := s.runner.RunStream(ctx, RunOptions{
		ProjectID: req.ProjectID, AgentID: req.AgentID, Trigger: "chat", Prompt: req.Message,
		History: req.History, SessionID: req.SessionID, OnRunID: onRunID,
	}, wrapped)
	if runErr != nil {
		return ChatResult{RunID: run.ID, Tools: res.Tools, Turns: res.Turns}, runErr
	}

	card := cardFromMessages(res.Messages)
	if card != nil {
		emit(agentcore.StreamEvent{Type: agentcore.StreamCard, Card: card})
	}
	return ChatResult{
		RunID: run.ID, Final: res.Final, Tools: res.Tools,
		Usage: res.Usage, Turns: res.Turns, Card: card,
	}, nil
}

// streamText emits text word-by-word as token events so a direct reply types out
// naturally, mirroring how a streamed model turn arrives. No-op when sink is nil
// (the JSON path returns the whole reply at once).
func streamText(text string, sink agentcore.StreamSink) {
	if sink == nil || strings.TrimSpace(text) == "" {
		return
	}
	for i, word := range strings.Fields(text) {
		frag := word
		if i > 0 {
			frag = " " + word
		}
		sink(agentcore.StreamEvent{Type: agentcore.StreamToken, Token: frag})
	}
}
