package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
)

// Only the serial completed-turn hooks write this state. RunPi reads it after
// waitForIdle has settled every callback, including the awaited terminal event.
type piHostLifecycle struct {
	mu           sync.Mutex
	pending      []agentcore.Message
	stopReason   string
	parked       bool
	prepareFirst func(context.Context, *PiSession) error
}

func bindPiHostLifecycle(ctx context.Context, cfg *PiRunConfig, projection *piRunProjection) (*piHostLifecycle, error) {
	host := cfg.Host
	if cfg.Session.Goal != "" && cfg.Session.Goal != host.PiGoal() {
		return nil, errors.New("Pi session goal differs from the composed host")
	}
	cfg.Session.Goal = host.PiGoal()
	if err := host.ValidatePiSession(cfg.Session.SessionID, cfg.Session.Store != nil); err != nil {
		return nil, err
	}
	options := map[string]json.RawMessage{}
	if len(cfg.Session.Pi.Options) > 0 {
		if err := json.Unmarshal(cfg.Session.Pi.Options, &options); err != nil || options == nil {
			return nil, errors.New("Pi options must be an object")
		}
	}
	var names []string
	if raw := options["callbacks"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &names); err != nil {
			return nil, err
		}
	}
	// These hooks own the scheduling decision. Silently replacing another
	// owner's hook would drop its policies; callers must compose explicitly.
	for _, name := range []string{"finishTurn", "prepareNextTurn", "prepareNextTurnWithContext", "transformContext"} {
		if slices.Contains(names, name) {
			return nil, fmt.Errorf("Pi host lifecycle conflicts with callback %s", name)
		}
	}
	names = append(names, "finishTurn", "prepareNextTurnWithContext", "transformContext")
	options["callbacks"], _ = json.Marshal(names)
	initial := map[string]json.RawMessage{}
	if raw := options["initialState"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &initial); err != nil || initial == nil {
			return nil, errors.New("Pi initial state must be an object")
		}
	}
	var model struct{ ID string }
	if raw := initial["model"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, err
		}
	}
	system, err := host.StartPiRun(ctx, cfg.Task)
	if err != nil {
		return nil, err
	}
	definitions, err := host.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	// A model binding may deliberately advertise no tools (or a smaller set).
	// Host composition must not widen that earlier capability decision.
	if configured, exists := initial["tools"]; exists {
		var allowed []struct{ Name string }
		if err := json.Unmarshal(configured, &allowed); err != nil {
			return nil, err
		}
		var all []json.RawMessage
		if err := json.Unmarshal(definitions, &all); err != nil {
			return nil, err
		}
		filtered := make([]json.RawMessage, 0, len(all))
		for _, raw := range all {
			var tool struct{ Name string }
			if err := json.Unmarshal(raw, &tool); err != nil {
				return nil, err
			}
			if slices.ContainsFunc(allowed, func(candidate struct{ Name string }) bool { return candidate.Name == tool.Name }) {
				filtered = append(filtered, raw)
			}
		}
		definitions, _ = json.Marshal(filtered)
	}
	initial["tools"] = definitions
	options["initialState"], _ = json.Marshal(initial)
	cfg.Session.Pi.Options, err = json.Marshal(options)
	if err != nil {
		return nil, err
	}
	// A named native section replaces the previous run's host instructions on
	// resume. Pi appends and logs this system message through normal events.
	section, _ := json.Marshal(map[string]any{"role": "system", "content": "", "sections": map[string]string{"agentray": system}, "timestamp": time.Now().UnixMilli()})
	input := []json.RawMessage{section}
	if len(cfg.Input) > 0 {
		raw := bytes.TrimSpace(cfg.Input)
		var text string
		var batch []json.RawMessage
		switch {
		case len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil:
			value, _ := json.Marshal(map[string]any{"role": "user", "content": text, "timestamp": time.Now().UnixMilli()})
			input = append(input, value)
		case len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &batch) == nil:
			input = append(input, batch...)
		default:
			if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
				return nil, errors.New("invalid native Pi input")
			}
			input = append(input, cfg.Input)
		}
	}
	cfg.Input, _ = json.Marshal(input)
	life := &piHostLifecycle{}
	progress := func(note string, turn int) {
		if note != "" && cfg.Sink != nil {
			cfg.Sink(agentcore.StreamEvent{Type: agentcore.StreamProgress, Turn: turn, Note: note})
		}
	}
	life.prepareFirst = func(ctx context.Context, session *PiSession) error {
		first, err := host.PreparePiTurn(ctx, agentcore.StepInfo{Turn: 1, Model: model.ID})
		if err != nil {
			return err
		}
		extra, err := nativehost.InputMessages(first.Messages)
		if err != nil {
			return err
		}
		// Draining external input starts only after the worker and session lease
		// exist. A failed startup must not consume a queued user correction.
		cfg.Input, err = json.Marshal(append(input, extra...))
		if err != nil {
			return err
		}
		if first.DisableTools {
			if err := session.agent.setState(ctx, json.RawMessage(`{"tools":[]}`)); err != nil {
				return err
			}
		}
		life.mu.Lock()
		life.stopReason = first.StopReason
		life.mu.Unlock()
		progress(first.Note, 1)
		return nil
	}
	original := cfg.Session.Pi.Callback
	cfg.Session.Pi.Callback = func(callCtx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		switch method {
		case "transformContext":
			return host.TransformPiContext(callCtx, params), nil
		case "tool":
			value, _, err := host.Execute(callCtx, params, emit)
			return value, err
		case "finishTurn":
			var turn struct {
				Message     json.RawMessage
				ToolResults []json.RawMessage
			}
			if err := json.Unmarshal(params, &turn); err != nil {
				return nil, err
			}
			message, err := nativehost.ProjectMessage(turn.Message)
			if err != nil {
				return nil, err
			}
			var native struct{ Model, StopReason string }
			if err := json.Unmarshal(turn.Message, &native); err != nil {
				return nil, err
			}
			var outcomes []agentcore.PiToolOutcome
			for _, raw := range turn.ToolResults {
				var result struct {
					ToolCallID, ToolName string
					Details              json.RawMessage
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					return nil, err
				}
				var audit agentcore.PiToolOutcome
				if json.Unmarshal(result.Details, &audit) == nil && audit.Trace.CallID == result.ToolCallID && audit.Trace.Tool == result.ToolName {
					outcomes = append(outcomes, audit)
				}
			}
			projection.mu.Lock()
			snapshot := projection.result
			snapshot.Tools = slices.Clone(snapshot.Tools)
			projection.mu.Unlock()
			decision, err := host.FinishPiTurn(callCtx, agentcore.PiCompletedTurn{
				Info: agentcore.StopInfo{Final: message.Content, Turns: snapshot.Turns, Tools: snapshot.Tools}, Usage: snapshot.Usage,
				Model: native.Model, StopReason: native.StopReason, Calls: message.ToolCalls, Outcomes: outcomes,
			})
			if err != nil {
				return nil, err
			}
			if decision.Parked && cfg.Session.Store != nil {
				// Parallel tool callbacks can settle their receipts in a different
				// order from native tool-result events. Present the same question
				// that RecordSessionAnswer will select from the durable journal.
				entries, err := cfg.Session.Store.Log(callCtx, cfg.Session.SessionID)
				if err != nil {
					return nil, err
				}
				_, question, pending := agentcore.PendingQuestion(entries)
				if !pending {
					return nil, errors.New("parked turn has no durable pending question")
				}
				projection.mu.Lock()
				changed := !nativehost.SameJSON(projection.result.Question, question)
				projection.result.Question = question
				projection.mu.Unlock()
				if changed {
					projection.emit(agentcore.StreamEvent{Type: agentcore.StreamQuestion, Question: question})
				}
			}
			life.mu.Lock()
			life.parked = decision.Parked
			if decision.Parked {
				life.stopReason = "parked"
			}
			if decision.StopReason != "" {
				life.stopReason = decision.StopReason
			}
			life.pending = append(life.pending, decision.Inject...)
			life.mu.Unlock()
			progress(decision.Note, snapshot.Turns)
			if decision.End {
				return json.RawMessage(`{"action":"end"}`), nil
			}
			if decision.Continue {
				return json.RawMessage(`{"action":"continue"}`), nil
			}
			return nil, nil
		case "prepareNextTurnWithContext":
			var turn struct {
				Context json.RawMessage
				Message struct{ Model string }
			}
			if err := json.Unmarshal(params, &turn); err != nil {
				return nil, err
			}
			projection.mu.Lock()
			info := agentcore.StepInfo{Turn: projection.result.Turns + 1, Model: turn.Message.Model, Usage: projection.result.Usage}
			projection.mu.Unlock()
			prepared, err := host.PreparePiTurn(callCtx, info)
			if err != nil {
				return nil, err
			}
			life.mu.Lock()
			if prepared.StopReason != "" {
				life.stopReason = prepared.StopReason
			}
			pending := append(life.pending, prepared.Messages...)
			life.pending = nil
			life.mu.Unlock()
			messages, err := nativehost.InputMessages(pending)
			if err != nil {
				return nil, err
			}
			system, changed, err := host.RefreshPiGoal()
			if err != nil {
				return nil, err
			}
			if changed {
				section, _ := json.Marshal(map[string]any{"role": "system", "content": "", "sections": map[string]string{"agentray": system}, "timestamp": time.Now().UnixMilli()})
				messages = append([]json.RawMessage{section}, messages...)
				progress("goal updated: "+host.PiGoal(), info.Turn)
			}
			update := map[string]any{"messages": messages}
			if prepared.DisableTools {
				var next map[string]json.RawMessage
				if err := json.Unmarshal(turn.Context, &next); err != nil || next == nil {
					return nil, errors.New("invalid native turn context")
				}
				next["tools"] = json.RawMessage(`[]`)
				update["context"] = next
			}
			progress(prepared.Note, info.Turn)
			return json.Marshal(update)
		default:
			if original == nil {
				return nil, fmt.Errorf("missing Pi callback for %s", method)
			}
			return original(callCtx, method, params, emit)
		}
	}
	return life, nil
}
