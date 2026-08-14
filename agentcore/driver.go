package agentcore

import "context"

// Driver is the agent loop, as a seam.
//
// Everything else in agentcore is a capability the loop consumes; the loop
// itself was the one thing you could not replace without forking the package.
// Making it a registered service closes that gap: DriverPlugin installs the
// built-in reason→act driver, and a consumer that needs a different control
// flow — a single-shot classifier with no tool phase, a plan-then-execute
// driver, a replay driver that answers from a recorded log — registers its own
// instead, keeping every other plugin (governance, durability, spill, jobs,
// observability) unchanged.
//
// A Driver receives the fully composed Agent. It owns the turn loop and nothing
// else: budgets, gates, hooks, and durability are all reachable through the
// Agent it is handed, so a replacement driver inherits the same rails rather
// than having to re-implement them.
type Driver interface {
	// Name identifies the driver in diagnostics.
	Name() string
	// Drive runs the loop to completion. messages is the seed history, task is
	// the recall/skill-selection string, sink is nil on a non-streamed run, and
	// emit forwards lifecycle events (it is safe to call with a nil sink).
	Drive(ctx context.Context, a *Agent, messages []Message, task string, sink StreamSink, emit func(StreamEvent)) (RunResult, error)
}

// reactDriver is agentcore's built-in driver: the reason→act loop in loop.go.
// It exists as a named type so the default is a plugin like any other, not a
// hardcoded call.
type reactDriver struct{}

func (reactDriver) Name() string { return "react" }

func (reactDriver) Drive(ctx context.Context, a *Agent, messages []Message, task string, sink StreamSink, emit func(StreamEvent)) (RunResult, error) {
	return a.drive(ctx, messages, task, sink, emit)
}

// DefaultDriver returns the built-in reason→act driver. Wrap or replace it in a
// custom DriverPlugin to change control flow without touching the package.
func DefaultDriver() Driver { return reactDriver{} }
