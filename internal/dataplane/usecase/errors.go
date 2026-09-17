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
		errors.Is(err, storage.ErrSourceArchived),
		// A declaration aimed at an archived board is the same shape of
		// refusal: the request contradicts current state, and the caller
		// unarchives rather than retrying.
		errors.Is(err, storage.ErrBoardArchived),
		errors.Is(err, storage.ErrSyncPaused):
		return &opcore.OpError{Kind: opcore.ErrConflict, Message: err.Error(), Err: err}
	case errors.Is(err, connector.ErrEngineBusy),
		// The analytics sandbox is an engine too: a child that could not start or
		// died mid-query is a transient refusal, and every surface has to say so.
		// Classifying it in ONE place, rather than at the HTTP route that
		// happened to need it, is what stops /api/op/run_sql, MCP, the in-process
		// tool and the CLI from telling an agent its SQL was wrong during an
		// outage. A budget refusal (rows/bytes) and the engine's own SQL error
		// stay unclassified on purpose: those the author fixes.
		errors.Is(err, storage.ErrSandboxUnavailable):
		return &opcore.OpError{Kind: opcore.ErrRetryable, Message: err.Error(), Err: err}
	case storage.IsEngineResourceError(err):
		// The trusted engine ran out of its own budget (memory_limit, temp
		// size). The raw message leaks engine internals — allocation sizes,
		// tuning advice — that mean nothing to a reader, so the surface gets a
		// clean retryable refusal while the detail stays in Err for logs.
		return &opcore.OpError{Kind: opcore.ErrRetryable, Message: "the analytics engine hit its resource limit — retry in a moment", Err: err}
	default:
		return err
	}
}
