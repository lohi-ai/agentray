package usecase

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// classifyOpError is the registry's sentinel→kind mapping — the single place
// storage/connector failures become the opcore taxonomy every adapter
// preserves (REST code+status, MCP _meta.error_code, tool text, CLI stderr).
// Handlers may still return explicit *opcore.OpError for a better message;
// those pass through untouched.
//
// pgx.ErrNoRows maps to a bare "not found": the sentinel's own text is a
// driver detail ("no rows in result set"), and handlers that know which
// resource was missing already return opcore.NotFound with the name.
func classifyOpError(err error) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return &opcore.OpError{Kind: opcore.ErrNotFound, Message: "not found", Err: err}
	case errors.Is(err, storage.ErrRevisionConflict),
		errors.Is(err, storage.ErrIdempotencyConflict),
		errors.Is(err, storage.ErrSyncPaused):
		return &opcore.OpError{Kind: opcore.ErrConflict, Message: err.Error(), Err: err}
	case errors.Is(err, connector.ErrEngineBusy):
		return &opcore.OpError{Kind: opcore.ErrRetryable, Message: err.Error(), Err: err}
	default:
		return err
	}
}
