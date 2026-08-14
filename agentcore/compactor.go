package agentcore

import "context"

// Compactor is transcript shrinking, as a seam.
//
// The loop owns WHEN to consider compacting (once per turn, past the context
// budget) and owns the durable bracket around it. A Compactor owns WHAT
// compaction does: which span is old enough to lose, what replaces it, and what
// that costs. Those are strategy decisions with more than one right answer —
// summarize the older span with a model call, prune stale tool results with no
// call at all, keep a domain-specific span pinned — so they belong behind an
// interface rather than inside loop.go.
//
// This is the same shape as Driver: DefaultCompactor is the built-in provider,
// and a composition that wants different behaviour registers its own through
// Registry.SetCompactor without touching the package.
type Compactor interface {
	// Name identifies the compactor in diagnostics.
	Name() string
	// ShouldCompact reports whether the transcript has grown past the point
	// where this strategy wants to act. The loop asks once per turn; a
	// compactor that measures pressure differently (message count, a real
	// tokenizer, a provider-reported context length) answers here.
	ShouldCompact(messages []Message, budget int) bool
	// Compact returns the shrunk transcript. It must preserve provider
	// validity — every tool result stays adjacent to the assistant turn that
	// called it — because the loop hands the result straight to the next
	// request. Returning the input unchanged is a legal no-op.
	//
	// An error degrades to "no compaction this turn" rather than failing the
	// run: a transcript that could not be shrunk is still a transcript the
	// model can work with, while a killed run loses everything.
	Compact(ctx context.Context, req CompactionRequest) (CompactionResult, error)
}

// CompactionRequest is everything a compactor needs to decide and act. It is a
// struct rather than a parameter list so a new input is an additive change that
// no existing implementation has to acknowledge.
type CompactionRequest struct {
	// Messages is the live transcript, leading system prompt included.
	Messages []Message
	// Budget is the run's context ceiling in estimated tokens
	// (Limits.MaxContextTokens), already defaulted.
	Budget int
	// Turn is the turn about to be taken, for correlating with the durable log.
	Turn int
	// Settings is the retention policy, already clamped against Budget.
	Settings CompactionSettings
	// Provider and Model are the tier the summarization call should use: the
	// dedicated compaction rung when the composition pinned one, otherwise the
	// rung the run has escalated to. A compactor that makes no model call
	// ignores both.
	Provider LLMProvider
	Model    string
}

// CompactionResult is what a compactor produced.
type CompactionResult struct {
	// Messages is the replacement transcript.
	Messages []Message
	// Usage is what the compaction itself spent — real billable spend on a
	// summarizing backend, zero on a deterministic one. The loop folds it into
	// the run's accounting and stamps it on the durable completion entry, so
	// the audit trail shows what staying inside the window cost.
	Usage Usage
}

// summaryCompactor is agentcore's built-in strategy: summarize the older span
// into a structured checkpoint with a model call, degrading to a deterministic
// elide when the call is unavailable or fails. It exists as a named type so the
// default is a provider like any other, not a hardcoded call.
type summaryCompactor struct{}

func (summaryCompactor) Name() string { return "summary" }

func (summaryCompactor) ShouldCompact(messages []Message, budget int) bool {
	return shouldCompact(messages, budget)
}

func (summaryCompactor) Compact(ctx context.Context, req CompactionRequest) (CompactionResult, error) {
	msgs, u := compactWithSummary(ctx, req.Provider, req.Model, req.Messages, req.Settings)
	return CompactionResult{Messages: msgs, Usage: u}, nil
}

// DefaultCompactor returns the built-in summarizing compactor. Wrap or replace
// it in a custom compaction plugin to change the strategy without touching the
// package.
func DefaultCompactor() Compactor { return summaryCompactor{} }
