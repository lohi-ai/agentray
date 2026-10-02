package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
)

func equalPiInvocation(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return samePiJSON(a, b)
}

func piStoredInvocation(entries []agentcore.SessionEntry) (json.RawMessage, error) {
	var invocation json.RawMessage
	for _, entry := range entries {
		if entry.Kind != agentcore.EntryPiInvocation {
			continue
		}
		var object map[string]json.RawMessage
		raw := json.RawMessage(entry.Content)
		if invocation != nil || json.Unmarshal(raw, &object) != nil || object == nil {
			return nil, errors.New("invalid or repeated Pi invocation contract")
		}
		invocation = append(json.RawMessage{}, raw...)
	}
	return invocation, nil
}

func piMessagesDigest(state json.RawMessage) (string, error) {
	var value struct{ Messages []json.RawMessage }
	if json.Unmarshal(state, &value) != nil || value.Messages == nil {
		return "", errors.New("native child has no message array")
	}
	// JSONB and worker transport can reorder object keys. Normalize JSON values,
	// preserving every native field, before binding the completion receipt.
	var messages any
	raw, _ := json.Marshal(value.Messages)
	if err := json.Unmarshal(raw, &messages); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(messages)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

type piChildCompletion struct {
	Final          string `json:"final"`
	StopReason     string `json:"stop_reason"`
	Revision       string `json:"revision"`
	MessagesDigest string `json:"messages_digest"`
}

func piCompletedChild(entries []agentcore.SessionEntry) (agentcore.RunResult, bool, error) {
	var receipt *piChildCompletion
	for i, entry := range entries {
		if entry.Kind != agentcore.EntryPiChildResult {
			continue
		}
		var value piChildCompletion
		if receipt != nil || i != len(entries)-1 || json.Unmarshal([]byte(entry.Content), &value) != nil || strings.TrimSpace(value.Final) == "" || value.Revision == "" || value.MessagesDigest == "" || value.StopReason == "" || value.StopReason == "error" || value.StopReason == "aborted" || value.StopReason == "parked" {
			return agentcore.RunResult{}, false, errors.New("invalid native child completion")
		}
		receipt = &value
	}
	if receipt == nil {
		return agentcore.RunResult{}, false, nil
	}
	state, err := recoverPiState(entries)
	if err != nil {
		return agentcore.RunResult{}, false, err
	}
	digest, err := piMessagesDigest(state)
	if err != nil || digest != receipt.MessagesDigest {
		return agentcore.RunResult{}, false, errors.New("native child completion does not match its transcript")
	}
	for _, entry := range entries {
		if entry.Kind == piStateEntry && entry.Model != receipt.Revision {
			return agentcore.RunResult{}, false, errors.New("native child completion revision mismatch")
		}
	}
	var native struct{ Messages []json.RawMessage }
	_ = json.Unmarshal(state, &native)
	result := agentcore.RunResult{Final: receipt.Final, StopReason: "reattached", NativeState: state, NativeRevision: receipt.Revision}
	for _, raw := range native.Messages {
		message, err := projectPiMessage(raw)
		if err != nil {
			return agentcore.RunResult{}, false, err
		}
		result.Messages = append(result.Messages, message)
	}
	// A completion record cannot manufacture an answer absent from Pi's log.
	final := ""
	for _, message := range result.Messages {
		if message.Role == agentcore.RoleAssistant {
			final = message.Content
		}
	}
	if result.Final != final {
		return agentcore.RunResult{}, false, errors.New("native child completion answer differs from its transcript")
	}
	return result, true, nil // Reattachment performs no model/tool work and has zero usage.
}

// A native agent_end is not proof that host output validation and completion
// hooks succeeded. If the process stopped before recording that outcome, do
// not repeat paid model work or manufacture an approved answer from plain text.
func piChildEndedWithoutReceipt(entries []agentcore.SessionEntry) bool {
	ended := false
	for _, entry := range entries {
		if entry.Kind != piEventEntry {
			continue
		}
		var event struct{ Type string }
		if json.Unmarshal([]byte(entry.Content), &event) != nil {
			continue
		}
		if event.Type == "agent_start" {
			ended = false
		}
		if event.Type == "agent_end" {
			ended = true
		}
	}
	return ended
}

// piForkRunner executes only the already-governed Agent.Fork child. Native
// options are copied per child; the parent's messages and worker process are
// never shared. Session ownership encloses both execution and completion write.
func piForkRunner(worker agentcore.PiConfig, pricingKnown bool, store agentcore.SessionStore, choice agentcore.ToolChoice, compaction *PiContextCompaction) subagent.ForkRunner {
	return func(ctx context.Context, child *agentcore.Agent, request subagent.ForkRequest, sink agentcore.StreamSink) (agentcore.RunResult, error) {
		history := json.RawMessage(`[]`)
		revision := ""
		if request.Previous != nil {
			var previous struct{ Messages json.RawMessage }
			if json.Unmarshal(request.Previous.NativeState, &previous) != nil || len(previous.Messages) == 0 || request.Previous.NativeRevision == "" {
				return agentcore.RunResult{}, errors.New("native child retry requires original native state and revision")
			}
			if err := validatePiConversationMessages(previous.Messages); err != nil {
				return agentcore.RunResult{}, err
			}
			history, revision = previous.Messages, request.Previous.NativeRevision
		}
		seedState, _ := json.Marshal(map[string]any{"messages": history})
		seedDigest, err := piMessagesDigest(seedState)
		if err != nil {
			return agentcore.RunResult{}, err
		}
		invocation, _ := json.Marshal(map[string]any{"task": request.Task, "prompt": request.Prompt, "history_digest": seedDigest, "revision": revision})
		identity := request.SessionID
		if identity == "" {
			identity = "pi-child-" + uuid.NewString()
		}
		ctx = agentcore.WithRunSession(ctx, identity)
		var sessionStore agentcore.SessionStore
		resume := false
		if request.SessionID != "" {
			if store == nil || child.SessionID() != request.SessionID {
				return agentcore.RunResult{}, errors.New("native child has no matching inherited session")
			}
			sessionStore = store
			leaseCtx, release, err := agentcore.AcquireSessionLease(ctx, store, request.SessionID)
			if err != nil {
				return agentcore.RunResult{}, err
			}
			defer func() { _ = release() }()
			ctx = leaseCtx
			entries, err := store.Log(ctx, request.SessionID)
			if err != nil {
				return agentcore.RunResult{}, err
			}
			resume = len(entries) > 0
			if resume {
				recorded, err := piStoredInvocation(entries)
				if err != nil {
					return agentcore.RunResult{}, err
				}
				if !equalPiInvocation(recorded, invocation) {
					return agentcore.RunResult{}, errors.New("native child session belongs to a different request")
				}
				result, done, err := piCompletedChild(entries)
				if err != nil {
					return result, err
				}
				if done {
					if sink != nil {
						sink(agentcore.StreamEvent{Type: agentcore.StreamProgress, Note: "reattached to completed native child"})
					}
					return result, nil
				}
				if piChildEndedWithoutReceipt(entries) {
					return agentcore.RunResult{}, errors.New("native child ended without a durable completion receipt; explicit reconciliation required")
				}
			}
		}
		cfg := worker
		var options map[string]json.RawMessage
		if err := json.Unmarshal(worker.Options, &options); err != nil {
			return agentcore.RunResult{}, err
		}
		var initial map[string]json.RawMessage
		if err := json.Unmarshal(options["initialState"], &initial); err != nil {
			return agentcore.RunResult{}, err
		}
		if initial == nil {
			initial = map[string]json.RawMessage{}
		}
		initial["messages"] = history
		options["initialState"], _ = json.Marshal(initial)
		options["sessionId"], _ = json.Marshal(identity)
		cfg.Options, _ = json.Marshal(options)
		host, err := child.OpenPiTools(ctx)
		if err != nil {
			return agentcore.RunResult{}, err
		}
		defer host.Close()
		names, err := piHostToolNames(ctx, host)
		if err != nil {
			return agentcore.RunResult{}, err
		}
		if err := piValidateToolChoice(choice, names); err != nil {
			return agentcore.RunResult{}, err
		}
		var input json.RawMessage
		if !resume {
			input, _ = json.Marshal(request.Prompt)
		}
		result, err := RunPi(ctx, PiRunConfig{Host: host, Task: request.Task, Input: input, Sink: sink, PricingKnown: pricingKnown, Compaction: compaction,
			Session: PiSessionConfig{Pi: cfg, Policy: agentcore.NewAllowList(names...), Store: sessionStore, SessionID: request.SessionID, Resume: resume, HistoryRevision: revision, Invocation: invocation}})
		projection := result.Projection
		projection.NativeState, projection.NativeTelemetry, projection.NativeRevision = result.State, result.Telemetry, result.Revision
		if err != nil {
			return projection, err
		}
		if ctx.Err() != nil {
			return projection, ctx.Err()
		}
		if projection.Parked {
			return projection, errors.New("native child is waiting for a human answer")
		}
		if projection.StopReason == "error" || projection.StopReason == "aborted" || projection.StopReason == "" || strings.TrimSpace(projection.Final) == "" {
			return projection, fmt.Errorf("native child did not complete an answer (%s)", projection.StopReason)
		}
		if sessionStore != nil {
			digest, err := piMessagesDigest(result.State)
			if err != nil {
				return projection, err
			}
			receipt, _ := json.Marshal(piChildCompletion{Final: projection.Final, StopReason: projection.StopReason, Revision: result.Revision, MessagesDigest: digest})
			wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := sessionStore.Append(wctx, request.SessionID, agentcore.SessionEntry{Kind: agentcore.EntryPiChildResult, Content: string(receipt)}); err != nil {
				return projection, err
			}
		}
		return projection, nil
	}
}

func piHostToolNames(ctx context.Context, host *agentcore.PiToolHost) ([]string, error) {
	definitions, err := host.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	var tools []struct{ Name string }
	if err := json.Unmarshal(definitions, &tools); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names, nil
}
