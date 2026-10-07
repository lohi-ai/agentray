package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/2found/2ai/agentcore"
)

type piGoalRevision struct {
	agentcore.GoalRevision
	EffectID     string `json:"effectId"`
	SourceCallID string `json:"sourceCallId"`
}

func (s *PiSession) recordGoalRevision(ctx context.Context, effectID, callID string, revision agentcore.GoalRevision) error {
	if !s.config.ReviseGoal {
		return errors.New("native goal revision is not enabled")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.goalMu.Lock()
	defer s.goalMu.Unlock()
	if strings.TrimSpace(s.goal) == "" || strings.TrimSpace(s.goal) != revision.Previous || strings.TrimSpace(revision.Goal) == "" || strings.TrimSpace(revision.Reason) == "" || revision.Goal == revision.Previous {
		return s.latch(errors.New("invalid native goal revision"))
	}
	// The consumer normalizes its initial condition; retain the exact durable
	// predecessor in the record even if the caller supplied surrounding spaces.
	revision.Previous = s.goal
	source, ok := agentcore.ToolCallID(ctx)
	if !ok {
		return s.latch(errors.New("native goal revision requires a governed tool call"))
	}
	payload, err := json.Marshal(piGoalRevision{GoalRevision: revision, EffectID: effectID, SourceCallID: source})
	if err != nil {
		return s.latch(err)
	}
	if err := s.record(ctx, agentcore.EntryPiGoalRevision, callID, payload); err != nil {
		return err
	}
	s.goal = revision.Goal
	return nil
}

// piStoredGoal folds only explicit commits inside an admitted, still-open
// physical effect. Completion receipts and native messages are validated by
// recoverPiState; a goal commit cannot make an unsettled tool replayable.
func piStoredGoal(entries []agentcore.SessionEntry) (goal string, found bool, err error) {
	open := map[string]string{}
	for _, entry := range entries {
		switch entry.Kind {
		case agentcore.EntryPiEffectStart:
			var value struct {
				EffectID string `json:"effectId"`
			}
			if json.Unmarshal([]byte(entry.Content), &value) == nil && value.EffectID != "" {
				open[value.EffectID] = entry.CallID
			}
		case agentcore.EntryPiEffectDone:
			var value struct {
				EffectID string `json:"effectId"`
			}
			if json.Unmarshal([]byte(entry.Content), &value) == nil {
				delete(open, value.EffectID)
			}
		case agentcore.EntryPiGoal:
			var value *string
			if json.Unmarshal([]byte(entry.Content), &value) != nil || value == nil {
				return "", false, errors.New("invalid native Pi goal record")
			}
			if found && *value != goal {
				return "", false, errors.New("Pi goal revisions require explicit native integration")
			}
			goal, found = *value, true
		case agentcore.EntryPiGoalRevision:
			var revision piGoalRevision
			if json.Unmarshal([]byte(entry.Content), &revision) != nil {
				return "", false, errors.New("invalid native Pi goal revision record")
			}
			callID, admitted := open[revision.EffectID]
			if !found || strings.TrimSpace(goal) == "" || revision.Previous != goal || strings.TrimSpace(revision.Goal) == "" || revision.Goal == goal || strings.TrimSpace(revision.Reason) == "" || revision.SourceCallID == "" || !admitted || callID != entry.CallID {
				return "", false, fmt.Errorf("invalid native Pi goal revision for effect %q", revision.EffectID)
			}
			goal = revision.Goal
		}
	}
	return goal, found, nil
}
