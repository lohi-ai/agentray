package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
	"github.com/lohi-ai/agentray/ai"
)

const (
	piStateEntry  = agentcore.EntryPiState
	piEventEntry  = agentcore.EntryPiEvent
	piEffectStart = agentcore.EntryPiEffectStart
	piEffectDone  = agentcore.EntryPiEffectDone
)

// ErrPiUnsettledEffect means an earlier process admitted a tool effect without
// recording its outcome. Replaying it automatically could duplicate a write.
var ErrPiUnsettledEffect = nativehost.ErrUnsettledEffect

// PiSessionConfig supplies host policy and persistence around the Pi contract.
// It deliberately uses lossless Pi JSON, not the older Go Message projection.
type PiSessionConfig struct {
	Pi                      NativeAgentConfig
	nativeLadder            *nativeModelLadder     // session-owned; routing is composed before host wrappers
	nativeAttempts          *agentcore.RetryPolicy // nonnil installs native request admission
	nativeAttemptObserved   func(nativeBoundRung, ai.FallbackAttempt) error
	nativeTerminalPublished func(ai.AttemptOutcome) error
	// NativeStream optionally overrides the built-in Go provider.
	NativeStream    engine.StreamFn
	Policy          agentcore.Policy // nil is deny-all
	Store           agentcore.SessionStore
	SessionID       string
	Resume          bool
	Goal            string          // host completion contract; must match durable state on resume
	ReviseGoal      bool            // explicit authority to revise the completion condition
	Invocation      json.RawMessage // immutable host request contract, when supplied
	HistoryRevision string          // revision of a seeded native transcript, when known
}

// PiSession owns an agent and its session lease. SessionEntry.Content retains
// the native JSON verbatim; the existing Postgres and memory stores can persist
// it without changing or truncating Pi message fields.
type PiSession struct {
	agent     *NativeAgent
	config    PiSessionConfig
	ctx       context.Context
	release   func() error
	close     sync.Once
	closeErr  error
	writeMu   sync.Mutex
	modelMu   sync.Mutex // serializes model selection with checkpoint snapshots
	mu        sync.Mutex
	fault     error
	closed    bool
	callbacks map[string]bool
	goalMu    sync.Mutex
	goal      string
}

// NewPiSession builds a native Pi agent under the server's permission policy.
// A durable session requires a lease-capable store. Resume reads the native log;
// a legacy Go session must be migrated explicitly rather than silently decoded
// through an incompatible message schema.
func NewPiSession(ctx context.Context, cfg PiSessionConfig) (*PiSession, error) {
	if len(cfg.Invocation) > 0 {
		if _, err := piStoredInvocation([]agentcore.SessionEntry{{Kind: agentcore.EntryPiInvocation, Content: string(cfg.Invocation)}}); err != nil {
			return nil, err
		}
	}
	if (cfg.Store == nil) != (cfg.SessionID == "") {
		return nil, errors.New("Pi session requires both store and session ID, or neither")
	}
	if cfg.Resume && cfg.Store == nil {
		return nil, errors.New("Pi resume requires a durable session")
	}
	if cfg.Policy == nil {
		cfg.Policy = agentcore.DenyAll{}
	}
	s := &PiSession{config: cfg, ctx: ctx, goal: cfg.Goal, release: func() error { return nil }, callbacks: map[string]bool{}}
	if cfg.Store != nil {
		if _, ok := cfg.Store.(agentcore.SessionLeaseStore); !ok {
			return nil, errors.New("Pi durable sessions require a session lease")
		}
		var err error
		s.ctx, s.release, err = agentcore.AcquireSessionLease(ctx, cfg.Store, cfg.SessionID)
		if err != nil {
			return nil, err
		}
	}
	ok := false
	defer func() {
		if !ok {
			if s.agent != nil {
				_ = s.agent.Close()
			}
			_ = s.release()
		}
	}()
	options := map[string]json.RawMessage{}
	if len(cfg.Pi.Options) > 0 {
		if err := json.Unmarshal(cfg.Pi.Options, &options); err != nil || options == nil {
			return nil, errors.New("Pi options must be a JSON object")
		}
	}
	var names []string
	if raw := options["callbacks"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &names); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		s.callbacks[name] = true
	}
	for _, name := range []string{"beforeToolCall", "prepareRequest"} {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	options["callbacks"], _ = json.Marshal(names)
	var restoredCommit string
	if cfg.Store != nil {
		entries, err := cfg.Store.Log(s.ctx, cfg.SessionID)
		if err != nil {
			return nil, err
		}
		invocation, err := piStoredInvocation(entries)
		if err != nil {
			return nil, err
		}
		if cfg.Resume && !equalPiInvocation(invocation, cfg.Invocation) {
			return nil, errors.New("Pi invocation differs from the durable request")
		}
		if len(entries) > 0 && !cfg.Resume {
			return nil, errors.New("Pi session already exists; resume it explicitly")
		}
		if cfg.Resume {
			goal, _, err := piStoredGoal(entries)
			if err != nil {
				return nil, err
			}
			if cfg.Goal != goal {
				return nil, errors.New("Pi session goal differs from the resumed host contract")
			}
			for _, entry := range entries {
				if entry.Kind != piStateEntry {
					continue
				}
				if entry.Model == "" || (restoredCommit != "" && restoredCommit != entry.Model) {
					return nil, errors.New("Pi session has missing or inconsistent upstream revision")
				}
				restoredCommit = entry.Model
			}
			state, err := recoverPiState(entries)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Kind == agentcore.EntryPiModelSelection && cfg.nativeLadder == nil {
					return nil, errors.New("native model selection requires current native ladder bindings")
				}
			}
			if cfg.nativeLadder != nil {
				if err := cfg.nativeLadder.restoreJournal(entries); err != nil {
					return nil, err
				}
			}
			// Execution code and callbacks are always supplied by this host;
			// only persisted runtime state is restored from the log.
			var restored, current map[string]json.RawMessage
			_ = json.Unmarshal(state, &restored)
			if raw := options["initialState"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &current); err != nil {
					return nil, err
				}
			}
			// Never resurrect tool definitions from a previous host build.
			restored["tools"] = json.RawMessage(`[]`)
			if tools := current["tools"]; len(tools) > 0 {
				restored["tools"] = tools
			}
			options["initialState"], _ = json.Marshal(restored)
		}
	}
	if raw := options["initialState"]; len(raw) > 0 {
		var initial map[string]json.RawMessage
		if err := json.Unmarshal(raw, &initial); err != nil || initial == nil {
			return nil, errors.New("Pi initial state must be an object")
		}
		if rawTools := initial["tools"]; len(rawTools) > 0 {
			filtered, err := s.filterTools(rawTools)
			if err != nil {
				return nil, err
			}
			initial["tools"] = filtered
		}
		options["initialState"], _ = json.Marshal(initial)
	}
	binding := cfg.Pi
	binding.Options, _ = json.Marshal(options)
	binding.Callback = s.callback
	binding.OnEvent = s.event
	var err error

	var native *NativeAgent
	config := binding
	config.StreamFn = cfg.NativeStream
	if cfg.nativeAttempts != nil {
		if cfg.nativeLadder == nil {
			return nil, errors.New("native request admission requires a model ladder")
		}
		policy := *cfg.nativeAttempts
		config.admitRequest = func(ctx context.Context, request engine.Request, options map[string]any) (*engine.RequestAdmission, error) {
			return s.admitNativeRequest(ctx, request, options, policy)
		}
	}
	native, err = NewNativeAgent(s.ctx, config)
	if err == nil {
		s.agent = native
	}
	if err != nil {
		return nil, err
	}
	if restoredCommit != "" && restoredCommit != s.agent.UpstreamCommit() {
		return nil, fmt.Errorf("Pi session revision %s differs from native engine %s; explicit migration required", restoredCommit, s.agent.UpstreamCommit())
	}
	if cfg.HistoryRevision != "" && cfg.HistoryRevision != s.agent.UpstreamCommit() {
		return nil, errors.New("Pi history revision differs from native engine; explicit migration required")
	}
	if !cfg.Resume {
		if len(cfg.Invocation) > 0 {
			if !json.Valid(cfg.Invocation) || string(cfg.Invocation) == "null" {
				return nil, errors.New("invalid Pi invocation")
			}
			if err := s.record(s.ctx, agentcore.EntryPiInvocation, "", cfg.Invocation); err != nil {
				return nil, err
			}
		}
		goal, _ := json.Marshal(cfg.Goal)
		if err := s.record(s.ctx, agentcore.EntryPiGoal, "", goal); err != nil {
			return nil, err
		}
	}
	if err := s.checkpoint(s.ctx); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func (s *PiSession) filterTools(raw json.RawMessage) (json.RawMessage, error) {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		var header struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(tool, &header); err != nil {
			return nil, err
		}
		names = append(names, header.Name)
	}
	allowed := s.config.Policy.PermittedTools(s.ctx, names)
	filtered := make([]json.RawMessage, 0, len(tools))
	for i, tool := range tools {
		if slices.Contains(allowed, names[i]) {
			filtered = append(filtered, tool)
		}
	}
	return json.Marshal(filtered)
}

func (s *PiSession) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fault
}

func (s *PiSession) latch(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault == nil {
		s.fault = err
	}
	return s.fault
}

func (s *PiSession) record(ctx context.Context, kind agentcore.SessionEntryKind, callID string, payload json.RawMessage) error {
	if err := s.failure(); err != nil {
		return err
	}
	if s.config.Store == nil {
		return nil
	}
	if !json.Valid(payload) {
		return s.latch(errors.New("invalid native Pi record"))
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return context.Canceled
	}
	if err := s.failure(); err != nil {
		return err
	}
	// Terminal events still need to land after cancellation. A distributed
	// fencing token stays on this context; a lost lease still rejects the write.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	entry := agentcore.SessionEntry{
		Kind: kind, CallID: callID, Content: string(payload), CreatedAt: time.Now(),
	}
	if kind == piStateEntry {
		entry.Model = s.agent.UpstreamCommit()
	}
	err := s.config.Store.Append(writeCtx, s.config.SessionID, entry)
	if err != nil {
		err = s.latch(fmt.Errorf("persist Pi session: %w", err))
	}
	return err
}

func (s *PiSession) checkpoint(ctx context.Context) error {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	state, err := s.agent.State(ctx)
	if err != nil {
		return err
	}
	return s.record(ctx, piStateEntry, "", state)
}

func (s *PiSession) event(ctx context.Context, raw json.RawMessage) error {
	if err := s.failure(); err != nil {
		return err
	}
	var event struct{ Type, ToolCallID string }
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	switch event.Type {
	case "message_start", "message_end", "tool_execution_start", "tool_execution_end", "turn_end", "agent_start", "agent_end":
		if err := s.record(ctx, piEventEntry, event.ToolCallID, raw); err != nil {
			return err
		}
	}
	if event.Type == "agent_end" {
		if err := s.checkpoint(ctx); err != nil {
			return err
		}
	}
	if s.config.Pi.OnEvent != nil {
		return s.config.Pi.OnEvent(ctx, raw)
	}
	return nil
}

func (s *PiSession) hostCallback(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
	if err := s.failure(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if method == "beforeToolCall" {
		var input struct {
			ToolCall struct{ ID, Name string } `json:"toolCall"`
			Args     json.RawMessage           `json:"args"`
		}
		if err := json.Unmarshal(params, &input); err != nil {
			return nil, err
		}
		decision := s.config.Policy.Allow(ctx, agentcore.ToolCall{ID: input.ToolCall.ID, Name: input.ToolCall.Name, Arguments: string(input.Args)})
		if !decision.Allow {
			return json.Marshal(map[string]any{"block": true, "reason": decision.Reason})
		}
	}
	if method != "stream" && method != "tool" && !s.callbacks[method] {
		return nil, nil
	}
	if s.config.Pi.Callback == nil {
		return nil, fmt.Errorf("missing Pi callback for %s", method)
	}
	var callID, effectID string
	var effectMu sync.Mutex
	settled := false
	stopCancel := func() bool { return false }
	if method == "tool" {
		var call struct {
			ToolCallID string `json:"toolCallId"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, err
		}
		callID = call.ToolCallID
		effectID = uuid.NewString()
		intent, _ := json.Marshal(map[string]any{"effectId": effectID, "call": params})
		if err := s.record(ctx, piEffectStart, callID, intent); err != nil {
			return nil, err
		}
		ctx = agentcore.WithToolInvocationScope(ctx, effectID)
		ctx = agentcore.WithGoalRevisionRecorder(ctx, func(revisionCtx context.Context, revision agentcore.GoalRevision) error {
			effectMu.Lock()
			defer effectMu.Unlock()
			if settled {
				return errors.New("goal revision after tool settlement")
			}
			return s.recordGoalRevision(revisionCtx, effectID, callID, revision)
		})
		stopCancel = context.AfterFunc(ctx, func() {
			effectMu.Lock()
			defer effectMu.Unlock()
			if !settled {
				s.latch(fmt.Errorf("%w: %s", ErrPiUnsettledEffect, callID))
			}
		})
		defer stopCancel()
		if err := ctx.Err(); err != nil {
			return nil, s.latch(fmt.Errorf("%w: %s", ErrPiUnsettledEffect, callID))
		}
	}
	value, err := func() (value json.RawMessage, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("Pi host callback panic: %v", p)
			}
		}()
		return s.config.Pi.Callback(ctx, method, params, emit)
	}()
	if method == "tool" && ctx.Err() == nil {
		// This is physical completion, before afterToolCall or source-order
		// transcript placement. Cancellation is not evidence of completion.
		outcome := map[string]any{"effectId": effectID, "result": value}
		if err != nil {
			outcome = map[string]any{"effectId": effectID, "error": err.Error()}
		}
		payload, marshalErr := json.Marshal(outcome)
		if marshalErr != nil {
			return nil, s.latch(marshalErr)
		}
		effectMu.Lock()
		if writeErr := s.record(ctx, piEffectDone, callID, payload); writeErr != nil {
			effectMu.Unlock()
			return nil, writeErr
		}
		settled = true
		effectMu.Unlock()
		stopCancel()
	} else if method == "tool" {
		return nil, s.latch(fmt.Errorf("%w: %s", ErrPiUnsettledEffect, callID))
	}
	if err == nil && (method == "prepareRequest" || method == "prepareNextTurn" || method == "prepareNextTurnWithContext") && len(value) > 0 {
		var update map[string]json.RawMessage
		if err := json.Unmarshal(value, &update); err != nil {
			return nil, err
		}
		if contextRaw := update["context"]; len(contextRaw) > 0 {
			var next map[string]json.RawMessage
			if err := json.Unmarshal(contextRaw, &next); err != nil {
				return nil, err
			}
			if rawTools := next["tools"]; len(rawTools) > 0 {
				next["tools"], err = s.filterTools(rawTools)
				if err != nil {
					return nil, err
				}
			}
			update["context"], _ = json.Marshal(next)
			value, err = json.Marshal(update)
		}
	}
	return value, err
}

func (s *PiSession) Prompt(ctx context.Context, input json.RawMessage) error {
	if err := s.failure(); err != nil {
		return err
	}
	if stopped, err := s.finishTerminalDelegations(ctx, input); stopped || err != nil {
		return err
	}
	input, err := s.answerInput(ctx, input)
	if err != nil {
		return err
	}
	err = s.agent.Prompt(ctx, input)
	if fault := s.failure(); fault != nil {
		return fault
	}
	return err
}

func (s *PiSession) Continue(ctx context.Context) error {
	if err := s.failure(); err != nil {
		return err
	}
	if stopped, err := s.finishTerminalDelegations(ctx, nil); stopped || err != nil {
		return err
	}
	input, err := s.answerInput(ctx, nil)
	if err != nil {
		return err
	}
	if len(input) > 0 {
		err = s.agent.Prompt(ctx, input)
	} else {
		err = s.agent.Continue(ctx)
	}
	if fault := s.failure(); fault != nil {
		return fault
	}
	return err
}

func (s *PiSession) State(ctx context.Context) (json.RawMessage, error) { return s.agent.State(ctx) }

func (s *PiSession) Close() error {
	s.close.Do(func() {
		s.writeMu.Lock()
		s.closed = true
		s.writeMu.Unlock()
		_ = s.agent.Close()
		s.closeErr = s.release()
	})
	return s.closeErr
}

// recoverPiState adds server workflow validation to shared native recovery.
func recoverPiState(entries []agentcore.SessionEntry) (json.RawMessage, error) {
	raw, err := agentcore.RecoverNativeTranscript(entries)
	if err != nil {
		return nil, err
	}
	if _, err := piAnswerMessages(entries, raw); err != nil {
		return nil, err
	}
	if _, err := piStoredInvocation(entries); err != nil {
		return nil, err
	}
	if _, _, err := piStoredGoal(entries); err != nil {
		return nil, err
	}
	return raw, nil
}
