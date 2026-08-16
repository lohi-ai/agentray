package goal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// ToolName is the model-facing name of the goal-revision tool.
const ToolName = "update_goal"

// maxReasonBytes bounds the justification a revision carries. It is stored and
// shown, not summarized, so it needs a ceiling like anything else that lives for
// the rest of a long run.
const maxReasonBytes = 2000

// updateGoalTool lets the agent restate what finishing means.
//
// This is a deliberate transfer of authority and it should be read as one. The
// gate exists to stop a model ending a run it has not finished; a tool that
// rewrites the gate's condition is a tool that can end any run by redefining
// success. It is here because the alternative is worse in the case it is for: a
// long autonomous run discovers things, and a requirement that turns out to rest
// on a wrong assumption leaves an agent with only two honest moves — grind
// against it until MaxTurns, or declare BLOCKED and hand back nothing. Neither
// is what a competent collaborator does when they learn the task was mis-scoped.
//
// What keeps it accountable is not a restriction on the tool but the record it
// leaves. Every revision requires a reason, is written to the durable log as an
// EntryGoal the moment the loop drains it, and stays in the store's audit trail
// with the ones before it — so "the agent quietly narrowed its goal until it
// could pass" is a thing you can see afterwards, in order, with the model's own
// justification attached to each step.
//
// It is deliberately NOT bookkeeping: revising the objective is not
// self-management, and a turn spent on it should cost a turn.
type updateGoalTool struct{ store *Store }

func (updateGoalTool) Name() string { return ToolName }

func (updateGoalTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolName,
		Description: "Revise this run's completion condition — what has to be true for the run to be finished. " +
			"Use it ONLY when the work has shown the current condition to be wrong: impossible as stated, " +
			"based on an assumption that turned out false, or the wrong shape for what you found. " +
			"Do NOT use it to make finishing easier, to drop work you have not done, or to restate the " +
			"same condition in different words. The new condition must still serve what the user actually " +
			"asked for; the pinned requirement above it does not change. Every revision is recorded with " +
			"your reason and is reviewable.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"goal": map[string]any{
					"type": "string",
					"description": "The new completion condition, stated so that it is objectively checkable — " +
						"what must be true for this run to be done.",
				},
				"reason": map[string]any{
					"type": "string",
					"description": "What you discovered that makes the current condition wrong. " +
						"Be specific: name the finding, not the intention.",
				},
			},
			"required": []string{"goal", "reason"},
		},
	}
}

func (t updateGoalTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Goal   string `json:"goal"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("update_goal: %w", err)
	}
	in.Goal = strings.TrimSpace(in.Goal)
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Goal == "" {
		return "", fmt.Errorf("update_goal: goal is required")
	}
	// A revision without a reason is the exact move this tool must not make
	// cheap: it is the whole audit trail, and an unexplained narrowing is
	// indistinguishable from giving up.
	if in.Reason == "" {
		return "", fmt.Errorf("update_goal: reason is required — say what you found that makes the current condition wrong")
	}
	if len(in.Reason) > maxReasonBytes {
		in.Reason = in.Reason[:maxReasonBytes]
	}
	if !t.store.Update(in.Goal, in.Reason) {
		// Not an error: the model asked for a state that already holds. Saying so
		// plainly stops it retrying, which an error would invite.
		return "The completion condition is already:\n" + t.store.Goal() + "\nNothing changed.", nil
	}
	return "Completion condition updated. This run now finishes when:\n" + t.store.Goal() +
		"\nThe change is recorded with your reason. The user's original requirement is unchanged — " +
		"keep serving it.", nil
}

// Tools contributes the revision tool for this run. It is only offered when the
// run is actually gated: an ungated run has no condition to revise, and
// advertising a tool that cannot do anything spends schema tokens on every turn
// to no purpose.
func (g *gateRun) Tools() []agentcore.Tool {
	if g.store == nil || !g.revisable {
		return nil
	}
	return []agentcore.Tool{updateGoalTool{store: g.store}}
}

// ReviseGoal hands a pending revision to the loop, which owns what follows: the
// durable EntryGoal and the rebuilt system prompt. Draining here rather than in
// the tool is what keeps the plugin out of the log — the same rule that puts the
// goal's durable state in agentcore/goal.go and its policy here.
func (g *gateRun) ReviseGoal() (string, bool) {
	if g.store == nil {
		return "", false
	}
	return g.store.takePending()
}
