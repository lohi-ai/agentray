package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// PiArgumentPreparer identifies a bundled native implementation of a tool's
// synchronous preparation hook. The worker rejects unknown identifiers; Go
// functions cannot be transported as synchronous JavaScript callbacks.
type PiArgumentPreparer interface {
	PiArgumentPreparation() string
}

// PiToolOutcome is host audit/control data from one governed tool invocation.
// Pi owns the native transcript; these projections are for the server's traces,
// auxiliary context, and human-input workflow, not provider message replay.
type PiToolOutcome struct {
	Trace              ToolTrace        `json:"trace"`
	Invocations        []ToolInvocation `json:"invocations,omitempty"`
	AdditionalContexts []Message        `json:"additionalContexts,omitempty"`
	Parked             bool             `json:"parked,omitempty"`
	QuestionID         string           `json:"questionId,omitempty"`
	Executed           bool             `json:"executed"`
	Terminate          bool             `json:"terminate,omitempty"`
}

// PiToolHost exposes a composed Agent's governed tools to the original Pi loop.
// It owns the Agent's busy slot and extension resources until Close. The native
// run adapter connects its turn policies through the methods in pi_lifecycle.go.
type PiToolHost struct {
	agent         *Agent
	tools         *ToolSet
	exts          *extensionSet
	exempt        map[string]bool
	budget        *toolExecutionBudget
	ctx           context.Context
	cancel        context.CancelFunc
	gate          sync.RWMutex
	closed        bool
	goal          string
	baseSystem    string
	revisionMu    sync.Mutex
	committedGoal string
	finalizing    atomic.Bool
	lifecycle     atomic.Bool
	completed     atomic.Bool
}

// OpenPiTools creates the run's effective tool registry without entering the Go
// model loop. The Pi host must close the worker before closing this host.
func (a *Agent) OpenPiTools(ctx context.Context) (*PiToolHost, error) {
	if !a.tryAcquire() {
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &PiToolHost{agent: a, ctx: ctx, cancel: cancel, goal: a.goal, committedGoal: a.goal, budget: newToolExecutionBudget(a.limits.MaxToolCalls)}
	ok := false
	defer func() {
		if !ok {
			cancel()
			if h.exts != nil {
				h.exts.closeRun()
			}
			a.release()
		}
	}()
	owner := a.sessionID
	if owner == "" {
		owner = "run_" + newEntryID()
	}
	var err error
	var registry atomic.Pointer[ToolSet]
	registry.Store(a.tools)
	h.exts, err = beginExtensions(ctx, a.extensions, RunInfo{
		SessionID: a.sessionID, Owner: owner, ScopeID: a.def.ScopeID,
		Limits: a.limits, Depth: DelegationDepth(ctx), Durable: a.session != nil && a.sessionID != "",
		Session: a.session, Agent: a, Goal: a.goal,
		Bookkeeping: func(name string) bool { return isBookkeeping(registry.Load(), name) },
	})
	if err != nil {
		return nil, err
	}
	h.tools = a.tools.With()
	if len(a.def.enabledSkills()) > 0 {
		h.tools = withReadSkill(h.tools, a.def)
	}
	contributed, exempt := h.exts.contributedTools()
	h.tools = h.tools.With(contributed...)
	registry.Store(h.tools)
	h.exempt = exempt
	for _, name := range h.tools.Names() {
		tool, _ := h.tools.Get(name)
		if _, prepares := tool.(ArgPreparer); prepares {
			if native, ok := tool.(PiArgumentPreparer); !ok || native.PiArgumentPreparation() == "" {
				return nil, fmt.Errorf("tool %q requires synchronous prepareArguments; supply a native Pi tool implementation", name)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.ctx = WithRunSession(h.exts.runContext(ctx), a.sessionID)
	// Native calls need an explicit commit boundary even without a store. A
	// session callback supplies it on the invocation context, taking precedence.
	h.ctx = WithGoalRevisionRecorder(h.ctx, func(context.Context, GoalRevision) error {
		return errors.New("native goal revision requires a session recorder")
	})
	ok = true
	return h, nil
}

// Definitions returns policy-filtered native AgentTool definitions. Native Pi
// applies its own batch scheduling; unmarked Go tools request sequential mode.
func (h *PiToolHost) Definitions(ctx context.Context) (json.RawMessage, error) {
	h.gate.RLock()
	defer h.gate.RUnlock()
	if h.closed {
		return nil, errors.New("Pi tool host is closed")
	}
	permitted := h.agent.policy.PermittedTools(ctx, h.tools.Names())
	definitions := make([]map[string]any, 0)
	for _, schema := range h.tools.Schemas() {
		if slices.Contains(h.agent.seedDisabledTools, schema.Name) {
			continue
		}
		if !slices.Contains(permitted, schema.Name) && !gateExemptTools[schema.Name] && !h.exempt[schema.Name] {
			continue
		}
		mode := "sequential"
		if isParallelTool(h.tools, ToolCall{Name: schema.Name}) {
			mode = "parallel"
		}
		parameters := schema.Parameters
		if parameters == nil {
			// An absent Go schema means unconstrained arguments. Pi expects a
			// JSON Schema object; null crashes native provider schema traversal.
			parameters = map[string]any{}
		}
		definition := map[string]any{
			"name": schema.Name, "label": schema.Name, "description": schema.Description,
			"parameters": parameters, "executionMode": mode,
		}
		tool, _ := h.tools.Get(schema.Name)
		if native, ok := tool.(PiArgumentPreparer); ok {
			definition["agentrayPrepareArguments"] = native.PiArgumentPreparation()
		}
		definitions = append(definitions, definition)
	}
	return json.Marshal(definitions)
}

// Execute is the tool branch of a PiCallback. The second result carries host
// workflow data that the enclosing server must handle at its event boundary.
// No provider message is decoded or reconstructed here.
func (h *PiToolHost) Execute(ctx context.Context, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, PiToolOutcome, error) {
	h.gate.RLock()
	defer h.gate.RUnlock()
	if h.closed {
		return nil, PiToolOutcome{}, errors.New("Pi tool host is closed")
	}
	var input struct {
		ID   string          `json:"toolCallId"`
		Name string          `json:"toolName"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, PiToolOutcome{}, err
	}
	if input.ID == "" || input.Name == "" || !json.Valid(input.Args) {
		return nil, PiToolOutcome{}, errors.New("invalid Pi tool invocation")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	defer func() { stop(); cancel() }()
	if h.ctx.Err() != nil {
		return nil, PiToolOutcome{}, h.ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, PiToolOutcome{}, err
	}
	if h.agent.sessionID != "" && toolInvocationScope(ctx) == "" {
		return nil, PiToolOutcome{}, errors.New("durable Pi tool execution requires a recorded invocation scope")
	}
	ctx = WithRunSession(piToolContext{Context: ctx, values: h.ctx}, h.agent.sessionID)
	record, _ := ctx.Value(goalRevisionRecorderKey{}).(goalRevisionRecorder)
	ctx = WithGoalRevisionRecorder(ctx, func(revisionCtx context.Context, revision GoalRevision) error {
		h.revisionMu.Lock()
		defer h.revisionMu.Unlock()
		if record == nil || revision.Previous != strings.TrimSpace(h.committedGoal) {
			return errors.New("native goal revision differs from the committed host contract")
		}
		if err := record(revisionCtx, revision); err != nil {
			return err
		}
		h.committedGoal = revision.Goal
		return nil
	})
	ctx = withToolExecutionBudget(ctx, h.budget)
	ctx = withToolBridgeGuard(ctx, func(name string) (bool, string) {
		if h.finalizing.Load() {
			return false, "run is finalizing"
		}
		if slices.Contains(h.agent.seedDisabledTools, name) {
			return false, "tool is disabled"
		}
		return true, ""
	})
	call := ToolCall{ID: input.ID, Name: input.Name, Arguments: string(input.Args)}
	var outcome toolOutcome
	switch {
	case h.finalizing.Load():
		outcome = toolOutcome{trace: ToolTrace{CallID: call.ID, Tool: call.Name, Args: call.Arguments, Reason: "run is finalizing"}, message: toolResult(call, "run is finalizing")}
	case slices.Contains(h.agent.seedDisabledTools, call.Name):
		outcome = toolOutcome{trace: ToolTrace{CallID: call.ID, Tool: call.Name, Args: call.Arguments, Reason: "tool is disabled"}, message: toolResult(call, "tool is disabled")}
	case !h.budget.reserve():
		outcome = toolOutcome{trace: ToolTrace{CallID: call.ID, Tool: call.Name, Args: call.Arguments, Reason: "tool-call budget exhausted"}, message: toolResult(call, "tool-call budget exhausted")}
	default:
		var update func(ToolCall, string)
		if emit != nil {
			update = func(_ ToolCall, partial string) {
				value, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": partial}}})
				if err := emit(value); err != nil {
					cancel()
				}
			}
		}
		outcome = h.agent.runToolCall(ctx, h.exts, h.exempt, h.tools, call, h.agent.limits, update)
		if !outcome.executed {
			h.budget.release()
		}
	}
	audit := PiToolOutcome{Trace: outcome.trace, Invocations: outcome.invocations,
		AdditionalContexts: outcome.extra, Parked: outcome.parked, Executed: outcome.executed, Terminate: outcome.terminate}
	if outcome.parked {
		audit.QuestionID = toolInvocationScope(ctx)
		if audit.QuestionID == "" {
			audit.QuestionID = newEntryID()
		}
		// Parking is a host workflow. Pi completes its normal immutable tool
		// result; a later human answer is a new native user message.
		outcome.message.Content = "Waiting for the user's answer."
		audit.Terminate = true
	}
	if err := ctx.Err(); err != nil {
		return nil, audit, err
	}
	content := make([]any, 0, 1+len(outcome.message.ContentParts))
	if outcome.message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": outcome.message.Content})
	}
	for _, part := range outcome.message.ContentParts {
		switch part.Type {
		case ContentPartText:
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case ContentPartImage:
			content = append(content, map[string]any{"type": "image", "mimeType": part.MIMEType, "data": part.Data})
		}
	}
	value, err := json.Marshal(map[string]any{"content": content, "details": audit,
		"isError": outcome.trace.Error != "" || !outcome.trace.Allowed, "terminate": audit.Terminate})
	return value, audit, err
}

// PiQuestionFromEntry recognizes only a successfully settled governed parked
// effect. The physical effect ID is the workflow ID: provider call IDs may be
// reused by a later assistant turn and must not reuse a previous human answer.
func PiQuestionFromEntry(entry SessionEntry) (string, json.RawMessage, bool) {
	if entry.Kind != EntryPiEffectDone {
		return "", nil, false
	}
	var receipt struct {
		ID     string `json:"effectId"`
		Result struct{ Details PiToolOutcome }
	}
	if json.Unmarshal([]byte(entry.Content), &receipt) != nil {
		return "", nil, false
	}
	audit := receipt.Result.Details
	if receipt.ID == "" || audit.QuestionID != receipt.ID || !audit.Parked || !audit.Executed || !audit.Trace.Allowed || audit.Trace.Error != "" || audit.Trace.CallID != entry.CallID || !json.Valid([]byte(audit.Trace.Args)) {
		return "", nil, false
	}
	return receipt.ID, json.RawMessage(audit.Trace.Args), true
}

// Keep per-call cancellation and fencing values while inheriting extensions
// from the long-lived run context. Rebuilding RunContext per tool would tie
// background jobs to a callback that ends as soon as their launch returns.
type piToolContext struct {
	context.Context
	values context.Context
}

func (c piToolContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.values.Value(key)
}

// Close cancels host work and waits for admitted tool callbacks before releasing
// extension resources and the Agent's busy slot. It is safe to call repeatedly.
func (h *PiToolHost) Close() error {
	h.cancel()
	h.gate.Lock()
	defer h.gate.Unlock()
	if !h.closed {
		h.closed = true
		h.exts.closeRun()
		h.agent.release()
	}
	return nil
}
