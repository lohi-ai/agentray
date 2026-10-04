package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
)

// PiRuntimeConfig configures the native Go engine for parents, children and summaries.
// NativeStream optionally overrides the built-in Go provider dispatcher.
type PiRuntimeConfig struct {
	NativeStream engine.StreamFn
}

func WithPiRuntime(cfg PiRuntimeConfig) RunnerOption {
	return func(r *Runner) { copy := cfg; r.Pi = &copy }
}

// runModelLoop is the runner's single execution dispatch, after resolution and
// admission and before its common trace/terminal persistence path.
func (r *Runner) runModelLoop(ctx context.Context, p BuildParams, opts RunOptions, tier ModelTier, sink agentcore.StreamSink) (agentcore.RunResult, error) {
	if r.Pi == nil {
		if len(opts.NativeHistory) > 0 {
			return agentcore.RunResult{}, errors.New("native history requires the Pi runtime")
		}
		a, err := Build(p)
		if err != nil {
			return agentcore.RunResult{}, err
		}
		messages := append([]agentcore.Message{}, opts.History...)
		if opts.Prompt != "" || opts.ResumeFromRunID == "" {
			messages = append(messages, agentcore.Message{Role: agentcore.RoleUser, Content: opts.Prompt})
		}
		if sink != nil {
			return a.ContinueStream(ctx, messages, opts.Prompt, sink)
		}
		return a.Continue(ctx, messages, opts.Prompt)
	}
	runtime := *r.Pi
	// Auxiliary calls bind their own tier; never inherit the parent's bound
	// dispatcher or account pool. Preserve an explicit host stream override.
	summaryRuntime := runtime
	if len(opts.History) > 0 {
		return agentcore.RunResult{}, errors.New("Pi runs require native history; migrate the legacy conversation explicitly")
	}
	resume := opts.ResumeFromRunID != "" || p.ResumeSession
	if resume {
		if p.Session == nil || p.SessionID == "" {
			return agentcore.RunResult{}, errors.New("Pi runner resume requires a durable session")
		}
		if len(opts.NativeHistory) > 0 {
			return agentcore.RunResult{}, errors.New("Pi resume uses durable native history, not a replacement transcript")
		}
		leaseCtx, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
		if err != nil {
			return agentcore.RunResult{}, err
		}
		defer func() { _ = release() }()
		ctx = leaseCtx
		entries, err := p.Session.Log(ctx, p.SessionID)
		if err != nil {
			return agentcore.RunResult{}, err
		}
		if _, err := recoverPiState(entries); err != nil {
			return agentcore.RunResult{}, err
		}
		goal, found, err := piStoredGoal(entries)
		if err != nil {
			return agentcore.RunResult{}, err
		}
		if !found {
			return agentcore.RunResult{}, errors.New("Pi runner resume requires a recorded native goal contract")
		}
		if p.Goal != "" && p.Goal != goal {
			return agentcore.RunResult{}, errors.New("Pi resume cannot replace the durable goal")
		}
		p.Goal = goal
	}
	if opts.PrepareNextTurn != nil || p.PrepareNextTurn != nil {
		return agentcore.RunResult{}, errors.New("Pi runs require a native turn-preparation hook")
	}
	if p.Session == nil {
		p.SessionID = ""
	}
	initial := map[string]any{}
	if len(opts.NativeHistory) > 0 {
		var messages []json.RawMessage
		if err := json.Unmarshal(opts.NativeHistory, &messages); err != nil || messages == nil {
			return agentcore.RunResult{}, errors.New("native history must be a message array")
		}
		initial["messages"] = messages
	}
	if opts.ReasoningEffort != "" {
		switch opts.ReasoningEffort {
		case "off", "minimal", "low", "medium", "high", "xhigh":
			initial["thinkingLevel"] = opts.ReasoningEffort
		default:
			return agentcore.RunResult{}, fmt.Errorf("invalid Pi thinking level %q", opts.ReasoningEffort)
		}
	}
	options, err := json.Marshal(map[string]any{"initialState": initial, "sessionId": p.ProviderSessionID})
	if err != nil {
		return agentcore.RunResult{}, err
	}
	modelOptions := PiModelOptions{MaxTokens: p.MaxTokens, Pricing: observe.DefaultPricing(), RefreshKey: p.RefreshKey, ToolChoice: p.ToolChoice, ParallelToolCalls: p.ParallelToolCalls, OutputSchema: p.OutputSchema}
	baseBinding := agentcore.PiConfig{Options: options}
	var binding agentcore.PiConfig
	var known bool
	var ladder *nativeModelLadder
	if runtime.NativeStream == nil {
		var optionsFor func(ModelTier) (PiModelOptions, error)
		optionsFor, err = p.nativeLadderOptions(tier, modelOptions)
		if err == nil {
			ladder, err = newNativeModelLadder(tier, baseBinding, optionsFor)
		}
		if err == nil {
			binding, known, runtime.NativeStream = ladder.admissionBinding()
		}
	} else {
		binding, known, runtime.NativeStream, err = tier.bindNativeTier(baseBinding, p.nativeModelOptions(tier, modelOptions), runtime.NativeStream)
	}
	if err != nil {
		return agentcore.RunResult{}, err
	}
	var attempts *agentcore.RetryPolicy
	if ladder != nil {
		attempts = &agentcore.RetryPolicy{}
	}
	ctx = agentcore.WithRunSession(ctx, p.SessionID)
	binding = bindPiTrace(binding, p.Tracer, known, p.RunID)
	compactTier := tier
	if p.PiCompactionTier != nil {
		compactTier = *p.PiCompactionTier
	}
	summaryOptions := piSummaryModelOptions(p.RefreshKey)
	summaryOptionsFor, err := p.nativeLadderOptions(compactTier, summaryOptions)
	if err != nil {
		return agentcore.RunResult{}, err
	}
	compaction := &PiContextCompaction{Summarize: func(ctx context.Context, messages json.RawMessage, revision string) (string, agentcore.Usage, error) {
		// A separate trace session prevents auxiliary model calls from becoming
		// the apparent next conversational turn in the parent/child inspector.
		ctx = agentcore.WithRunSession(ctx, agentcore.RunSessionFrom(ctx)+"/summary-"+uuid.NewString())
		return summarizePiHistoryWithModelOptions(ctx, summaryRuntime, compactTier, messages, revision, summaryOptionsFor, p.Tracer)
	}}
	if p.Subagents != nil {
		plugin := *p.Subagents
		plugin.RunFork = piForkRunner(PiSessionConfig{Pi: binding, nativeLadder: ladder, nativeAttempts: attempts, NativeStream: runtime.NativeStream}, known, p.Session, p.ToolChoice, compaction)
		p.Subagents = &plugin
	}
	a, err := Build(p)
	if err != nil {
		return agentcore.RunResult{}, err
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		return agentcore.RunResult{}, err
	}
	defer host.Close()
	names, err := piHostToolNames(ctx, host)
	if err != nil {
		return agentcore.RunResult{}, err
	}
	if err := piValidateToolChoice(p.ToolChoice, names); err != nil {
		return agentcore.RunResult{}, err
	}
	// The native session admits only advertised names. Physical calls still
	// enter the composed host's complete policy, hooks, and credential boundary.
	var input json.RawMessage
	if opts.Prompt != "" || !resume {
		input, _ = json.Marshal(opts.Prompt)
		if opts.InputID != "" {
			input, _ = json.Marshal(map[string]any{"role": "user", "content": opts.Prompt, "timestamp": time.Now().UnixMilli(), "agentrayInputId": opts.InputID})
		}
	}
	result, err := RunPi(ctx, PiRunConfig{Host: host, Task: opts.Prompt, Input: input, Sink: sink, PricingKnown: known, Compaction: compaction,
		Session: PiSessionConfig{Pi: binding, nativeLadder: ladder, nativeAttempts: attempts, NativeStream: runtime.NativeStream, Store: p.Session, SessionID: p.SessionID, Resume: resume, ReviseGoal: p.ReviseGoal, HistoryRevision: opts.NativeHistoryRevision, Policy: agentcore.NewAllowList(names...)}})
	result.Projection.NativeState = result.State
	result.Projection.NativeTelemetry = result.Telemetry
	result.Projection.NativeRevision = result.Revision
	return result.Projection, err
}
