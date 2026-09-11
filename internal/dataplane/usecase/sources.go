package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Source lifecycle operations (redesign slice 2): test/preview probes,
// create/update, pause, run, status, cancel — one contract shared by MCP,
// REST /api/op, and in-process agents. Approved credential model: write-only
// reusable credential IDs — operations take a credential_id, never secret
// material; secrets enter only through the session-only credential routes.
//
// Authorization is the operation's Access class + MinSessionRole; the store
// methods here are project-scoped because the principal already proved
// project membership. DSN material never leaves the process — probe results
// carry ok/error/table metadata only.

// SourceRunner is the engine surface the run/cancel operations need. The
// connector engine implements it; tests can fake it.
type SourceRunner interface {
	EnqueueRun(ctx context.Context, projectID, syncID, idemKey string) (run connector.Run, enqueued bool, err error)
	CancelRun(runID string)
}

// runnerFrom recovers the engine from deps; nil means this process has no
// engine (tests) and run/cancel report unavailable rather than silently
// enqueueing work nothing executes.
func runnerFrom(d *Deps) (SourceRunner, error) {
	if d.Runner == nil {
		return nil, fmt.Errorf("source runs are unavailable in this process")
	}
	return d.Runner, nil
}

// probeTimeout bounds one test/preview end to end — dial plus the probe
// query — so a source that connects but hangs cannot pin the call.
const sourceProbeTimeout = 15 * time.Second

// openProjectSource resolves + decrypts the connector DSN and opens it. The
// DSN is a local variable only — never serialized, logged, or returned.
func openProjectSource(ctx context.Context, d *Deps, projectID, connectorID string) (connector.Source, error) {
	kind, dsn, err := d.Repo.ConnectorDSNForProject(ctx, projectID, connectorID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("connector not found")
		}
		return nil, err
	}
	return connector.Open(ctx, kind, dsn)
}

// --- test_source ---

type testSourceInput struct {
	ConnectorID string `json:"connector_id" required:"true" desc:"connector to probe"`
}

type testSourceOutput struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func testSource() opcore.Operation[testSourceInput, testSourceOutput] {
	return opcore.Operation[testSourceInput, testSourceOutput]{
		Name:           "test_source",
		Summary:        "Test a data source connection: dial it and report ok/error. Never returns credentials.",
		Access:         opcore.AccessSourcesRead,
		Scope:          "data_quality",
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in testSourceInput) (testSourceOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return testSourceOutput{}, err
			}
			probeCtx, cancel := context.WithTimeout(ctx, sourceProbeTimeout)
			defer cancel()
			source, err := openProjectSource(probeCtx, d, cc.ProjectID, in.ConnectorID)
			if err != nil {
				return testSourceOutput{OK: false, Error: err.Error()}, nil
			}
			defer source.Close()
			if err := source.TestConnection(probeCtx); err != nil {
				return testSourceOutput{OK: false, Error: err.Error()}, nil
			}
			return testSourceOutput{OK: true}, nil
		},
	}
}

// --- preview_source ---

const previewRowLimit = 25

// previewByteCap bounds the serialized payload: a few large text/JSON/bytea
// values must not turn a bounded probe into an unbounded MCP/HTTP response.
const previewByteCap = 256 << 10

type previewSourceInput struct {
	ConnectorID  string `json:"connector_id" required:"true" desc:"connector to preview"`
	SourceTable  string `json:"source_table" required:"true" desc:"table to preview"`
	KeyColumn    string `json:"key_column" required:"true" desc:"key/ordering column"`
	CursorColumn string `json:"cursor_column" desc:"incremental cursor column (optional — snapshot preview when empty)"`
}

type previewSourceOutput struct {
	OK       bool             `json:"ok"`
	Columns  []string         `json:"columns,omitempty"`
	Rows     []map[string]any `json:"rows,omitempty"`
	Warnings []string         `json:"warnings"`
	Error    string           `json:"error,omitempty"`
}

func previewSource() opcore.Operation[previewSourceInput, previewSourceOutput] {
	return opcore.Operation[previewSourceInput, previewSourceOutput]{
		Name:           "preview_source",
		Summary:        "Preview rows a sync would pull: validates table/key/cursor against the discovered schema and returns a bounded sample. Never returns credentials.",
		Access:         opcore.AccessSourcesRead,
		Scope:          "data_quality",
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in previewSourceInput) (previewSourceOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return previewSourceOutput{}, err
			}
			probeCtx, cancel := context.WithTimeout(ctx, sourceProbeTimeout)
			defer cancel()
			source, err := openProjectSource(probeCtx, d, cc.ProjectID, in.ConnectorID)
			if err != nil {
				return previewSourceOutput{OK: false, Error: err.Error(), Warnings: []string{}}, nil
			}
			defer source.Close()

			out := previewSourceOutput{Warnings: []string{}}
			// Validate the requested identifiers against the discovered schema
			// before pulling — a typo'd table/column gets a clear error, and
			// nothing caller-supplied reaches the source unverified.
			tables, err := source.DiscoverSchema(probeCtx)
			if err != nil {
				out.Error = err.Error()
				return out, nil
			}
			var table *connector.Table
			for i := range tables {
				if tables[i].Name == in.SourceTable {
					table = &tables[i]
					break
				}
			}
			if table == nil {
				out.Error = fmt.Sprintf("table %q not found on the source", in.SourceTable)
				return out, nil
			}
			hasCol := func(name string) bool {
				for _, c := range table.Columns {
					if c.Name == name {
						return true
					}
				}
				return false
			}
			if !hasCol(in.KeyColumn) {
				out.Error = fmt.Sprintf("key column %q not found on %q", in.KeyColumn, in.SourceTable)
				return out, nil
			}
			if in.CursorColumn != "" && !hasCol(in.CursorColumn) {
				out.Error = fmt.Sprintf("cursor column %q not found on %q", in.CursorColumn, in.SourceTable)
				return out, nil
			}
			for _, c := range table.Columns {
				out.Columns = append(out.Columns, c.Name)
			}

			cursorColumn := in.CursorColumn
			if cursorColumn == "" {
				cursorColumn = in.KeyColumn
				out.Warnings = append(out.Warnings, "snapshot mode: no cursor column — a real sync re-pulls the whole table each run")
			}
			pull, err := source.PullRows(probeCtx, connector.PullRequest{
				Table:        in.SourceTable,
				KeyColumn:    in.KeyColumn,
				CursorColumn: cursorColumn,
				Limit:        previewRowLimit,
			})
			if err != nil {
				out.Error = err.Error()
				return out, nil
			}
			encoded := 0
			for _, r := range pull.Rows {
				rowBytes, _ := json.Marshal(r.Data)
				if encoded+len(rowBytes) > previewByteCap {
					out.Warnings = append(out.Warnings, fmt.Sprintf("preview truncated at %d bytes — %d of %d pulled rows shown", previewByteCap, len(out.Rows), len(pull.Rows)))
					break
				}
				encoded += len(rowBytes)
				out.Rows = append(out.Rows, r.Data)
			}
			if pull.HasMore {
				out.Warnings = append(out.Warnings, fmt.Sprintf("showing first %d rows", previewRowLimit))
			}
			out.OK = true
			return out, nil
		},
	}
}

// --- pause_source ---

type pauseSourceInput struct {
	SyncID         string `json:"sync_id" required:"true" desc:"sync to pause or resume"`
	Paused         bool   `json:"paused" required:"true" desc:"true pauses, false resumes"`
	Revision       int64  `json:"revision" required:"true" desc:"expected current revision — stale revisions conflict"`
	IdempotencyKey string `json:"idempotency_key" desc:"retry key — a repeated identical request returns the first result"`
}

func pauseSource() opcore.Operation[pauseSourceInput, storage.ConnectorSync] {
	return opcore.Operation[pauseSourceInput, storage.ConnectorSync]{
		Name:           "pause_source",
		Summary:        "Pause or resume a source sync (reversible). Pausing stops future runs; an active run keeps going — cancel it explicitly. Requires the current revision.",
		Access:         opcore.AccessSourcesManage,
		Scope:          "",
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in pauseSourceInput) (storage.ConnectorSync, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.ConnectorSync{}, err
			}
			if in.Revision <= 0 {
				return storage.ConnectorSync{}, fmt.Errorf("revision must be the sync's current revision (> 0)")
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.ConnectorSync{}, err
			}
			return d.Repo.SetConnectorSyncEnabledIdempotent(ctx, cc.ProjectID, in.SyncID, !in.Paused, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}

// --- run_source ---

type runSourceInput struct {
	SyncID         string `json:"sync_id" required:"true" desc:"sync to run now"`
	IdempotencyKey string `json:"idempotency_key" desc:"retry key — a repeated request returns the same run"`
}

type runSourceOutput struct {
	Run      connector.Run `json:"run"`
	Enqueued bool          `json:"enqueued"` // false = an active run already exists; run is that one
}

func runSource() opcore.Operation[runSourceInput, runSourceOutput] {
	return opcore.Operation[runSourceInput, runSourceOutput]{
		Name:           "run_source",
		Summary:        "Start a source sync now. Returns a persistent run id for source_status/cancel_source_run. At most one active run per sync; a repeated idempotency key returns the same run.",
		Access:         opcore.AccessSourcesManage,
		Scope:          "",
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in runSourceInput) (runSourceOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return runSourceOutput{}, err
			}
			runner, err := runnerFrom(d)
			if err != nil {
				return runSourceOutput{}, err
			}
			run, enqueued, err := runner.EnqueueRun(ctx, cc.ProjectID, in.SyncID, strings.TrimSpace(in.IdempotencyKey))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return runSourceOutput{}, fmt.Errorf("sync not found")
				}
				if errors.Is(err, storage.ErrSyncPaused) {
					return runSourceOutput{}, fmt.Errorf("sync is paused — resume it before running")
				}
				return runSourceOutput{}, err
			}
			return runSourceOutput{Run: run, Enqueued: enqueued}, nil
		},
	}
}

// --- source_status ---

type sourceStatusInput struct {
	ConnectorID string `json:"connector_id" desc:"list this connector's syncs with their latest run"`
	SyncID      string `json:"sync_id" desc:"one sync's status"`
	RunID       string `json:"run_id" desc:"one run's status"`
}

type syncStatus struct {
	Sync      storage.ConnectorSync `json:"sync"`
	LatestRun *connector.Run        `json:"latest_run,omitempty"`
}

type sourceStatusOutput struct {
	Syncs []syncStatus   `json:"syncs,omitempty"`
	Run   *connector.Run `json:"run,omitempty"`
}

func sourceStatus() opcore.Operation[sourceStatusInput, sourceStatusOutput] {
	return opcore.Operation[sourceStatusInput, sourceStatusOutput]{
		Name:    "source_status",
		Summary: "Read source status: one run by id, one sync's latest run, or every sync on a connector. Project-scoped; unknown ids are not-found.",
		Access:  opcore.AccessSourcesRead,
		Scope:   "monitor",
		Handler: func(ctx context.Context, cc opcore.CallContext, in sourceStatusInput) (sourceStatusOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return sourceStatusOutput{}, err
			}
			out := sourceStatusOutput{}
			switch {
			case strings.TrimSpace(in.RunID) != "":
				run, err := d.Repo.ConnectorRunForProject(ctx, cc.ProjectID, in.RunID)
				if errors.Is(err, pgx.ErrNoRows) {
					return sourceStatusOutput{}, fmt.Errorf("run not found")
				}
				if err != nil {
					return sourceStatusOutput{}, err
				}
				out.Run = &run
				return out, nil
			case strings.TrimSpace(in.SyncID) != "":
				sync, err := d.Repo.ConnectorSyncForProject(ctx, cc.ProjectID, in.SyncID)
				if errors.Is(err, pgx.ErrNoRows) {
					return sourceStatusOutput{}, fmt.Errorf("sync not found")
				}
				if err != nil {
					return sourceStatusOutput{}, err
				}
				entry := syncStatus{Sync: sync}
				if run, rerr := d.Repo.LatestConnectorRun(ctx, cc.ProjectID, in.SyncID); rerr == nil {
					entry.LatestRun = &run
				}
				out.Syncs = []syncStatus{entry}
				return out, nil
			case strings.TrimSpace(in.ConnectorID) != "":
				syncs, err := d.Repo.ListConnectorSyncsForProject(ctx, cc.ProjectID, in.ConnectorID)
				if err != nil {
					return sourceStatusOutput{}, err
				}
				out.Syncs = []syncStatus{}
				for _, sync := range syncs {
					entry := syncStatus{Sync: sync}
					if run, rerr := d.Repo.LatestConnectorRun(ctx, cc.ProjectID, sync.ID); rerr == nil {
						entry.LatestRun = &run
					}
					out.Syncs = append(out.Syncs, entry)
				}
				return out, nil
			default:
				return sourceStatusOutput{}, fmt.Errorf("one of run_id, sync_id, or connector_id is required")
			}
		},
	}
}

// --- cancel_source_run ---

// --- create_source ---

type createSourceInput struct {
	Name           string `json:"name" required:"true" desc:"source name"`
	Kind           string `json:"kind" required:"true" desc:"connector kind, e.g. postgres"`
	CredentialID   string `json:"credential_id" required:"true" desc:"a live source credential of this project — never the secret itself"`
	IdempotencyKey string `json:"idempotency_key" desc:"retry key — a repeated identical request returns the first result"`
}

func createSource() opcore.Operation[createSourceInput, storage.DataConnector] {
	return opcore.Operation[createSourceInput, storage.DataConnector]{
		Name:           "create_source",
		Summary:        "Create a data source referencing a stored credential by ID. The credential's secret is never accepted or returned here — store it first via the session credential endpoint.",
		Access:         opcore.AccessSourcesManage,
		Scope:          "analyze_build",
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in createSourceInput) (storage.DataConnector, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.DataConnector{}, err
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.DataConnector{}, err
			}
			return d.Repo.CreateDataConnectorIdempotent(ctx, cc.ProjectID, in.Name, in.Kind, strings.TrimSpace(in.CredentialID), strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}

// --- update_source ---

type updateSourceInput struct {
	ConnectorID    string  `json:"connector_id" required:"true" desc:"source to update"`
	Name           *string `json:"name" desc:"new name — omit to keep current"`
	CredentialID   *string `json:"credential_id" desc:"rotate to this credential ID — omit to keep current"`
	Revision       int64   `json:"revision" required:"true" desc:"expected current revision — stale revisions conflict"`
	IdempotencyKey string  `json:"idempotency_key" desc:"retry key — a repeated identical request returns the first result"`
}

func updateSource() opcore.Operation[updateSourceInput, storage.DataConnector] {
	return opcore.Operation[updateSourceInput, storage.DataConnector]{
		Name:           "update_source",
		Summary:        "Update a source's name or rotate its credential reference. Requires the current revision; omitting credential_id preserves the existing credential.",
		Access:         opcore.AccessSourcesManage,
		Scope:          "analyze_build",
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in updateSourceInput) (storage.DataConnector, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.DataConnector{}, err
			}
			if in.Revision <= 0 {
				return storage.DataConnector{}, fmt.Errorf("revision must be the source's current revision (> 0)")
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.DataConnector{}, err
			}
			return d.Repo.UpdateDataConnectorIdempotent(ctx, cc.ProjectID, in.ConnectorID, in.Name, in.CredentialID, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
		},
	}
}

type cancelSourceRunInput struct {
	RunID string `json:"run_id" required:"true" desc:"run to cancel — queued runs end immediately, running runs stop at the next batch boundary"`
}

func cancelSourceRun() opcore.Operation[cancelSourceRunInput, connector.Run] {
	return opcore.Operation[cancelSourceRunInput, connector.Run]{
		Name:           "cancel_source_run",
		Summary:        "Cancel a queued or running source run. Idempotent; a finished run returns its terminal state unchanged.",
		Access:         opcore.AccessSourcesManage,
		Scope:          "",
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in cancelSourceRunInput) (connector.Run, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return connector.Run{}, err
			}
			runner, err := runnerFrom(d)
			if err != nil {
				return connector.Run{}, err
			}
			run, err := d.Repo.CancelConnectorRun(ctx, cc.ProjectID, in.RunID)
			if errors.Is(err, pgx.ErrNoRows) {
				return connector.Run{}, fmt.Errorf("run not found")
			}
			if err != nil {
				return connector.Run{}, err
			}
			// The DB flag is the contract; the in-process cancel makes a live
			// local worker stop promptly instead of at the next poll.
			runner.CancelRun(run.ID)
			return run, nil
		},
	}
}
