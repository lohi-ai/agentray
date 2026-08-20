package bench_test

// Bench-local run observation. The benches used to report only aggregates
// (turns, request count, "did compaction happen"), which is enough to say a run
// passed and not enough to say the HARNESS behaved: how tools were dispatched,
// whether parallel calls actually ran as a group, when compaction fired and what
// it did to the prompt, which provider calls were retried and why.
//
// runObserver collects two streams and writes them next to the artifacts:
//
//   - tool-timeline.json — one entry per EXECUTED tool call (hooks run after the
//     policy gate, so a blocked call never appears as executed), with the turn
//     it belonged to, argument preview, result size, and error.
//   - request-trace.json — one entry per provider call, via observe.Monitor's
//     decorator, so failed calls are visible too (a hook only fires on success).
//     Messages are digested, never dumped: count, bytes, and which compaction
//     marker the prompt carried.
//   - observer-summary.json — the run-level rollup, computed by observe.FoldRun
//     over the RunResult and the trace stream rather than over the hooks. That is
//     what makes a BLOCKED call and tool coverage (advertised vs invoked vs
//     unused) visible here at all; the hook streams above cannot see either.
//
// All three are cheap and additive: the composition is preset.Plugins(cfg) plus the
// monitor, so what runs is still the default agent.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/preset"
)

// toolEvent is one executed tool call as the loop saw it.
type toolEvent struct {
	Seq        int     `json:"seq"`
	Turn       int     `json:"turn"`
	AtSeconds  float64 `json:"at_seconds"`
	DurationMS int64   `json:"-"` // not measurable from an After hook alone
	Tool       string  `json:"tool"`
	Args       string  `json:"args"`
	ResultLen  int     `json:"result_bytes"`
	Result     string  `json:"result_preview,omitempty"`
	Err        string  `json:"error,omitempty"`
}

// requestEvent is one provider call at the model seam.
type requestEvent struct {
	Seq          int             `json:"seq"`
	AtSeconds    float64         `json:"at_seconds"`
	Model        string          `json:"model"`
	Messages     int             `json:"messages"`
	PromptBytes  int             `json:"prompt_bytes"`
	Compaction   string          `json:"compaction,omitempty"` // "summary" | "elide" when the prompt carried one
	GoalNudges   int             `json:"goal_nudges,omitempty"`
	Tools        int             `json:"tools_advertised"`
	StopReason   string          `json:"stop_reason,omitempty"`
	ToolCalls    []string        `json:"tool_calls,omitempty"`
	ParallelSize int             `json:"parallel_size,omitempty"` // >1 means the model asked for a group
	LatencyMS    int64           `json:"latency_ms"`
	Usage        agentcore.Usage `json:"usage"`
	Err          string          `json:"error,omitempty"`
}

// turnEvent marks a turn boundary, so the tool timeline can be read per turn.
type turnEvent struct {
	Turn       int     `json:"turn"`
	AtSeconds  float64 `json:"at_seconds"`
	Model      string  `json:"model"`
	StopReason string  `json:"stop_reason,omitempty"`
	End        bool    `json:"end"`
}

// runObserver accumulates the three streams. Every method is safe under the
// loop's parallel tool dispatch.
type runObserver struct {
	start time.Time

	mu       sync.Mutex
	turn     int
	tools    []toolEvent
	requests []requestEvent
	turns    []turnEvent
	// records are the raw provider-seam records, kept alongside the digested
	// requests because observe.FoldRun folds them (advertised tool names, stop
	// reasons, delegation depth) and requestEvent has already thrown those away.
	// Messages are stripped first: they are digested into PromptBytes/Compaction
	// above, the fold never reads them, and a long bench run would otherwise hold
	// the whole transcript twice.
	records []observe.TraceRecord
}

func newRunObserver() *runObserver { return &runObserver{start: time.Now()} }

func (o *runObserver) since() float64 { return time.Since(o.start).Seconds() }

// hooks returns the observation wiring for agentcore.Config.Hooks.
func (o *runObserver) hooks() agentcore.Hooks {
	return agentcore.Hooks{
		TurnStart: []agentcore.TurnHook{func(_ context.Context, info agentcore.TurnInfo) {
			o.mu.Lock()
			o.turn = info.Turn
			o.turns = append(o.turns, turnEvent{Turn: info.Turn, AtSeconds: o.since(), Model: info.Model})
			o.mu.Unlock()
		}},
		TurnEnd: []agentcore.TurnHook{func(_ context.Context, info agentcore.TurnInfo) {
			o.mu.Lock()
			o.turns = append(o.turns, turnEvent{Turn: info.Turn, AtSeconds: o.since(), Model: info.Model,
				StopReason: info.StopReason, End: true})
			o.mu.Unlock()
		}},
		After: []agentcore.AfterToolCall{func(_ context.Context, call agentcore.ToolCall, result string, runErr error) (string, bool) {
			o.mu.Lock()
			ev := toolEvent{
				Seq:       len(o.tools) + 1,
				Turn:      o.turn,
				AtSeconds: o.since(),
				Tool:      call.Name,
				Args:      ttsTruncateMiddle(call.Arguments, 400),
				ResultLen: len(result),
				Result:    ttsTruncateMiddle(result, 300),
			}
			if runErr != nil {
				ev.Err = runErr.Error()
			}
			o.tools = append(o.tools, ev)
			o.mu.Unlock()
			return result, false // observation only: never rewrite, never terminate
		}},
	}
}

// sink returns the observe.Sink that digests each provider call.
func (o *runObserver) sink() observe.Sink {
	return observe.SinkFunc(func(r observe.TraceRecord) {
		ev := requestEvent{
			AtSeconds:  o.since(),
			Model:      r.Model,
			Messages:   len(r.Messages),
			Tools:      len(r.Tools),
			StopReason: r.StopReason,
			LatencyMS:  r.LatencyMS,
			Usage:      r.Usage,
			Err:        r.Err,
		}
		for _, m := range r.Messages {
			ev.PromptBytes += len(m.Content)
			switch {
			case strings.HasPrefix(m.Content, summaryMarkerProbe):
				ev.Compaction = "summary"
			case strings.HasPrefix(m.Content, elideMarkerProbe):
				ev.Compaction = "elide"
			}
			if strings.HasPrefix(m.Content, goalNudgeMarkerProbe) {
				ev.GoalNudges++
			}
		}
		for _, c := range r.ToolCalls {
			ev.ToolCalls = append(ev.ToolCalls, c.Name)
		}
		if len(ev.ToolCalls) > 1 {
			ev.ParallelSize = len(ev.ToolCalls)
		}
		rec := r
		rec.Messages = nil
		o.mu.Lock()
		ev.Seq = len(o.requests) + 1
		o.requests = append(o.requests, ev)
		o.records = append(o.records, rec)
		o.mu.Unlock()
	})
}

// build composes the DEFAULT agent (preset.Plugins) plus the monitor decorator,
// with the hooks carried on the config exactly as a caller would set them.
func (o *runObserver) build(cfg agentcore.Config) (*agentcore.Agent, error) {
	cfg.Hooks = o.hooks()
	return agentcore.Build(append(preset.Plugins(cfg), observe.Monitor{Sink: o.sink()})...)
}

// observerSummary is the one-line behavioral digest printed into the test log.
//
// Its run-level half is no longer computed here: observe.FoldRun folds
// agentcore.RunResult.Tools, which is why this artifact can finally report a
// BLOCKED call (the After hook that used to feed these counts runs post-gate and
// structurally never saw one) and tool COVERAGE — which of the tools the persona
// paid schema tokens to advertise it actually touched.
//
// What stays local are the bench's own probes: compaction markers, goal nudges,
// parallel-group size, prompt size, slowest call. Those are harness questions
// about THIS suite, not run facts, so they do not belong in agentcore.
type observerSummary struct {
	Turns        int                             `json:"turns"`
	Requests     int                             `json:"requests"`
	FailedCalls  int                             `json:"failed_provider_calls"`
	ToolCalls    int                             `json:"tool_calls"`
	ToolErrors   int                             `json:"tool_errors"`
	ToolsBlocked int                             `json:"tools_blocked"`
	ToolsAborted int                             `json:"tools_aborted,omitempty"`
	GhostRun     bool                            `json:"ghost_run,omitempty"`
	ByTool       map[string]observe.ToolCounters `json:"by_tool"`
	ByStopReason map[string]int                  `json:"by_stop_reason,omitempty"`
	Coverage     observe.RunCoverage             `json:"coverage"`

	ParallelTurns int   `json:"parallel_tool_turns"`
	MaxParallel   int   `json:"max_parallel_group"`
	CompactedAt   []int `json:"compacted_requests,omitempty"`
	MaxPromptKB   int   `json:"max_prompt_kb"`
	SlowestCallMS int64 `json:"slowest_provider_call_ms"`
}

// summarize folds the finished run into the digest. It takes the RunResult
// because that is the only value carrying the tool trace — including the calls
// that never executed.
func (o *runObserver) summarize(res agentcore.RunResult) observerSummary {
	o.mu.Lock()
	defer o.mu.Unlock()
	run, coverage := observe.FoldRun(res, o.records)
	s := observerSummary{
		Turns:        run.Turns,
		Requests:     run.ProviderCalls,
		FailedCalls:  run.FailedProviderCalls,
		ToolCalls:    run.Tools.Total,
		ToolErrors:   run.Tools.Error,
		ToolsBlocked: run.Tools.Blocked,
		ToolsAborted: run.Tools.Aborted,
		GhostRun:     run.Ghost,
		ByTool:       run.ByTool,
		ByStopReason: run.ByStopReason,
		Coverage:     coverage,
	}
	for _, r := range o.requests {
		if r.ParallelSize > 1 {
			s.ParallelTurns++
			if r.ParallelSize > s.MaxParallel {
				s.MaxParallel = r.ParallelSize
			}
		}
		if r.Compaction != "" {
			s.CompactedAt = append(s.CompactedAt, r.Seq)
		}
		if kb := r.PromptBytes / 1024; kb > s.MaxPromptKB {
			s.MaxPromptKB = kb
		}
		if r.LatencyMS > s.SlowestCallMS {
			s.SlowestCallMS = r.LatencyMS
		}
	}
	return s
}

// dump writes the collected streams into dir (created if needed).
func (o *runObserver) dump(dir string, res agentcore.RunResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	o.mu.Lock()
	tools := append([]toolEvent(nil), o.tools...)
	reqs := append([]requestEvent(nil), o.requests...)
	turns := append([]turnEvent(nil), o.turns...)
	o.mu.Unlock()
	for name, v := range map[string]any{
		"tool-timeline.json":    tools,
		"request-trace.json":    reqs,
		"turn-boundaries.json":  turns,
		"observer-summary.json": o.summarize(res),
	} {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
