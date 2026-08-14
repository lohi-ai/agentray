package opcore

import (
	"context"

	"github.com/lohi-ai/agentray/agentcore"
)

// opTool adapts a Spec into an agentcore.Tool bound to one CallContext. This is
// the in-process adapter: the agent invokes the usecase handler directly in the
// same process, with no network hop — the cost win over routing the agent through
// the CLI/HTTP path.
type opTool struct {
	spec Spec
	cc   CallContext
}

func (t opTool) Name() string { return t.spec.OpName() }

func (t opTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        t.spec.OpName(),
		Description: t.spec.OpSummary(),
		Parameters:  t.spec.OpSchema(),
	}
}

func (t opTool) Run(ctx context.Context, args string) (string, error) {
	return t.spec.OpInvoke(ctx, t.cc, args)
}

// Tools projects every registered operation into agentcore.Tool form, bound to
// cc. The agentcore Policy (not this set) decides which are actually exposed to
// the model, so every operation is offered and scope filtering happens in the
// core, exactly as the previous hand-written ToolSet did.
func Tools(r *Registry, cc CallContext) []agentcore.Tool {
	specs := r.Specs()
	out := make([]agentcore.Tool, 0, len(specs))
	for _, s := range specs {
		out = append(out, opTool{spec: s, cc: cc})
	}
	return out
}

// TerminalNames returns the set of operation names whose successful tool call
// should end the agent run (used to build the agentcore terminate hook).
func TerminalNames(r *Registry) map[string]bool {
	m := map[string]bool{}
	for _, s := range r.Specs() {
		if s.OpTerminal() {
			m[s.OpName()] = true
		}
	}
	return m
}
