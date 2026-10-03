package agentruntime

import (
	"context"
	"errors"
)

type nativeLadderAttemptKey struct{}
type nativeLadderAttemptScope struct {
	owner      *nativeModelLadder
	rung       int
	generation uint64
}

func (l *nativeModelLadder) attemptContext(ctx context.Context, rung int, generation uint64) (context.Context, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rung < 0 || rung >= len(l.rungs) || generation != l.generation {
		return nil, errors.New("invalid or stale native attempt scope")
	}
	return context.WithValue(ctx, nativeLadderAttemptKey{}, nativeLadderAttemptScope{owner: l, rung: rung, generation: generation}), nil
}

// Only provider preparation/dispatch may use a candidate scope. Tool policy and
// execution must continue to use the committed binding until selection succeeds.
func (l *nativeModelLadder) requestBinding(ctx context.Context) (nativeBoundRung, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	index := l.active
	if scope, ok := ctx.Value(nativeLadderAttemptKey{}).(nativeLadderAttemptScope); ok {
		if scope.owner != l || scope.generation != l.generation || scope.rung < 0 || scope.rung >= len(l.rungs) {
			return nativeBoundRung{}, errors.New("foreign or stale native attempt scope")
		}
		index = scope.rung
	}
	return l.rungs[index], nil
}
