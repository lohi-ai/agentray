package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lohi-ai/agentray/agentcore"
)

func parseNativeLadderSelection(raw string, previous *nativeLadderSelection) (nativeLadderSelection, error) {
	var record nativeLadderSelection
	var fields map[string]json.RawMessage
	invalid := func() (nativeLadderSelection, error) {
		return record, errors.New("corrupt native ladder selection record")
	}
	if json.Unmarshal([]byte(raw), &record) != nil || json.Unmarshal([]byte(raw), &fields) != nil {
		return invalid()
	}
	for _, name := range []string{"version", "generation", "rung", "providerId", "model"} {
		if len(fields[name]) == 0 || string(fields[name]) == "null" {
			return invalid()
		}
	}
	var model struct{ ID, API, Provider string }
	if record.Version != 1 || record.Rung < 0 || json.Unmarshal(record.Model, &model) != nil || model.ID == "" || model.API == "" || model.Provider == "" {
		return invalid()
	}
	generation, prior := uint64(0), 0
	if previous != nil {
		generation, prior = previous.Generation, previous.Rung
	}
	if generation == ^uint64(0) || record.Generation != generation+1 || record.Rung == prior {
		return invalid()
	}
	return record, nil
}

// restoreJournal validates every transition, including older selections hidden
// by later checkpoints. It publishes only after the entire sequence is valid.
func (l *nativeModelLadder) restoreJournal(entries []agentcore.SessionEntry) error {
	var previous *nativeLadderSelection
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range entries {
		if entry.Kind != agentcore.EntryPiModelSelection {
			continue
		}
		record, err := parseNativeLadderSelection(entry.Content, previous)
		if err != nil {
			return err
		}
		if err = l.validateLocked(record); err != nil {
			return err
		}
		previous = &record
	}
	if previous != nil {
		l.active, l.generation = previous.Rung, previous.Generation
	}
	return nil
}

// selectNativeRung writes through the session's existing lease/fault fence before
// publishing either the bound dispatcher or the Agent's checkpoint model.
func (s *PiSession) selectNativeRung(ctx context.Context, expected uint64, next int) error {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	ladder := s.config.nativeLadder
	if ladder == nil {
		return errors.New("native ladder binding is required")
	}
	if err := s.failure(); err != nil {
		return err
	}
	err := ladder.selectRung(ctx, expected, next, func(ctx context.Context, selection nativeLadderSelection) error {
		raw, err := json.Marshal(selection)
		if err != nil {
			return err
		}
		return s.record(ctx, agentcore.EntryPiModelSelection, "", raw)
	})
	if err != nil {
		return err
	}
	selection := ladder.selection()
	update, _ := json.Marshal(map[string]json.RawMessage{"model": selection.Model})
	// Once the journal commits, cancellation must not turn it into an in-memory
	// rollback. Any state-application failure latches the session until recovery.
	if _, err = s.agent.Call(context.WithoutCancel(ctx), "setState", update); err != nil {
		return s.latch(fmt.Errorf("apply committed native model: %w", err))
	}
	return nil
}
