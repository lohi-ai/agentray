package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/ai"
)

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
		record, err := ai.ParseFallbackSelection(entry.Content, previous)
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
	if err = s.agent.setState(context.WithoutCancel(ctx), update); err != nil {
		return s.latch(fmt.Errorf("apply committed native model: %w", err))
	}
	return nil
}
