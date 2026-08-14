// Package memory installs the working-memory store used for recall across runs.
package memory

import "github.com/lohi-ai/agentray/agentcore"

// Plugin installs the memory store. A nil Store leaves the agent memoryless,
// which disables recall and persistence rather than failing.
type Plugin struct {
	Store agentcore.MemoryStore
}

// Name identifies the plugin.
func (Plugin) Name() string { return "memory" }

// Register claims the memory seam.
func (p Plugin) Register(r *agentcore.Registry) error {
	if p.Store == nil {
		return nil
	}
	return r.SetMemory(p.Store)
}
