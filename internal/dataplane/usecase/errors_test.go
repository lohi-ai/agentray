package usecase

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	store "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// errors_test.go pins the board model's failure taxonomy: the class is what an
// adapter translates into a status and what a client branches on, so "the board
// changed" and "the document is wrong" must not arrive as the same answer.

func TestBoardFailureTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want opcore.ErrorKind
	}{
		{"stale revision", fmt.Errorf("save_board: %w", store.ErrRevisionConflict), opcore.ErrConflict},
		{"idempotency key reused", store.ErrIdempotencyConflict, opcore.ErrConflict},
		{"declaration on an archived board", store.ErrBoardArchived, opcore.ErrConflict},
		{"unknown board", fmt.Errorf("get_board: %w", pgx.ErrNoRows), opcore.ErrNotFound},
		// A refused document is the author's to fix, so it carries no kind: the
		// adapter answers 400 with the message naming the offending tile.
		{"invalid declaration", store.ErrBoardDefinitionInvalid, ""},
		{"unknown metric", store.ErrMetricUnknown, ""},
	}
	for _, tc := range cases {
		got := opcore.KindOf(classifyOpError(tc.err))
		if got != tc.want {
			t.Errorf("%s: kind = %q, want %q (err %v)", tc.name, got, tc.want, tc.err)
		}
	}

	// The wrapped form must classify too: handlers wrap the sentinel with the
	// tile that caused it, and a classification that only matched the bare
	// sentinel would send a conflict out as a 400.
	wrapped := fmt.Errorf("section %q tile %q: %w", "usage", "revenue", store.ErrBoardArchived)
	if got := opcore.KindOf(classifyOpError(wrapped)); got != opcore.ErrConflict {
		t.Fatalf("wrapped archived-board error kind = %q, want conflict", got)
	}
	if !errors.Is(classifyOpError(wrapped), store.ErrBoardArchived) {
		t.Fatal("classification dropped the sentinel the caller needs to test")
	}
}
