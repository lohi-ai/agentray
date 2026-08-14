// Package session installs durability: the append-only log that makes a run
// resumable after a crash or a compaction.
package session

import "github.com/lohi-ai/agentray/agentcore"

// Plugin installs the durable session log. Store and ID are both required for
// durability; supplying neither leaves the run purely in-memory.
type Plugin struct {
	Store agentcore.SessionStore
	ID    string
	// Resume continues the existing log at ID instead of opening a fresh run:
	// history is rebuilt from the log, dangling retry-safe calls are replayed
	// with their ORIGINAL call ids (so idempotency keys and child session ids
	// reproduce), and a log that already reached its leaf returns its recorded
	// answer with no provider call.
	Resume bool
	// SeedDisabledTools pre-disables tools for this run's circuit breaker, so a
	// tool that was broken in the crashed run stays disabled across the resume.
	SeedDisabledTools []string
}

// Name identifies the plugin.
func (Plugin) Name() string { return "session" }

// Register claims the session seam.
func (p Plugin) Register(r *agentcore.Registry) error {
	if len(p.SeedDisabledTools) > 0 {
		if err := r.SetSeedDisabledTools(p.SeedDisabledTools); err != nil {
			return err
		}
	}
	if p.Store == nil || p.ID == "" {
		return nil
	}
	return r.SetSession(p.Store, p.ID, p.Resume)
}
