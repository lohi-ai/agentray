// Package compaction installs the transcript-shrinking policy applied as a run
// approaches its context limit, plus the optional dedicated tier the
// summarization call runs on.
//
// Compaction is only safe because the summarized detail is still recoverable:
// pair it with the sessionquery plugin so the model can search what was
// summarized away instead of re-running the tool that produced it.
package compaction

import "github.com/lohi-ai/agentray/agentcore"

// Plugin installs compaction.
type Plugin struct {
	// Settings tunes how much recent transcript is kept verbatim. nil keeps
	// agentcore.DefaultCompactionSettings().
	Settings *agentcore.CompactionSettings
	// Provider + Model, when both set, pin the summary call to a dedicated
	// (typically cheaper) tier instead of borrowing whichever rung the run has
	// escalated to.
	Provider agentcore.LLMProvider
	Model    string
	// Strategy replaces WHAT compaction does. nil keeps
	// agentcore.DefaultCompactor: summarize the older span into a structured
	// checkpoint with a model call, degrading to a deterministic elide when the
	// call fails.
	//
	// This is the seam that makes a second strategy a sibling package rather
	// than an edit to the loop — a pruner that drops stale tool results with no
	// model call at all, or a domain compactor that pins content the default
	// would summarize away, implements agentcore.Compactor and lands here.
	Strategy agentcore.Compactor
}

// Using swaps the compaction strategy, keeping the default retention policy.
func Using(c agentcore.Compactor) Plugin { return Plugin{Strategy: c} }

// Name identifies the plugin.
func (Plugin) Name() string { return "compaction" }

// Register claims the compaction seams it was given.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Settings != nil {
		if err := r.SetCompaction(*p.Settings); err != nil {
			return err
		}
	}
	if p.Strategy != nil {
		if err := r.SetCompactor(p.Strategy); err != nil {
			return err
		}
	}
	if p.Provider != nil && p.Model != "" {
		return r.SetCompactionModel(p.Provider, p.Model)
	}
	return nil
}
